# cmdcode2api

`cmdcode2api` is a small OpenAI-compatible gateway for [Command Code](https://commandcode.ai/). It lets OpenAI-style clients call Command Code models through familiar endpoints such as `/v1/chat/completions` and `/v1/models`.

The project was originally named `cc-gateway`; it was renamed to avoid confusion with Claude Code's common `cc` abbreviation.

## Features

- OpenAI-compatible HTTP API
  - `POST /v1/chat/completions`
  - `GET /v1/models`
- Streaming and non-streaming chat completions
- OpenAI `image_url` input conversion to Command Code / Anthropic-style image blocks
- Browser OAuth helper for obtaining a Command Code API key
- Local bearer-token auth for clients
- CORS enabled for local UI clients
- Usage counter persisted to `usage.json`
- Health endpoint: `GET /health`
- Usage endpoint: `GET /usage`

## Build

```bash
go build -o cmdcode2api .
```

## First run

Run the binary once to generate `config.yaml`:

```bash
./cmdcode2api
```

Then complete Command Code OAuth:

```bash
./cmdcode2api --oauth
```

The OAuth flow writes the Command Code API key into `config.yaml`.

## Configuration

`config.yaml` is created automatically and intentionally ignored by git.

Example shape:

```yaml
api_key: ccgw-generated-local-client-key
commandcode:
  api_key: your-command-code-api-key
  base_url: https://api.commandcode.ai
port: 11434
```

Fields:

- `api_key` — local bearer token required by clients calling this gateway.
- `commandcode.api_key` — Command Code API key obtained via `--oauth`.
- `commandcode.base_url` — Command Code API base URL.
- `port` — local listen port. Defaults to `11434`.

## Run

```bash
./cmdcode2api
```

The server listens on:

```text
http://localhost:11434
```

## Use with OpenAI-compatible clients

Set the base URL to your local gateway:

```text
http://localhost:11434/v1
```

Use the generated `api_key` from `config.yaml` as the bearer token.

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

No authentication required. Returns locally accumulated usage counters:

```json
{
  "total_requests": 1,
  "prompt_tokens": 7527,
  "completion_tokens": 55,
  "cache_read_tokens": 7424,
  "cache_write_tokens": 0
}
```

Usage is persisted to `usage.json`, which is ignored by git.

### `GET /v1/models`

Returns the built-in model list.

### `POST /v1/chat/completions`

Accepts OpenAI-style chat completion requests and forwards them to Command Code.

Supported request styles:

- Plain text messages
- Multimodal content arrays with `image_url`
- `stream: true` server-sent events
- `stream: false` JSON response

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
