import { useCallback, useEffect, useState } from "react";
import { fetchAccounts, UnauthorizedError } from "./api";
import { getStoredKey, setStoredKey } from "./auth";

type Status = "checking" | "ok" | "locked";

// AuthGate wraps the app in a check against /accounts before rendering
// anything that depends on the admin API. A loopback caller (or anyone who
// already has a working stored key) passes straight through; everyone else
// — a LAN or Tailscale device, which the server does not exempt from the
// bearer-token check — sees a small key-entry form instead.
export default function AuthGate({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<Status>("checking");
  const [key, setKey] = useState(getStoredKey);
  const [error, setError] = useState("");

  const probe = useCallback(async () => {
    setStatus("checking");
    try {
      await fetchAccounts();
      setStatus("ok");
    } catch (err) {
      // A non-auth failure (e.g. the gateway is unreachable) isn't
      // something a login form fixes — let the rest of the app's own
      // error handling surface it instead of blocking here.
      setStatus(err instanceof UnauthorizedError ? "locked" : "ok");
    }
  }, []);

  useEffect(() => {
    probe();
  }, [probe]);

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setStoredKey(key.trim());
      setError("");
      try {
        await fetchAccounts();
        setStatus("ok");
      } catch (err) {
        setError(
          err instanceof UnauthorizedError
            ? "That key was rejected. Check config.yaml's api_key and try again."
            : "Could not reach the gateway."
        );
      }
    },
    [key]
  );

  if (status === "checking") {
    return <p style={{ padding: "1rem" }}>Loading…</p>;
  }

  if (status === "locked") {
    return (
      <div>
        <h1>cmdcode2api</h1>
        <div className="card guide-card">
          <h2>API key required</h2>
          <p className="subtitle">
            This gateway isn't being opened from the machine hosting it, so it needs the bearer{" "}
            <code>api_key</code> from <code>config.yaml</code> to unlock the admin UI.
          </p>
          {error ? <div className="banner error">{error}</div> : null}
          <form className="add-account" onSubmit={handleSubmit}>
            <input
              type="password"
              placeholder="api_key"
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
            <button className="primary" type="submit">
              Unlock
            </button>
          </form>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
