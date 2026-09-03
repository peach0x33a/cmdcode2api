# cmdcode2api

`cmdcode2api` is a local, OpenAI-compatible gateway for [Command Code](https://commandcode.ai/).
It exposes familiar endpoints such as `POST /v1/chat/completions`,
`POST /v1/responses`, and `GET /v1/models`, and forwards the traffic to Command
Code using one or more of your own accounts.

It was originally named `cc-gateway`, renamed to avoid colliding with Claude
Code's common `cc` abbreviation.

## What it's for

Any tool that speaks the OpenAI API can talk to Command Code through this
gateway without knowing anything about Command Code's own protocol. Typical
uses:

- Point an OpenAI-compatible client (Codex CLI, editor plugins, scripts, local
  chat UIs) at `http://localhost:11434/v1` and use Command Code models.
- Spread load across several Command Code accounts. Requests round-robin across
  every healthy account, with automatic failover when a key goes stale.
- Run it on a home server or VPS and reach it over LAN or
  [Tailscale](https://tailscale.com/).
- Watch usage and remaining credits per account, with optional Discord alerts
  when a window is close to its cap or a subscription is about to lapse.

It is a personal utility. It targets the Command Code API shape observed during
development; if Command Code changes its internal API, the adapter may need
updating.

## Features

- OpenAI-compatible HTTP API: `POST /v1/chat/completions`, `POST /v1/responses`
  (Responses API, for Codex CLI), `GET /v1/models`
- Streaming and non-streaming chat completions
- Base64 `data:` `image_url` content converted to Command Code / Anthropic-style
  image blocks
- Multi-account rotation with automatic failover and background staleness
  detection
- Browser OAuth helper for obtaining Command Code API keys per named account
- Local bearer-token auth for clients; CORS enabled for local UI clients
- Web UI at `/ui` for adding accounts, enabling models, and copying client
  config
- Per-account billing/credit polling (five-hour, weekly, monthly windows),
  shown in the UI and at `GET /admin/billing`
- Optional Discord alerts for usage thresholds and expiring subscriptions or
  billing sessions
- Usage counters persisted to `usage.json`, exposed at `GET /usage` and
  `GET /admin/usage`

## Installation

### Headless install (Linux, systemd)

Downloads a prebuilt release, creates a service user, and installs and starts a
systemd unit. No Go toolchain needed.

```bash
curl -fsSL https://raw.githubusercontent.com/FlightlessWeasel/cmdcode2api/master/scripts/install.sh | bash
```

Update an existing install to the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/FlightlessWeasel/cmdcode2api/master/scripts/install.sh | bash -s -- --update
```

Run `install.sh --help` for options (`--version`, `--dir`, `--service`,
`--user`, `--host`, `--os-upgrade`).

### Prebuilt binaries

Grab an archive for your platform from the
[releases page](https://github.com/FlightlessWeasel/cmdcode2api/releases),
extract it, and put `cmdcode2api` on your `PATH`. Builds are published for
Linux, macOS, and Windows (amd64 and arm64, except Windows arm64), plus a
`.deb`.

### Build from source

Requires Go 1.25+. The web UI build output is committed and embedded, so Node is
not needed for a plain build.

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
```

## Quick start

1. Run the binary once to generate `config.yaml`:

   ```bash
   ./cmdcode2api
   ```

2. Start the gateway and open the web UI:

   ```bash
   ./cmdcode2api
   # then open http://localhost:11434/ui
   ```

3. In the UI:
   - **Accounts** — **Add account** runs the Command Code OAuth flow and writes
     the key into `config.yaml`. **Reauthorize** refreshes an account that has
     gone stale.
   - **Models** — every model starts disabled; enable the ones your plan serves.
   - **Alerts** — configure Discord alerts (optional).
   - **Setup** — copy the base URL and API key, with per-client config snippets.

4. Point your OpenAI-compatible client at `http://localhost:11434/v1` and use
   the generated `api_key` from `config.yaml` as the bearer token.

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-pro",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'
```

Opened from the same machine, the UI needs no key. From another device it
prompts for the gateway's `api_key`.

## Documentation

Everything past the quick start lives in the
[wiki](https://github.com/FlightlessWeasel/cmdcode2api/wiki):

- **[CLI Reference](https://github.com/FlightlessWeasel/cmdcode2api/wiki/CLI-Reference)**
  — every flag and command-line account operation
- **[Configuration](https://github.com/FlightlessWeasel/cmdcode2api/wiki/Configuration)**
  — every `config.yaml` field
- **[HTTP API](https://github.com/FlightlessWeasel/cmdcode2api/wiki/HTTP-API)**
  — all endpoints, request shapes, and auth rules
- **[Accounts, Rotation & Failover](https://github.com/FlightlessWeasel/cmdcode2api/wiki/Accounts-Rotation-and-Failover)**
- **[Remote & Headless OAuth](https://github.com/FlightlessWeasel/cmdcode2api/wiki/Remote-and-Headless-OAuth)**
  — the SSH port-forward flow for a server with no browser
- **[Billing Session Token](https://github.com/FlightlessWeasel/cmdcode2api/wiki/Billing-Session-Token)**
- **[Discord Alerts](https://github.com/FlightlessWeasel/cmdcode2api/wiki/Discord-Alerts)**

## Project layout

```text
cmd/cmdcode2api/          CLI entrypoint (thin main)
internal/app/             gateway implementation
internal/app/webui-src/   TypeScript + React admin UI source (Vite)
internal/app/webui/dist/  committed UI build output, embedded via //go:embed
scripts/                  install.sh (headless installer) and deploy.sh
```

Coding conventions for contributors and reviewers are in
[CODING_STANDARDS.md](CODING_STANDARDS.md).

## Web UI development

The UI is a TypeScript + React app built with Vite. Its build output is
committed to `internal/app/webui/dist/` and embedded into the Go binary, so
`go build` never needs Node. After editing the UI, rebuild and commit the
output in the same change:

```bash
cd internal/app/webui-src
npm install
npm run build
```

## Files intentionally not committed

Runtime and secret artifacts are gitignored: the built binary, `config.yaml`,
`usage.json`, `discord-alerts.json`, `*.exe`, `.oauth_state`, `.oauth_url`, and
debug dumps (`log.txt*`, `*.log`, `*.patch`) — `--debug` writes full request
bodies and SSE events, so those can contain prompts and source. Local
agent/tooling state (`.claude/`, `.serena/`, `.sisyphus/`) is excluded too.
