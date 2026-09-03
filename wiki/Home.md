# cmdcode2api wiki

`cmdcode2api` is a local, OpenAI-compatible gateway for
[Command Code](https://commandcode.ai/). The [README](https://github.com/FlightlessWeasel/cmdcode2api#readme)
covers what it is and how to install and start it. This wiki holds the
reference material.

## Pages

- **[CLI Reference](CLI-Reference)** — every command-line flag and account
  operation.
- **[Configuration](Configuration)** — every `config.yaml` field.
- **[HTTP API](HTTP-API)** — all endpoints, request shapes, and auth rules.
- **[Accounts, Rotation & Failover](Accounts-Rotation-and-Failover)** — how the
  account pool, health probe, and failover behave.
- **[Remote & Headless OAuth](Remote-and-Headless-OAuth)** — authorizing an
  account on a server with no browser, via SSH port forwarding.
- **[Billing Session Token](Billing-Session-Token)** — the cookie that enables
  credit polling and threshold alerts.
- **[Discord Alerts](Discord-Alerts)** — usage-threshold and expiry alerts.

## Editing this wiki

These pages are mirrored from the `wiki/` directory in the main repository.
Edit the files there and push them to the wiki repo, so the source stays under
review with the code:

```bash
git clone https://github.com/FlightlessWeasel/cmdcode2api.wiki.git
cp /path/to/repo/wiki/*.md cmdcode2api.wiki/
cd cmdcode2api.wiki && git add -A && git commit -m "Sync wiki from repo" && git push
```
