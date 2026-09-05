# cmdcode2api

[中文说明](README.zh-CN.md)

`cmdcode2api` is a small OpenAI-compatible gateway for [Command Code](https://commandcode.ai/). It lets OpenAI-style clients call Command Code models through familiar endpoints such as `/v1/chat/completions` and `/v1/models`.

The project was originally named `cc-gateway`; it was renamed to avoid confusion with Claude Code's common `cc` abbreviation.

## Features

- OpenAI-compatible HTTP API
  - `POST /v1/chat/completions`
  - `GET /v1/models`
- Streaming and non-streaming chat completions
- OpenAI base64 data `image_url` conversion to Command Code / Anthropic-style image blocks
- Multiple Command Code accounts with round-robin rotation and automatic failover (401/403/429/5xx), including per-account 429 cooldown
- Multiple local client API keys with per-key usage tracking, managed in the WebUI or `config.yaml`
- Embedded single-file WebUI: usage dashboard, account/key management, model exposure editor, live settings, and log tail
- Browser OAuth helper for obtaining a Command Code API key (CLI or from the WebUI); each OAuth run adds an account
- Local bearer-token auth for clients and a separate admin password for the WebUI
- CORS enabled for local UI clients
- Usage counters (global, per-account, and per-client-key) persisted to `usage.json`
- Health endpoint: `GET /health`
- Usage endpoint: `GET /usage`

## Build

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
```

Or with Docker — prebuilt images are published to GHCR by CI on every
master push (`latest`) and every `v*` tag:

```bash
docker run -d --name cmdcode2api -p 11434:11434 -v cmdcode2api-data:/data ghcr.io/peach0x33a/cmdcode2api:latest
```

`config.yaml` and `usage.json` live in the `/data` volume. To build locally,
use `docker build -t cmdcode2api .` — networks that cannot reach
proxy.golang.org can pass `--build-arg GOPROXY=https://goproxy.cn,direct`.
A ready-to-copy Compose file is provided as `docker-compose.example.yml`.

## Project layout

```text
cmd/cmdcode2api/   CLI entrypoint
internal/app/      gateway implementation
internal/web/      embedded single-file WebUI (index.html)
```

## First run

Run the binary once to generate `config.yaml`:

```bash
./cmdcode2api
```

The generated config contains a local client key and a WebUI admin password —
both are printed once and stored in `config.yaml`.

Then complete Command Code OAuth:

```bash
./cmdcode2api --oauth
```

The OAuth flow adds the Command Code API key as an account in `config.yaml`.
Run `--oauth` again (e.g. with a different browser profile) to add more
accounts; re-authorizing an existing key is a no-op.

On a remote server without a browser, keep the callback server bound to
`127.0.0.1` and provide the callback URL that Command Code should call:

```bash
./cmdcode2api --oauth --oauth-callback http://localhost:5959/callback
```

If your browser is on a different machine, forward that callback URL to the
server, for example:

```bash
ssh -L 5959:127.0.0.1:5959 user@server
```

## Configuration

`config.yaml` is created automatically and intentionally ignored by git.

Example shape:

```yaml
api_key: ccgw-generated-local-client-key
api_keys:
  - name: default
    key: ccgw-generated-local-client-key
  - name: my-agent
    key: ccgw-another-client-key
admin_password: kR7vBn2xQm9T
webui: true
commandcode:
  base_url: https://api.commandcode.ai
  accounts:
    - name: main
      api_key: your-command-code-api-key
      enabled: true
    - name: backup
      api_key: another-command-code-api-key
host: localhost
port: 11434
exclude_models:
  - gpt-
  - claude-
  - gemini-
```

Fields:

- `api_key` — legacy single local client key. Migrated into `api_keys` on load and cleared on save once the list is non-empty.
- `api_keys` — local bearer keys that clients use to call this gateway. Requests and token usage are tracked per key. Manage them in the WebUI (generate new keys, enable/disable, delete); changes apply immediately and persist here.
- `admin_password` — password for the WebUI admin API. Generated on first start when empty and printed once.
- `webui` — set to `false` to disable serving the embedded WebUI and admin API entirely.
- `commandcode.accounts` — list of Command Code credentials. Requests rotate across enabled accounts (see below). The legacy single-key field `commandcode.api_key` is still accepted and migrated to a one-entry list on load.
- `commandcode.base_url` — Command Code API base URL.
- `host` — HTTP listen host. Defaults to `localhost`. Use `0.0.0.0` to listen on all interfaces.
- `port` — local listen port. Defaults to `11434`.
- `exclude_models` — model ID prefixes hidden from `/v1/models` and rejected by `/v1/chat/completions`. Maintained from the WebUI's Models tab, where the upstream catalog is shown with checkboxes.

New configs exclude `gpt-`, `claude-`, and `gemini-` by default. These prefixes match both plain model IDs such as `gpt-4` and provider-qualified IDs such as `openai/gpt-4` by checking the part after the final `/`.

To make all models available, remove the entries or set an empty list:

```yaml
exclude_models: []
```

### Multi-account rotation

Every chat request is sent with the next enabled account in round-robin order.
When an account fails with `401`, `403`, `429`, or a 5xx, the request is
retried with the next account automatically:

- A `429` puts the account into cooldown for the upstream `Retry-After`
  duration (60 seconds by default); cooldown accounts are skipped until they
  recover. If every enabled account is cooling down, the client receives
  `429 rate_limit_error` with the earliest recovery time.
- `400`/`422` (bad request) and client-canceled contexts are not retried.
- Failover happens before any bytes are sent to the client; once a stream has
  started it is never replayed on another account.
- Per-account request/token counters persist in `usage.json`; error state,
  last error, and cooldown windows are runtime-only and visible in the WebUI.

## Run

```bash
./cmdcode2api
```

Print the version and Go runtime version, then exit:

```bash
./cmdcode2api --version
```

To listen on all interfaces, useful for systemd or a remote server:

```bash
./cmdcode2api --host 0.0.0.0
```

The server listens on:

```text
http://localhost:11434
```

The server starts even with zero Command Code accounts configured — the WebUI,
client keys, and settings all work, and chat requests return `503 no_accounts`
until you add an account (WebUI → Accounts, or `--oauth`). Adding the first
account from the WebUI fetches the model catalog immediately.

## Use with OpenAI-compatible clients

Set the base URL to your local gateway:

```text
http://localhost:11434/v1
```

Use any key from the `api_keys` list in `config.yaml` as the bearer token.

### curl example

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ],
    "stream": false
  }'
