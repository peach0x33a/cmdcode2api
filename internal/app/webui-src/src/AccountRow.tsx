import { useState } from "react";
import CreditBar from "./CreditBar";
import StatusPill from "./StatusPill";
import { TERMINAL_REAUTH_STATUSES } from "./reauth";
import type { AccountBilling, AccountView, BillingWindow, ReauthSession } from "./types";

// MONTHLY_CREDIT_CAP is not part of the credits API response — the endpoint
// only reports how many monthly credits remain, not the plan's total. 10 is
// the fixed monthly allotment for the plans this gateway currently supports.
const MONTHLY_CREDIT_CAP = 10;

function formatTime(value?: string | null): string {
  if (!value) return "never";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "never";
  return d.toLocaleString();
}

// isSubscriptionExpired treats a missing subscription, a non-"active"
// status, a set endedAt, or a currentPeriodEnd already in the past as
// expired — any one of those means the plan shown below would be
// misleading if presented as still current.
function isSubscriptionExpired(sub: AccountBilling["subscription"]): boolean {
  if (!sub) return true;
  if (sub.status !== "active") return true;
  if (sub.endedAt) return true;
  const end = new Date(sub.currentPeriodEnd).getTime();
  return !Number.isNaN(end) && end < Date.now();
}

// windowAmount formats a window's remaining/cap as the "3.50 / 10" text
// shown next to the field label, above the bar.
function windowAmount(window: BillingWindow): string {
  const rem = Math.max(0, window.cap - window.used);
  return `${rem.toFixed(2)} / ${window.cap}`;
}

function windowBar(window: BillingWindow) {
  const rem = Math.max(0, window.cap - window.used);
  return <CreditBar remaining={rem} total={window.cap} label={windowAmount(window)} />;
}

// statusCode pulls just the HTTP status (e.g. "401") out of BillingInfo's
// LastError, which is a longer "<endpoint>: http <code>: <body>" string
// (see billing.go's refreshOne) — the UI should surface the code, not the
// full upstream error body.
function statusCode(err: string): string {
  const m = err.match(/http (\d+)/i);
  return m ? m[1] : err;
}

export interface AccountRowProps {
  account: AccountView;
  reauth?: ReauthSession;
  billing?: AccountBilling;
  onReauth: (name: string) => void;
  onDelete: (name: string) => void;
  onSaveToken: (name: string, sessionToken: string) => Promise<void>;
}

export default function AccountRow({
  account,
  reauth,
  billing,
  onReauth,
  onDelete,
  onSaveToken,
}: AccountRowProps) {
  const busy = !!reauth && !TERMINAL_REAUTH_STATUSES.has(reauth.status);
  const [tokenDraft, setTokenDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");

  const handleSaveToken = async () => {
    if (!tokenDraft.trim()) return;
    setSaving(true);
    setSaveError("");
    try {
      await onSaveToken(account.name, tokenDraft.trim());
      setTokenDraft("");
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const expired = isSubscriptionExpired(billing?.subscription);
  const sessionWorking = !!billing && !billing.last_error;

  return (
    <>
      <tr className="account-row">
        <td>{account.name}</td>
        <td className="muted">{account.email || account.key_name || "—"}</td>
        <td>
          <StatusPill status={account.status} />
        </td>
        <td className="timestamp">{formatTime(account.last_checked)}</td>
        <td className="error-text">{account.last_error || ""}</td>
        <td>
          <div className="row-actions">
            <button disabled={busy} onClick={() => onReauth(account.name)}>
              {busy ? "Re-authorizing…" : "Re-auth"}
            </button>
            <button className="danger" onClick={() => onDelete(account.name)}>
              Delete
            </button>
          </div>
          {reauth ? (
            <div className="reauth-status">
              status: {reauth.status}
              {reauth.error ? ` — ${reauth.error}` : null}
            </div>
          ) : null}
        </td>
      </tr>
      <tr className="billing-row">
        <td colSpan={6}>
          <div className="billing-panel">
            {!sessionWorking ? (
              <div className="billing-token-row">
                <input
                  type="password"
                  placeholder="Session token"
                  value={tokenDraft}
                  onChange={(e) => setTokenDraft(e.target.value)}
                />
                <button disabled={saving || !tokenDraft.trim()} onClick={handleSaveToken}>
                  {saving ? "Saving…" : "Save"}
                </button>
              </div>
            ) : null}
            {saveError ? <div className="error-text">{saveError}</div> : null}
            {billing ? (
              <div className="billing-summary">
                <div className="billing-info-col">
                  <div className="billing-field">
                    <span className="billing-field-label">Current Plan</span>
                    <span className="billing-field-value">
                      {expired ? (
                        <span className="billing-expired">Expired</span>
                      ) : (
                        billing.subscription?.planId || "—"
                      )}
                    </span>
                  </div>
                  <div className="billing-field">
                    <span className="billing-field-label">Plan expiration</span>
                    <span className="billing-field-value">
                      {expired ? "—" : formatTime(billing.subscription?.currentPeriodEnd)}
                    </span>
                  </div>
                  <div className="billing-field">
                    <span className="billing-field-label">Session</span>
                    <span className="billing-field-value">
                      {billing.session_email || "—"}
                      {billing.session_expires_at
                        ? ` (expires ${formatTime(billing.session_expires_at)})`
                        : ""}
                    </span>
                  </div>
                </div>
                <div className="billing-bars-col">
                  <div className="billing-field">
                    <div className="billing-field-header">
                      <span className="billing-field-label">Remaining Monthly</span>
                      {billing.credits ? (
                        <span className="billing-field-amount">
                          {`${billing.credits.credits.monthlyCredits.toFixed(2)} / ${MONTHLY_CREDIT_CAP}`}
                        </span>
                      ) : null}
                    </div>
                    {billing.credits ? (
                      <CreditBar
                        remaining={billing.credits.credits.monthlyCredits}
                        total={MONTHLY_CREDIT_CAP}
                        label="Remaining monthly credits"
                      />
                    ) : (
                      <span className="billing-field-value">—</span>
                    )}
                  </div>
                  <div className="billing-field">
                    <div className="billing-field-header">
                      <span className="billing-field-label">Remaining Weekly</span>
                      {billing.credits ? (
                        <span className="billing-field-amount">
                          {windowAmount(billing.credits.windowLimits.weekly)}
                        </span>
                      ) : null}
                    </div>
                    {billing.credits ? (
                      windowBar(billing.credits.windowLimits.weekly)
                    ) : (
                      <span className="billing-field-value">—</span>
                    )}
                  </div>
                  <div className="billing-field">
                    <div className="billing-field-header">
                      <span className="billing-field-label">Remaining 5 Hours</span>
                      {billing.credits ? (
                        <span className="billing-field-amount">
                          {windowAmount(billing.credits.windowLimits.fiveHour)}
                        </span>
                      ) : null}
                    </div>
                    {billing.credits ? (
                      windowBar(billing.credits.windowLimits.fiveHour)
                    ) : (
                      <span className="billing-field-value">—</span>
                    )}
                  </div>
                </div>
              </div>
            ) : (
              <div className="muted">No billing data fetched yet.</div>
            )}
            {billing?.last_error ? (
              <div className="error-text">Status: {statusCode(billing.last_error)}</div>
            ) : null}
          </div>
        </td>
      </tr>
    </>
  );
}
