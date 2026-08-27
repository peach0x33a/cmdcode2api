import { useEffect, useState } from "react";
import { fetchDiscordAlerts, saveDiscordAlerts } from "./api";
import { errorMessage } from "./reauth";
import type { DiscordAlertsConfig } from "./types";

const defaults: DiscordAlertsConfig = { enabled: false, webhook_url: "", webhook_configured: false, mention_everyone: false, hourly_cap: 3, weekly_cap: 6, monthly_cap: 10 };

export default function Alerts() {
  const [form, setForm] = useState(defaults);
  const [webhook, setWebhook] = useState("");
  const [clearWebhook, setClearWebhook] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  useEffect(() => { fetchDiscordAlerts().then(setForm).catch((e) => setMessage(errorMessage(e))).finally(() => setLoading(false)); }, []);
  const set = <K extends keyof DiscordAlertsConfig>(key: K, value: DiscordAlertsConfig[K]) => setForm((f) => ({ ...f, [key]: value }));
  async function submit(e: React.FormEvent) {
    e.preventDefault(); setSaving(true); setMessage("");
    try {
      const saved = await saveDiscordAlerts({ ...form, webhook_url: webhook, clear_webhook: clearWebhook });
      setForm(saved); setWebhook(""); setClearWebhook(false); setMessage("Alert settings saved.");
    } catch (e) { setMessage(errorMessage(e)); } finally { setSaving(false); }
  }
  if (loading) return <p style={{ padding: "1rem" }}>Loading…</p>;
  return <div className="alerts-page">
    <p className="subtitle">Send useful billing and account warnings to a private Discord channel.</p>
    {message ? <div className={`banner ${message.includes("saved") ? "success" : "error"}`}>{message}</div> : null}
    <form onSubmit={submit}>
      <div className="card guide-card alert-hero"><div><span className="eyebrow">Discord delivery</span><h2>Stay ahead of limits</h2><p className="muted">Alerts are deduplicated and sent only when this gateway has fresh billing data.</p></div><label className="switch-label"><input type="checkbox" checked={form.enabled} onChange={(e) => set("enabled", e.target.checked)} /><span>Enabled</span></label></div>
      <div className="card guide-card"><label className="field-label" htmlFor="webhook">Webhook URL</label><input id="webhook" className="wide-input" type="password" placeholder={form.webhook_configured ? "Saved webhook (blank keeps it)" : "https://discord.com/api/webhooks/…"} value={webhook} onChange={(e) => { setWebhook(e.target.value); setClearWebhook(false); }} /><p className="field-help">Leave blank to keep the saved webhook. Enter a new URL to replace it.</p>{form.webhook_configured ? <label className="check-row"><input type="checkbox" checked={clearWebhook} onChange={(e) => setClearWebhook(e.target.checked)} /> Clear saved webhook</label> : null}<label className="check-row"><input type="checkbox" checked={form.mention_everyone} onChange={(e) => set("mention_everyone", e.target.checked)} /> Allow <code>@everyone</code> mentions</label></div>
      <div className="card guide-card"><h2>Alert caps</h2><p className="muted">Notify at 90%, 95%, and 100% of each configured cap.</p><div className="cap-grid">{([ ["hourly_cap", "Hourly", "five-hour request window"], ["weekly_cap", "Weekly", "weekly request window"], ["monthly_cap", "Monthly", "monthly credits consumed"] ] as const).map(([key, label, help]) => <label className="cap-field" key={key}><span>{label}</span><input type="number" min="1" step="any" value={form[key]} onChange={(e) => set(key, Number(e.target.value))} /><small>{help}</small></label>)}</div></div>
      <div className="card guide-card alert-explainer"><h2>What you’ll receive</h2><div className="alert-types"><p><strong>Usage thresholds</strong><br />A warning when an account reaches 90%, 95%, or 100% of its hourly, weekly, or monthly cap.</p><p><strong>Subscription expiry</strong><br />A reminder when a subscription ends within seven days.</p><p><strong>Session expiry</strong><br />A reminder when a billing session token expires within 24 hours.</p></div><div className="callout info"><strong>Monthly remaining credits</strong><br />The billing API reports credits remaining, not credits used. We calculate consumed credits as <code>monthly cap − remaining</code>, clamp it to 0–100%, and compare that with your monthly cap. Monthly alerts stay quiet until fresh billing data is available.</div><p className="muted examples">Example: a session token expiring tomorrow triggers an alert; one expiring in two days does not. With a monthly cap of 10, 1 credit remaining means 90% consumed; 0 means 100%.</p></div>
      <button className="primary save-alerts" type="submit" disabled={saving}>{saving ? "Saving…" : "Save alert settings"}</button>
    </form>
  </div>;
}
