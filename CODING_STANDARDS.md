# Coding standards & behavioral spec

Conventions for `cmdcode2api`, and the invariants a change must not break.
The `/code-review` skill reads this file as the Standards source; the
**Behavioral spec** section doubles as a checklist for the Spec axis when a
change has no dedicated issue.

The rule of thumb behind all of it: this is a small single-purpose gateway.
Keep it small, keep it boring, keep it obvious to the next reader.

## Language & dependencies

- **Go 1.25** (`go.mod`). Match it; don't bump the toolchain in an unrelated
  change.
- **Standard library only, plus `gopkg.in/yaml.v3`.** No web framework, no
  router library, no assertion library. HTTP is `net/http` + `http.ServeMux`.
  Adding a dependency is a deliberate decision that needs its own justification
  in the PR, not a drive-by.
- The web UI (`internal/app/webui-src/`) is a separate TypeScript + React + Vite
  app. Its build output in `internal/app/webui/dist/` is committed and embedded
  via `//go:embed`. Any UI source change must rebuild and commit `dist/` in the
  **same** change, or `go build` breaks for everyone else.

## Package layout

- `cmd/cmdcode2api/main.go` stays a thin shim that calls `app.Run()`. No logic
  there.
- Everything else lives in the single flat package `internal/app`. Files are
  split by concern (`accounts.go`, `billing.go`, `oauth.go`, `handler.go`,
  `server.go`, …), each with a sibling `_test.go`. New concern → new file, same
  pattern; don't grow a grab-bag file.

## Formatting & naming

- `gofmt` (tabs, standard import grouping). `go vet ./...` must be clean — it is
  the lint bar; there is no golangci-lint config to satisfy.
- Exported identifiers only when something outside the file needs them. Most of
  the package is unexported.
- Names say what the thing is. `preserveSessionFields`, `isLocalAdminRequest`,
  `defaultCommandCodeBaseURL` — descriptive over terse. A name you can't make
  honest is a design smell; fix the design.

## Comments

- Doc comments explain **why**, not what the code plainly says. See
  `preserveSessionFields`, the `authMiddleware` / `corsMiddleware` blocks, and
  the legacy-migration block in `loadConfig` for the house style: a short
  paragraph on the reason the code is shaped this way and what breaks if you
  "simplify" it.
- Security-relevant decisions (auth bypasses, CORS, DNS-rebinding defense,
  secret handling) get an explicit comment at the site. Don't remove these when
  editing nearby code.
- A struct field with non-obvious semantics is documented on the field
  (`Account`, `Config`, `DiscordAlertConfig`).

## Errors

- Wrap with context: `fmt.Errorf("parse config: %w", err)`. Keep `%w` so
  callers can still match.
- `log.Fatalf` is for startup only (`Run()` and the CLI subcommands). Anything
  running under the server returns an error or logs and continues.
- HTTP handlers emit the OpenAI-compatible error shape via
  `writeError(w, status, type, msg)` — never a bare `http.Error`. Match the
  existing `type` strings (`invalid_request_error`, `authentication_error`, …).

## HTTP

- One `http.ServeMux`, wrapped by the middleware chain in `server.go`
  (auth → CORS → logging). Add routes to `newHandlerWithPolicy`.
- Handler tests exercise `newHandlerWithPolicy` / the `handle*` funcs directly
  with `httptest`. Don't open a real listener in a test.
- When a handler or constructor needs a new parameter, keep the old signature
  as a shim that supplies a default and delegates (`newHandler` →
  `newHandlerWithPolicy`, `handleChatCompletions` →
  `handleChatCompletionsWithPolicy`). Existing callers and tests keep working.

## Configuration

- `config.yaml` is written `0600` and is the single source of truth at runtime.
  While the server runs, **all** writes go through the one `ConfigStore` writer
  — never call `saveConfig` from a second goroutine.
- A new field: add the `yaml:"…"` tag, document it on the struct **and** in
  `wiki/Configuration.md`, treat the zero value as "unset / use default", and
  make sure an old `config.yaml` without the field still loads.
- Never delete a legacy field decode path. Old configs must keep loading
  (`exclude_models`, the `commandcode:` → `accounts` migration, the implicit
  `discord_alerts.enabled` rule). Migrations run once and persist.

## Secrets

