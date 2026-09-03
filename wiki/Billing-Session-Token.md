# Billing Session Token

Credit polling — the web UI credit bars, `GET /admin/billing`, and the
usage-threshold Discord alerts — needs a `session_token` for the account. This
is the `__Secure-commandcode_prod_.session_token` browser cookie from a
logged-in commandcode.ai session. The gateway's `api_key` won't work here;
Command Code's billing API authenticates with this cookie instead.

## Getting it

1. Log in to <https://commandcode.ai> in a browser.
2. Open DevTools (F12) → **Application** (Chrome) or **Storage** (Firefox) →
   **Cookies** → `https://commandcode.ai`.
3. Copy the **Value** of the `__Secure-commandcode_prod_.session_token` cookie.
4. Paste it into the **Session token** field for that account on the Accounts
   tab and save, or set `session_token:` under the account in `config.yaml`.

## Lifetime and handling

The token expires — Command Code issues short-lived sessions, which is why the
24-hour session-expiry alert exists. When it lapses, repeat the steps with a
fresh value.

Treat it like a password: it grants read access to your billing data. The
gateway keeps it only in `config.yaml` (git ignored), never logs it, and never
returns it from any endpoint. It is preserved across an OAuth reauth.

Without a session token, an account still works as an LLM proxy; you just get no
credit data for it, and a config with a webhook still sends subscription- and
session-expiry warnings but no usage-threshold alerts.
