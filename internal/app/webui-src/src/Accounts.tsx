import { useCallback, useEffect, useRef, useState } from "react";
import AccountRow from "./AccountRow";
import {
  deleteAccount,
  fetchAccounts,
  fetchBilling,
  fetchConnection,
  pollReauth,
  probeAccounts,
  saveBillingToken,
  startReauth,
} from "./api";
import CopyField from "./CopyField";
import ListPanel from "./ListPanel";
import { errorMessage, errorSession, TERMINAL_REAUTH_STATUSES } from "./reauth";
import type { AccountBilling, AccountView, ConnectionInfo, ReauthSession } from "./types";

// hostOf pulls just the hostname out of a base URL like
// "http://192.168.1.50:11434/v1", for splicing into the ssh example below.
function hostOf(url: string | undefined): string {
  if (!url) return "";
  try {
    return new URL(url).hostname;
  } catch {
    return "";
  }
}

const POLL_INTERVAL_MS = 15000;
const REAUTH_POLL_MS = 2000;

export default function Accounts() {
  const [accounts, setAccounts] = useState<AccountView[]>([]);
  const [billing, setBilling] = useState<Record<string, AccountBilling>>({});
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [newName, setNewName] = useState("");
  const [reauthSessions, setReauthSessions] = useState<Record<string, ReauthSession>>({});
  const [conn, setConn] = useState<ConnectionInfo | null>(null);
  const pollTimers = useRef<Record<string, ReturnType<typeof setTimeout>>>({});

  const loadAccounts = useCallback(async () => {
    try {
      const data = await fetchAccounts();
      setAccounts(data || []);
      setError("");
    } catch (err) {
      setError("Failed to load accounts: " + errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  const loadBilling = useCallback(async () => {
    try {
      const data = await fetchBilling();
      const byName: Record<string, AccountBilling> = {};
      for (const row of data || []) {
        byName[row.account] = row;
      }
      setBilling(byName);
    } catch {
      // Billing is a secondary panel — a failed fetch here shouldn't blank
      // out the primary account list/error state above.
    }
  }, []);

  useEffect(() => {
    loadAccounts();
    const id = setInterval(loadAccounts, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [loadAccounts]);

  useEffect(() => {
    loadBilling();
    const id = setInterval(loadBilling, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [loadBilling]);

  useEffect(() => {
    // Best-effort: the headless-auth help renders fine without it, so a
    // failure here is swallowed rather than surfaced in the error banner.
    fetchConnection()
      .then(setConn)
      .catch(() => {});
  }, []);

  const handleSaveToken = useCallback(async (name: string, sessionToken: string) => {
    const row = await saveBillingToken(name, sessionToken);
    setBilling((prev) => ({ ...prev, [name]: row }));
  }, []);

  useEffect(() => {
    // Stop every in-flight reauth poll on unmount, so a fast-navigating
    // browser tab doesn't leak timers.
    const timers = pollTimers.current;
    return () => {
      Object.values(timers).forEach(clearTimeout);
    };
  }, []);

  const pollUntilTerminal = useCallback(
    (name: string) => {
      const tick = async () => {
        try {
          const session = await pollReauth(name);
          setReauthSessions((prev) => ({ ...prev, [name]: session }));
          if (TERMINAL_REAUTH_STATUSES.has(session.status)) {
            delete pollTimers.current[name];
            if (session.status === "success") {
              loadAccounts();
            }
            return;
          }
        } catch (err) {
          setReauthSessions((prev) => ({ ...prev, [name]: errorSession(name, err) }));
          delete pollTimers.current[name];
          return;
        }
        pollTimers.current[name] = setTimeout(tick, REAUTH_POLL_MS);
      };
      tick();
    },
    [loadAccounts]
  );

  const handleReauth = useCallback(
    async (name: string) => {
      try {
        const session = await startReauth(name);
        setReauthSessions((prev) => ({ ...prev, [name]: session }));
        if (session.auth_url) {
          window.open(session.auth_url, "_blank", "noopener");
        }
        pollUntilTerminal(name);
      } catch (err) {
        setReauthSessions((prev) => ({ ...prev, [name]: errorSession(name, err) }));
      }
    },
    [pollUntilTerminal]
  );

  const handleDelete = useCallback(
    async (name: string) => {
      if (!window.confirm(`Delete account "${name}"? This removes its saved credentials.`)) {
        return;
      }
      // Stop polling for this account immediately, and drop its reauth
      // session, so an in-flight reauth can't call loadAccounts() on
      // eventual success and make the just-deleted account reappear.
      const timer = pollTimers.current[name];
      if (timer !== undefined) {
        clearTimeout(timer);
        delete pollTimers.current[name];
      }
      setReauthSessions((prev) => {
        if (!(name in prev)) return prev;
        const next = { ...prev };
        delete next[name];
        return next;
      });
      setAccounts((prev) => prev.filter((a) => a.name !== name));
      try {
        await deleteAccount(name);
      } catch (err) {
        setError("Failed to delete " + name + ": " + errorMessage(err));
      } finally {
        loadAccounts();
      }
    },
    [loadAccounts]
  );

  const handleRefresh = useCallback(async () => {
    setRefreshing(true);
    try {
      const data = await probeAccounts();
      setAccounts(data || []);
      setError("");
    } catch (err) {
      setError("Failed to refresh status: " + errorMessage(err));
    } finally {
      setRefreshing(false);
    }
  }, []);

  const handleAdd = useCallback(
    (evt: React.FormEvent) => {
      evt.preventDefault();
      const name = newName.trim();
      if (!name) return;
      setNewName("");
      handleReauth(name);
    },
    [newName, handleReauth]
  );

  // Prefer a Tailscale or LAN address for the ssh example; a loopback host is
  // useless as an ssh target, so fall back to the "server" placeholder.
  const sshHost =
    [conn?.tailscale_base_url, conn?.lan_base_url, conn?.base_url]
      .map(hostOf)
      .find((h) => h && h !== "localhost" && h !== "127.0.0.1") || "server";

  return (
    <div>
      <p className="subtitle">Command Code accounts configured for this gateway.</p>

      <ListPanel
        error={error}
        loading={loading}
        refreshing={refreshing}
        refreshLabel="Refresh status"
        onRefresh={handleRefresh}
      >
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Account</th>
              <th>Status</th>
              <th>Last checked</th>
              <th>Last error</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {accounts.length === 0 ? (
              <tr>
                <td colSpan={6} className="muted">
                  No accounts configured yet — add one below to get started.
                </td>
              </tr>
            ) : (
              accounts.map((a) => (
                <AccountRow
                  key={a.name}
                  account={a}
                  reauth={reauthSessions[a.name]}
                  billing={billing[a.name]}
                  onReauth={handleReauth}
                  onDelete={handleDelete}
                  onSaveToken={handleSaveToken}
                />
              ))
            )}
          </tbody>
        </table>
      </ListPanel>

      <form className="add-account" onSubmit={handleAdd}>
        <input
          type="text"
          placeholder="New account name"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
        />
        <button className="primary" type="submit">
          Add account
        </button>
      </form>

      <details className="card remote-auth-help">
        <summary>Authorizing an account on a remote / headless server</summary>
        <p>
          Do it right here &mdash; the &ldquo;Add account&rdquo; and
          &ldquo;Reauthorize&rdquo; buttons above run the whole OAuth flow. You
          do not need the CLI or <code>--oauth</code>.
        </p>
        <p>
          The one catch on a headless server: authorization waits for a callback
          on <code>http://localhost:5959/callback</code>, and Command Code only
          allows a <code>localhost</code> callback. If the gateway runs on a
          machine without a browser, forward that port over SSH from the machine
          you&rsquo;re browsing from (Windows included).
        </p>
        {conn ? (
          <div className="snippet">
            <div className="snippet-label">
              This gateway is reachable at &mdash; pick the address you can SSH to
              and browse:
            </div>
            <CopyField label="Base URL" value={conn.base_url} />
            {conn.lan_base_url ? (
              <CopyField label="LAN URL" value={conn.lan_base_url} />
            ) : null}
            {conn.tailscale_base_url ? (
              <CopyField label="Tailscale URL" value={conn.tailscale_base_url} />
            ) : null}
          </div>
        ) : null}
        <ol>
          <li>
            From your workstation, open an SSH session that forwards the callback
            port, and leave it open for the whole flow:
            <pre className="code-block">ssh -L 5959:localhost:5959 user@{sshHost}</pre>
          </li>
          <li>
            Back in this UI, click &ldquo;Add account&rdquo; or
            &ldquo;Reauthorize&rdquo; above.
          </li>
          <li>
            Approve in the Command Code tab that opens on your workstation. The
            callback travels back through the tunnel; the key lands in{" "}
            <code>config.yaml</code> and the account appears immediately &mdash;
            no restart needed.
          </li>
          <li>Close the SSH session.</li>
        </ol>
        <p className="muted">
          Port 5959 is fixed &mdash; don&rsquo;t set <code>--oauth-callback</code>{" "}
          for the tunnel case; the default <code>localhost</code> callback is what
          makes it work.
        </p>
      </details>
    </div>
  );
}
