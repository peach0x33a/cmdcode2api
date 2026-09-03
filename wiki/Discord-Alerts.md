# Discord Alerts

Set a webhook URL and the gateway posts a Discord message when usage crosses a
configured threshold or a subscription / billing session is about to lapse.

## Configuration

```yaml
discord_alerts:
  enabled: true
  webhook_url: https://discord.com/api/webhooks/...
  state_file: discord-alerts.json
  hourly_cap: 3
  weekly_cap: 6
  monthly_cap: 10
  mention_everyone: false
```

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | inferred | Master switch. If unset but `webhook_url` is present, treated as enabled (legacy behavior). An explicit `false` survives restarts. |
| `webhook_url` | — | Discord webhook URL. Never logged. In the UI the field is write-only: blank keeps the current URL, a new value replaces it, and there is an explicit clear action. |
| `state_file` | `discord-alerts.json` | Durable deduplication state path, so the same alert isn't sent twice across restarts. Git ignored. |
| `hourly_cap` | `3` | Cap for Command Code's rolling five-hour usage window. |
| `weekly_cap` | `6` | Cap for the weekly window. |
| `monthly_cap` | `10` | Cap for the monthly window. |
| `mention_everyone` | `false` | Prefix every alert with the plain-text `@everyone` mention. |

The legacy top-level `discord_webhook_url` and `discord_alert_state_file` fields
are still accepted and map to `webhook_url` / `state_file`.

## What fires when

- **Usage thresholds** need per-account credit data, which comes from a
  [billing session token](Billing-Session-Token). Without one, threshold alerts
  don't fire, but subscription- and session-expiry warnings still do.
- **Subscription expiry**: within seven days of the plan's renewal / end date.
  A separate warning covers a subscription set to auto-renew.
- **Billing session expiry**: within 24 hours of the session token expiring.

The monthly API value `monthlyCredits` is a *remaining* balance, so monthly
consumed credits are computed as `monthly_cap - monthlyCredits`, clamped to the
cap. With the default `monthly_cap: 10`, remaining values of 1, 0.5, and 0
correspond to 90%, 95%, and 100% consumed.
