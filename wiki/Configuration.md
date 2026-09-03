# Configuration

`config.yaml` is created automatically on first run and is intentionally
ignored by git. It is written with `0600` permissions and is the single source
of truth for accounts, model policy, and alert settings. While the server is
running, all writes to it go through one writer, so the web UI and the reauth
flow never race.

## Example

```yaml
api_key: ccgw-generated-local-client-key
accounts:
  - name: personal
    api_key: your-command-code-api-key
    base_url: https://api.commandcode.ai
host: localhost
port: 11434
allow_lan: false
allow_tailscale: false
model_overrides:
  deepseek/deepseek-v4-pro: true
```

## Top-level fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `api_key` | string | generated | Local bearer token that clients must send to reach this gateway. Generated as `ccgw-<48 hex chars>` on first run. |
| `accounts` | list | `[]` | Command Code accounts. See [account fields](#account-fields) below. |
| `host` | string | `localhost` | HTTP listen host. `0.0.0.0` listens on all interfaces. |
| `port` | int | `11434` | HTTP listen port. |
| `allow_lan` | bool | `false` | Let other devices on the local network reach the gateway. Detects the LAN IP at startup and logs it. If `host` is still `localhost`, enabling this switches it to `0.0.0.0`. |
| `allow_tailscale` | bool | `false` | Same, for a device reachable over [Tailscale](https://tailscale.com/) if it's installed and connected. Detects the Tailscale IP at startup. |
| `ui_no_auth` | bool | `false` | Serve `/ui` and the account-management API (`/accounts*`, `/admin/*`) with no authentication. `/v1/*` still requires `api_key`. Trusted networks only — it exposes account add/remove, OAuth, model policy, and billing to anyone who can reach the port. Also settable per-run with `--ui-no-auth`. |
| `model_overrides` | map[string]bool | — | Per-model enabled/disabled override, keyed by exact model ID. The only way to enable a model. See [Enabling models](#enabling-models). |
| `family_overrides` | map[string]bool | — | Per-family override, keyed by family (e.g. `deepseek/deepseek-v4`). Set from the UI's Models tab, which expands the family into exact `model_overrides` entries and applies to models added to that family later. |
| `exclude_models` | list | — | Legacy prefix-blocklist from before every model was disabled by default. Still decoded so old config files load, but no longer consulted. |
| `discord_webhook_url` | string | — | Legacy top-level webhook URL. Prefer `discord_alerts.webhook_url`. Never logged. |
| `discord_alert_state_file` | string | `discord-alerts.json` | Legacy top-level dedup state path. Prefer `discord_alerts.state_file`. |
| `discord_alerts` | object | — | Discord alert settings. See [Discord Alerts](Discord-Alerts). |

`allow_lan` / `allow_tailscale` only make the port reachable — they do not
weaken authentication. The web UI's tokenless convenience login only applies to
a genuine same-machine (loopback) request; from a LAN or Tailscale device you
still need `api_key`, which the UI prompts for the first time you open it from
that device.

`ui_no_auth: true` (or `--ui-no-auth`) removes that requirement for the admin
surface only. It does not touch the `/v1/*` proxy routes. Use it only on a
trusted network. The startup log prints `WARNING: ui_no_auth is set` while it is
active.

## Account fields

Each entry in `accounts`:

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | Account name. Used with `--oauth --account <name>` and in status output. |
| `api_key` | string | The Command Code API key, obtained via the OAuth flow. A secret — never returned by any JSON endpoint. |
| `base_url` | string | Command Code API base URL. Defaults to `https://api.commandcode.ai` when omitted. |
| `email` | string | Whatever identity string the OAuth callback returned (userName, falling back to keyName). Not a validated address; kept so a human can tell which login the account belongs to. |
| `user_id` | string | Raw `userID` from the OAuth callback. Kept for debugging. |
| `key_name` | string | Raw `keyName` from the OAuth callback. Kept for debugging. |
| `session_token` | string | The `__Secure-commandcode_prod_.session_token` browser cookie, used to authenticate billing API calls. A secret, exactly like `api_key`. Enables credit polling and threshold alerts. See [Billing Session Token](Billing-Session-Token). |
| `session_email` | string | Which commandcode.ai login the session token belongs to, from `/auth/get-session`. Metadata, not a credential. |
| `session_expires_at` | timestamp | When the billing session expires. Drives the 24-hour session-expiry alert. |
| `plan_expires_at` | timestamp | The subscription's `currentPeriodEnd`, cached so it stays available after the session token expires. Refreshed on every successful billing fetch. |

The billing-session group (`session_token`, `session_email`,
`session_expires_at`, `plan_expires_at`) is carried across an OAuth reauth,
which otherwise rebuilds the account with those fields blank.

## Enabling models

Every model starts disabled. A fresh `config.yaml` has no `model_overrides` at
all, and `GET /v1/models` returns an empty list until you enable something.
Enable the models your plan serves from the UI's Models tab at
`http://localhost:11434/ui#models`, which persists your choices to
`model_overrides`. See
[commandcode.ai/docs/plans/go#models](https://commandcode.ai/docs/plans/go#models)
for what's included on the Go plan.

A request for a disabled model returns `404` with the OpenAI-compatible error
JSON shape.

## Legacy config migration

A `config.yaml` from before multi-account support carried a single
`commandcode: {api_key, base_url}` block. It is migrated automatically into an
`accounts` list with one account named `default` the next time it loads, and
persisted immediately so the migration runs once.

Configurations written before the explicit `discord_alerts.enabled` switch used
a non-empty webhook URL as the enabled signal. That is preserved: a webhook URL
with no explicit `enabled` still counts as enabled, while an explicit
`enabled: false` survives a restart.
