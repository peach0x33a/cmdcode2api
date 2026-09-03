# Accounts, Rotation & Failover

Command Code accounts are stored by name in `config.yaml`. Add as many as you
like. The **Accounts** tab in the web UI does all of it: **Add account** and
**Reauthorize** run the OAuth flow, each row shows live status, and **Delete**
removes one.

The same operations from the command line:

```bash
./cmdcode2api --oauth --account personal    # add, or reauthorize an existing name
./cmdcode2api --list-accounts               # names and live status
./cmdcode2api --remove-account work         # remove
```

Running `--oauth --account <name>` again for an existing name overwrites that
account's key — the same as **Reauthorize** in the UI. Accounts added by CLI
load on the next gateway start; accounts added in the UI are live immediately.

## Rotation and failover

While the server is running, `/v1/chat/completions` and `/v1/responses`
requests round-robin across every account that isn't marked stale.

- If Command Code returns `401` or `403` for an account, the gateway marks it
  **stale** and routes subsequent requests to the remaining accounts.
- One request will try at most 3 accounts before giving up, so a request
  against a large pool of mostly-stale accounts still fails fast.
- A background probe rechecks every account roughly every 10 minutes (a
  lightweight authenticated call), so a dead key is caught even without live
  traffic.
- A `200` response clears a stale flag.
- Transient failures — timeouts, 5xx — are recorded but leave the account's
  status untouched.
- A `429` rate-limit / quota-exhaustion response marks the account **limited**
  rather than stale: the key still authenticates, so reauthorizing would not
  help.

## Reauthorizing a stale account

Command Code API keys have no refresh mechanism, so a stale account needs a
fresh login, not a restart. Click **Reauthorize** on the account's row in the
web UI, or run:

```bash
./cmdcode2api --oauth --account <name>
```

The billing session token and related fields are preserved across a reauth.

Check current status any time on the Accounts tab, with `--list-accounts`, or
via `GET /accounts`.

## Billing / credit polling

Once an account has a `session_token`, the gateway polls its subscription,
credits, and rolling usage windows about once a minute. This feeds the UI's
credit bars, `GET /admin/billing`, and the usage-threshold Discord alerts. See
[Billing Session Token](Billing-Session-Token) and [Discord Alerts](Discord-Alerts).