- `api_key`, account `api_key`, `session_token`, and Discord `webhook_url` are
  secrets. They must never be logged, never appear in a struct that gets
  serialized to a client, and never be returned by an endpoint. Use the
  view/response structs for JSON output; don't `json.Marshal` an `Account` or
  `Config` directly.
- `--debug` dumps full request bodies and SSE events. Its output paths
  (`log.txt*`, `*.log`, `*.patch`) stay gitignored.

## Concurrency

- Background workers (health probe, billing refresh) take a `context.Context`
  and exit on cancel. `Run()` owns the root context and cancels it when the
  server returns.
- Shared mutable state is guarded by a `sync.Mutex` / `sync.RWMutex` held for
  the smallest span that stays correct (`AccountPool`, `accountState`,
  `UsageTracker`, `BillingTracker`).

## Tests

- Table-driven where there's more than one case:
  `tests := []struct{ name string; … }` then
  `for _, tt := range tests { t.Run(tt.name, func(t *testing.T) { … }) }`.
- `t.Fatalf` for a failure that makes the rest of the case meaningless,
  `t.Errorf` to keep going. Message format `"got X, want Y"`.
- Standard library only: `testing`, `net/http/httptest`, `encoding/json`. No
  test framework.
- A bug fix comes with a test that fails before it and passes after.
- `go test ./... -count=1` must pass with no network access.

## CI gates (`.github/workflows/ci.yml`)

Every PR runs, and must pass: web UI `npm ci && npm run build`, then
`go vet ./...`, `go test ./... -count=1`, `go build ./...`. Releases are cut by
GoReleaser on a `v*` tag — don't hand-edit release plumbing without updating
`.goreleaser.yaml`.

## Git

- Branch names: `feat/…`, `fix/…`, `ci/…`, `docs/…` with a short slug.
- Commit subject: imperative mood, capitalized, no trailing period, ~50–72
  chars ("Recover dirty tool-call inputs instead of aborting the stream").
  Non-feature commits may use a `docs:` / `test:` / `chore:` / `ci:` prefix —
  the changelog filters those out.
- Land through a GitHub pull request; keep the merge-commit history.
- No Claude session links or `Claude-Session:` trailers in commit messages, PR
  titles, or PR bodies.

---

## Behavioral spec (invariants)

A change that alters any of these is changing the product's contract and must
say so explicitly in the PR.

### Auth & network exposure

- **`/v1/*` always requires the bearer `api_key`.** No loopback bypass, no
  `ui_no_auth` exemption, ever. `/v1/responses` inherits this from the global
  middleware — don't add per-route gating that could weaken it.
- The admin surface (`/ui`, `/accounts*`, `/admin/*`) is reachable without a
  token **only** via the loopback + `Host` + `Origin` gate in
  `isLocalAdminRequest`, or when `ui_no_auth` is set. A LAN / Tailscale caller
  never gets the bypass.
- Admin routes never receive a wildcard `Access-Control-Allow-Origin`. Only
  `/v1/*` and other non-admin routes do.
- `--allow-lan` / `--allow-tailscale` change only the bind address. They do not
  weaken authentication.
- `--oauth` requires `--account <name>`.

### Model policy

- Every model is disabled until it has a `true` entry in `model_overrides`.
  `GET /v1/models` returns only enabled models — an empty list by default.
- A request for a disabled model returns `404` with the OpenAI-compatible error
  JSON shape.

### Proxy behavior

- The gateway is stateless. `previous_response_id` is not supported; the full
  conversation must arrive in each request.
- Chat requests round-robin across non-stale accounts. `401`/`403` marks an
  account stale and fails over; `429` marks it `limited`; timeouts/5xx don't
  change status. One request tries at most 3 accounts.
- Command Code API keys don't refresh: recovery is a fresh OAuth login, not a
  retry or restart.
- An OAuth reauth must preserve the billing-session fields (`session_token`,
  `session_email`, `session_expires_at`, `plan_expires_at`).
- Image content is accepted only as a base64 `data:` URL. Remote HTTP(S) image
  URLs are rejected with `400 invalid_request_error`.
- Cache-read tokens surface as `usage.prompt_tokens_details.cached_tokens`;
  cache-write tokens appear only in `/usage` and `/admin/usage`.

### Compatibility

- An older `config.yaml` must keep loading: legacy single-`commandcode` block,
  `exclude_models`, and webhook-implies-enabled all have decode paths that stay.
- `config.yaml`, `usage.json`, and `discord-alerts.json` stay gitignored and
  are never committed.
