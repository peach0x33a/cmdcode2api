import StatusPill from "./StatusPill";
import { TERMINAL_REAUTH_STATUSES } from "./reauth";
import type { AccountView, ReauthSession } from "./types";

function formatTime(value?: string | null): string {
  if (!value) return "never";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "never";
  return d.toLocaleString();
}

export interface AccountRowProps {
  account: AccountView;
  reauth?: ReauthSession;
  onReauth: (name: string) => void;
  onDelete: (name: string) => void;
}

export default function AccountRow({ account, reauth, onReauth, onDelete }: AccountRowProps) {
  const busy = !!reauth && !TERMINAL_REAUTH_STATUSES.has(reauth.status);
  return (
    <tr>
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
  );
}
