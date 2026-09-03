# CLI Reference

The web UI at `/ui` is the primary way to run the gateway. Every account
operation also has a command-line equivalent, for scripting or a machine you
can't point a browser at.

Invoke the binary as `./cmdcode2api` (or `cmdcode2api` if it's on your `PATH`).
With no flags it starts the server.

## Flags

| Flag | Type | Default | Purpose |
| --- | --- | --- | --- |
| `--oauth` | bool | `false` | Authorize via browser OAuth to obtain a Command Code API key. Requires `--account`. Does not start the server. |
| `--account <name>` | string | — | Account name to authorize. **Required with `--oauth`.** |
| `--oauth-callback <url>` | string | `http://localhost:5959/callback` | Override the OAuth callback URL. Only needed when something other than `localhost:5959` must receive the callback (e.g. a public HTTPS reverse proxy). Do **not** set it for the SSH-tunnel case — see [Remote & Headless OAuth](Remote-and-Headless-OAuth). |
| `--list-accounts` | bool | `false` | Print each configured account and its live status from a one-time probe, then exit. |
| `--remove-account <name>` | string | — | Delete the named account from `config.yaml`, then exit. |
| `--host <host>` | string | `localhost` | HTTP listen host. Use `0.0.0.0` to listen on all interfaces. Overrides `host` in `config.yaml`. |
| `--port <n>` | int | `11434` | HTTP listen port. Overrides `port` in `config.yaml`. |
| `--allow-lan` | bool | `false` | Let other devices on the local network reach the gateway. Switches `host` to `0.0.0.0` if it's still the default. Does not weaken auth. |
| `--allow-tailscale` | bool | `false` | Same, for a device reachable over Tailscale. Switches `host` to `0.0.0.0` if it's still the default. |
| `--ui-no-auth` | bool | `false` | Serve `/ui` and the account-management API (`/accounts*`, `/admin/*`) with no authentication. The `/v1/*` proxy still requires `api_key`. Trusted networks only. Also settable as `ui_no_auth: true` in `config.yaml`. |
| `--debug` | bool | `false` | Print the full request body and all Command Code SSE events to stderr. These contain prompts and source — the debug dump paths are gitignored. |
| `--version` | bool | `false` | Print the version and Go runtime version, then exit. |

Flags override the corresponding `config.yaml` fields for that run only; they
are not persisted. See [Configuration](Configuration) for the file.

## Account operations

```bash
# Add an account, or reauthorize an existing name (overwrites its key).
./cmdcode2api --oauth --account personal

# List every account with its live status.
./cmdcode2api --list-accounts

# Remove an account.
./cmdcode2api --remove-account work
```

`--account <name>` is required whenever you use `--oauth`. The OAuth flow writes
the Command Code API key into `config.yaml` under that name. Repeat with a
different name to add more accounts.

Running `--oauth --account <name>` again for a name that already exists
overwrites that account's key — the same as **Reauthorize** in the UI. The
billing session token and related fields are preserved across a reauth.

An account added through the UI is live immediately. One added with `--oauth`
loads on the next gateway start.

Command Code API keys have no refresh mechanism, so a stale account needs a
fresh login, not a restart. See
[Accounts, Rotation & Failover](Accounts-Rotation-and-Failover).

## Running the server

```bash
# Start on the default localhost:11434.
./cmdcode2api

# Listen on all interfaces (useful for systemd or a remote server).
./cmdcode2api --host 0.0.0.0

# Open up to LAN and/or Tailscale (also switches host to 0.0.0.0).
./cmdcode2api --allow-lan --allow-tailscale

# Print the version and exit.
./cmdcode2api --version
```

The server logs every URL it is actually reachable at, for example:

```text
cmdcode2api starting, listening on 0.0.0.0:11434
reachable at http://localhost:11434 (loopback, no API key needed for /ui)
reachable at http://192.168.1.50:11434 (LAN — /ui requires the API key)
reachable at http://100.101.102.103:11434 (Tailscale — /ui requires the API key)
```

If `allow_tailscale` is set but Tailscale isn't installed or connected, the log
says so instead of printing a URL.

`--allow-lan` / `--allow-tailscale` only make the port reachable. They do not
weaken authentication: the web UI's tokenless login only applies to a genuine
same-machine (loopback) request. From a LAN or Tailscale device you still need
the `api_key`. `--ui-no-auth` removes that requirement for the admin surface
only; the `/v1/*` proxy routes always require `api_key`, and the startup log
prints a `WARNING: ui_no_auth is set` line while it is active.

## install.sh

`scripts/install.sh` is a headless installer/updater for Linux with systemd. It
pulls prebuilt release archives from GitHub — no Go toolchain required.

| Option | Purpose |
| --- | --- |
| `--update` | Update an existing install to the latest release, then restart. Rolls back automatically if the service fails to come back up. |
| `--os-upgrade` | Run a full OS package upgrade first (apt / dnf / pacman / zypper). |
| `--version TAG` | Install or update to a specific release tag (e.g. `v1.2.3`). Default: latest. |
| `--repo OWNER/NAME` | GitHub repo to fetch releases from. Default: `FlightlessWeasel/cmdcode2api`. Also `CMDCODE2API_REPO`. |
| `--dir PATH` | Install directory. Default: `/opt/cmdcode2api`. |
| `--service NAME` | systemd service name. Default: `cmdcode2api`. |
| `--user NAME` | Service account to create and run as. Default: `cmdcode2api`. |
| `--host HOST` | Listen host passed to the binary. Default: `0.0.0.0`. |
| `--no-start` | Install but don't start the service. |
| `--force` | Reinstall / rewrite the systemd unit even if one exists. |

After a fresh install with no `config.yaml`, authorize an account from the web
UI (`http://<host>:11434/ui` → **Add account**), or from the shell:

```bash
sudo -u cmdcode2api /opt/cmdcode2api/cmdcode2api --oauth --account Default
sudo systemctl restart cmdcode2api
```

## deploy.sh

`scripts/deploy.sh` builds from a source checkout and deploys: `git pull` →
`go vet` → `go test` → `go build` → atomic binary replace → `systemctl restart`,
with automatic rollback if the service doesn't come back active.

```bash
scripts/deploy.sh [--force] [REPO_DIR] [TARGET] [SERVICE]
```

Defaults: `REPO_DIR` is the checkout containing the script, `TARGET` is
`/opt/cmdcode2api/cmdcode2api`, `SERVICE` is `cmdcode2api`. `--force` stops the
service before replacing the binary.
