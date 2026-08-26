# CommandCode API surface: CmdCdSwitcher vs. CommandCodeAI/desktop

## Summary

`CommandCodeAI/desktop` (https://github.com/CommandCodeAI/desktop, public, cloned
at commit reachable via `main` on 2026-08-25) contains **no application
source code**. Its own README states this directly:

> "This is the public download, installer, documentation, and issue-tracking
> repository for Command Code GUI. Application development happens in a
> private repository."
> — `README.md:103-107`

The repo's contents are limited to `install.sh`, `install.ps1`, `README.md`,
`INSTALL.md`, `CHANGELOG.md`, `SECURITY.md`, `SUPPORT.md`, and
`.github/ISSUE_TEMPLATE/`. There is no `package.json`, no `src/`, no
`api/`/`client/` directory, no `fetch`/`axios`/HTTP-client code, no
`openapi.yaml`/`swagger.json`, and no GraphQL schema. `SECURITY.md:22` and
`SUPPORT.md` point at a `CommandCodeAI/gui` repository as "the official"
source, but `gh repo view CommandCodeAI/gui` resolves to the same
`CommandCodeAI/desktop` repo — it was renamed, not a separate codebase.

**Conclusion: the desktop GUI's actual backend API calls are not observable
from this repository.** The only network endpoints present anywhere in the
repo are GitHub's own Releases API (used by the installer scripts to fetch
and verify the `.dmg`/`.deb`/`.exe` binaries), which is unrelated to
CommandCode's product backend and is already effectively covered by
CmdCdSwitcher's git-based tooling. No new CommandCode API endpoints could be
identified for CmdCdSwitcher to leverage, because none are present in the
source available to us. Per the task's own instruction ("If the repo is
private/inaccessible, say so explicitly and stop rather than fabricating
endpoints"), this report stops here rather than guessing at what the private
GUI app's backend calls might be.

## Baseline: endpoints CmdCdSwitcher currently calls

Outbound calls to Command Code's backend (`internal/app`):

| Method | Path | Base URL | Purpose | Source |
| --- | --- | --- | --- | --- |
| POST | `/alpha/generate` | `{account.BaseURL}` (default `https://api.commandcode.ai`, `internal/app/config.go:12`) | Chat/agent completion, SSE streaming response | `internal/app/cc.go:171` |
| GET | `/provider/v1/models` | `{account.BaseURL}` | List available models; also used as the unmetered account-health probe | `internal/app/models.go:15`, `internal/app/accounts.go:379` |
| GET | `/studio/auth/cli?callback=...&state=...` | `https://commandcode.ai` (`internal/app/oauth.go:19`) | Browser-based OAuth authorization page | `internal/app/oauth.go:173-174` |

Request headers sent on `/alpha/generate`: `Content-Type: application/json`,
`Authorization: Bearer {apiKey}`, `x-command-code-version: 0.24.1`,
`x-cli-environment: production` (`internal/app/cc.go:175-178`).

Locally, CmdCdSwitcher also *exposes* its own OpenAI-compatible surface
(inbound, not calls to CommandCode):

| Method | Path | Purpose | Source |
| --- | --- | --- | --- |
| POST | `/v1/chat/completions` | OpenAI-compatible chat endpoint, proxies to `/alpha/generate` | `internal/app/server.go:92` |
| GET | `/v1/models` | OpenAI-compatible model list, proxies to `/provider/v1/models` | `internal/app/server.go:93` |
| — | `/ui` | Loopback-only local web UI | `internal/app/server.go:287` |
| POST/GET | `/accounts/reauth` | Local reauth session management (wraps the OAuth flow above) | `internal/app/reauth.go:9,20` |

The local OAuth callback listener (`127.0.0.1:5959`+, path `/callback`) is
the receiving side of the OAuth flow, not an outbound API call
(`internal/app/oauth.go:16-18, 97`).

## Diff

No endpoints, base URLs, or API surface were found in `CommandCodeAI/desktop`
beyond GitHub's Releases API (`api.github.com/repos/CommandCodeAI/desktop`,
`install.sh:6`, `install.ps1:4` — installer/update mechanism only, not part
of CommandCode's product API and not something CmdCdSwitcher has a use for).
Therefore there is nothing to add to CmdCdSwitcher's baseline from this
source.

## Recommendation

To find additional CommandCode API surface, a different primary source is
needed — e.g. network capture of the actual desktop GUI app (binaries are
published on the repo's [Releases page](https://github.com/CommandCodeAI/desktop/releases)),
or access to the private application repository. Neither was in scope for
this pass.