```

### Streaming

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "Write a short haiku."}
    ],
    "stream": true
  }'
```

## Endpoints

### `GET /health`

No authentication required.

```json
{"status":"ok"}
```

### `GET /usage`

No authentication required. Returns locally accumulated usage counters, plus
per-account counters when accounts are configured:

```json
{
  "total_requests": 1,
  "prompt_tokens": 7527,
  "completion_tokens": 55,
  "cache_read_tokens": 7424,
  "cache_write_tokens": 0,
  "accounts": {
    "a1b2c3d4": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  },
  "client_keys": {
    "k9f8e7d6c": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  }
}
```

Usage is persisted to `usage.json`, which is ignored by git.

For `/v1/chat/completions`, upstream `inputTokenDetails.cacheReadTokens` is exposed using the OpenAI-compatible response field `usage.prompt_tokens_details.cached_tokens`. `cacheWriteTokens` remains available in `/usage` and is not emitted in the Chat Completions response because OpenAI's standard usage schema has no cache-write field.

### `GET /v1/models`

Returns the model list after applying `exclude_models` filtering. Each entry
carries a `context_window` field when the upstream reports one.

### `POST /v1/chat/completions`

Accepts OpenAI-style chat completion requests and forwards them to Command Code.
Requests for excluded models return `404` with the existing OpenAI-compatible error JSON shape.

Supported request styles:

- Plain text messages
- Multimodal content arrays with base64 `data:` URLs in `image_url`
- `stream: true` server-sent events
- `stream: false` JSON response

Remote HTTP(S) image URLs are rejected with `400 invalid_request_error`; image
content must be supplied as a base64 `data:image/...;base64,...` URL.

## WebUI

With `webui` enabled (the default), the binary serves an embedded
single-file admin interface under `/webui` (the root path stays free for
the API):

```text
http://localhost:11434/webui
```

Log in with the server address and `admin_password`. The same
`internal/web/index.html` file can also be opened directly in a browser and
pointed at any running instance.

Features:

- **Overview** — version, uptime, listen address, usage counters, account/key/model summaries
- **Accounts** — add (paste a key or run OAuth), edit name/key, enable/disable, connectivity test, delete; per-account requests, tokens, errors, cooldown state, and last error. OAuth-added accounts are named after the Command Code user automatically
- **Models** — checkbox list of upstream models; checked = exposed via `/v1/models` and callable, unchecked = hidden. This is the editor for `exclude_models` and applies live
- **Keys** — create local client API keys (auto-generated or custom), enable/disable, copy, delete; per-key request and token usage
- **Settings** — edit `base_url` and `exclude_models` (live), `host`/`port`/`webui` (persisted, applied on restart), and change the admin password
- **Logs** — tail of the in-memory log ring (last 500 lines)

Changes to accounts and settings are written back to `config.yaml` immediately.

### Admin API

The UI talks to a JSON API under `/admin/api/*`, authenticated with
`Authorization: Bearer <admin_password>` (works for scripts and curl too):

```text
GET    /admin/api/overview
GET    /admin/api/accounts
POST   /admin/api/accounts             {"name": "...", "api_key": "..."}
PATCH  /admin/api/accounts/{id}        {"enabled": true}, {"name": "..."} or {"api_key": "..."}
DELETE /admin/api/accounts/{id}
POST   /admin/api/accounts/{id}/test
GET    /admin/api/models
PUT    /admin/api/models               {"exposed": ["model-id", ...]}
GET    /admin/api/keys
POST   /admin/api/keys                 {"name": "...", "key": "ccgw-... (optional)"}
PATCH  /admin/api/keys/{id}            {"enabled": true} or {"name": "..."}
DELETE /admin/api/keys/{id}            (the last remaining key cannot be deleted)
GET    /admin/api/settings
PUT    /admin/api/settings
GET    /admin/api/logs?after=SEQ
POST   /admin/api/oauth/start
GET    /admin/api/oauth/status
POST   /admin/api/oauth/cancel
```

The WebUI OAuth flow needs the browser to reach the server's
`127.0.0.1:5959-5968` callback ports (local runs work out of the box; on
remote servers use an SSH tunnel or the CLI `--oauth` mode instead).

## Files intentionally not committed

The repository ignores runtime/secrets artifacts:

```text
cmdcode2api
cc-gateway
config.yaml
usage.json
*.exe
.oauth_state
.oauth_url
```

## Notes

This is a personal utility gateway and currently targets the Command Code API shape observed during development. If Command Code changes its internal API, the adapter may need updates.
