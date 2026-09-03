# HTTP API

Base URL for OpenAI-compatible clients:

```text
http://localhost:11434/v1
```

Use the generated `api_key` from `config.yaml` as the bearer token.

## Auth model

| Route group | Auth |
| --- | --- |
| `/health`, `/usage`, `/favicon.ico`, CORS preflight | none |
| `/v1/*` (LLM proxy) | bearer `api_key`, always |
| `/accounts`, `/accounts/*` | bearer `api_key` |
| `/ui`, `/admin/*` | bearer `api_key`, **or** a genuine same-machine (loopback) browser request, **or** any caller when `ui_no_auth` is set |

The loopback bypass for the admin surface requires a same-machine TCP peer, a
`Host` header of `localhost` / `127.0.0.1` / `[::1]`, and — if present — an
`Origin` that matches. This is what stops a remote page from reaching it via DNS
rebinding. A LAN or Tailscale caller does not get the bypass; it needs the
bearer token like any other remote caller.

CORS: `/v1/*` and other non-admin routes get `Access-Control-Allow-Origin: *`.
The admin surface never gets a wildcard — only a `Vary: Origin` header — so a
website in another tab can't read the account list or trigger a delete.

## LLM proxy

### `POST /v1/chat/completions`

OpenAI-style chat completions, forwarded to Command Code. Supported request
styles:

- Plain text messages
- Multimodal content arrays with base64 `data:` URLs in `image_url`
- `stream: true` server-sent events
- `stream: false` JSON response

A request for a disabled model returns `404` with the OpenAI-compatible error
JSON shape.

When Command Code reports `inputTokenDetails.cacheReadTokens`, it is surfaced as
`usage.prompt_tokens_details.cached_tokens`. OpenAI's usage schema has no
cache-write field, so `cacheWriteTokens` appears only in `/usage` and
`/admin/usage`.

Remote HTTP(S) image URLs are rejected with `400 invalid_request_error`. Image
content must be a base64 `data:image/...;base64,...` URL.

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-pro",
    "messages": [{"role": "user", "content": "Write a short haiku."}],
    "stream": true
  }'
```

### `POST /v1/responses`

OpenAI's Responses API protocol, for clients that speak it instead of Chat
Completions (notably Codex CLI). Same bearer auth as
`/v1/chat/completions`, adapted onto the same Command Code dispatch pipeline —
it shares the enabled-model gate, account rotation, and failover.

The gateway is stateless: `previous_response_id` is not supported, because there
is no server-side conversation store. Send the full conversation in `input` on
every request.

```bash
curl http://localhost:11434/v1/responses \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-pro",
    "input": "Hello!",
    "stream": false
  }'
```

### `GET /v1/models`

Returns only the models enabled via `model_overrides` — an empty list until you
enable at least one. Each entry includes `context_window` when Command Code
reports one.

## Status and usage

### `GET /health`

No auth.

```json
{"status":"ok","version":"v1.2.3"}
```

### `GET /usage`

No auth. Locally accumulated usage counters, persisted to `usage.json` (git
ignored). Global totals only — never broken down by account.

```json
{
  "total_requests": 1,
  "prompt_tokens": 7527,
  "completion_tokens": 55,
  "cache_read_tokens": 7424,
  "cache_write_tokens": 0
}
```

### `GET /admin/usage`

Loopback bypass or bearer token (see [Auth model](#auth-model)). The same global
totals as `/usage` plus a per-account breakdown:

```json
{
  "total_requests": 12,
  "prompt_tokens": 41200,
  "completion_tokens": 980,
  "cache_read_tokens": 39000,
  "cache_write_tokens": 0,
  "accounts": [
    {"account": "personal", "total_requests": 7, "prompt_tokens": 25000, "completion_tokens": 600, "cache_read_tokens": 24000, "cache_write_tokens": 0},
    {"account": "work", "total_requests": 5, "prompt_tokens": 16200, "completion_tokens": 380, "cache_read_tokens": 15000, "cache_write_tokens": 0}
  ]
}
```

```bash
# From the machine hosting the gateway, no Authorization header needed.
curl http://localhost:11434/admin/usage
# From anywhere else:
curl http://<host>:11434/admin/usage -H "Authorization: Bearer <api_key>"
```

### `GET /admin/billing`

Same gate as `/admin/usage`. One row per account that has a `session_token`,
each with the current five-hour, weekly, and monthly credit windows polled from
Command Code, plus the subscription and session expiry the Discord alerts
watch. Accounts without a session token are omitted. This is the data behind the
UI's credit bar.

## Accounts

### `GET /accounts`

Requires the bearer token (unlike `/health` and `/usage`). Each configured
account's name and live status, never the API key:

```json
[
  {"name": "personal", "status": "healthy", "last_checked": "2026-08-24T10:00:00Z"},
  {"name": "work", "status": "stale", "last_checked": "2026-08-24T09:50:00Z", "last_error": "http 401"}
]
```

`status` is `unknown` (not probed yet), `healthy`, `stale` (key no longer
authenticates — reauthorize), or `limited` (key authenticates but hit a
rate-limit / quota error — reauthorizing would not help).

## Admin surface (web UI backend)

These back the `/ui` tabs and follow the admin auth model. They are not a
stable public API.

| Route | Purpose |
| --- | --- |
| `POST /accounts/reauth` | Run the OAuth flow for an existing account. |
| `POST /accounts/delete` | Remove an account. |
| `POST /accounts/probe` | Force an immediate health probe. |
| `POST /accounts/billing-token` | Set or clear an account's billing session token. |
| `GET /admin/models`, `POST /admin/models/toggle` | Read and change model policy. |
| `GET /admin/connection` | Per-client connection config shown on the Setup tab. |
| `GET /admin/alerts` | Discord alert settings (webhook URL is write-only, never returned). |
| `GET /admin/monitor` | Server-sent stream of live model-API calls (Monitoring tab). |
