import { useCallback, useEffect, useRef, useState } from "react";
import { openMonitorStream, type MonitorStreamStatus } from "./api";
import type { MonitorEvent } from "./types";

// Keep the in-memory feed bounded — the tab shows live traffic, not a log.
const MAX_ROWS = 500;

const STATUS: Record<MonitorStreamStatus, { pill: string; text: string }> = {
  connecting: { pill: "unknown", text: "Connecting…" },
  live: { pill: "healthy", text: "Live" },
  reconnecting: { pill: "stale", text: "Reconnecting…" },
  error: { pill: "limited", text: "Disconnected" },
};

function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString();
}

function formatMicros(us: number): string {
  if (us < 1000) return `${us} µs`;
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)} ms`;
  return `${(us / 1_000_000).toFixed(2)} s`;
}

interface Row extends MonitorEvent {
  key: string;
}

export default function Monitoring() {
  const [rows, setRows] = useState<Row[]>([]);
  const [status, setStatus] = useState<MonitorStreamStatus>("connecting");
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  const seq = useRef(0);

  useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  useEffect(() => {
    const handle = openMonitorStream(
      (ev) => {
        if (pausedRef.current) return;
        setRows((prev) => {
          const next = [{ ...ev, key: `${Date.now()}-${seq.current++}` }, ...prev];
          return next.length > MAX_ROWS ? next.slice(0, MAX_ROWS) : next;
        });
      },
      (s) => setStatus(s)
    );
    return () => handle.close();
  }, []);

  const clear = useCallback(() => setRows([]), []);

  return (
    <div>
      <p className="subtitle">
        Live model-API calls handled by the gateway (<code>/v1/chat/completions</code>,{" "}
        <code>/v1/responses</code>, <code>/v1/models</code>). Only calls made while this tab is open
        appear here; switching away clears the list. <strong>Proxy</strong> is the gateway's own
        processing time; <strong>Command Code</strong> is the round trip to the upstream (outbound
        call plus reading its response).
      </p>

      <div className="toolbar">
        <span className={`pill ${STATUS[status].pill}`}>{STATUS[status].text}</span>
        <span className="muted">{rows.length} call{rows.length === 1 ? "" : "s"}</span>
        <div className="spacer"></div>
        <button onClick={() => setPaused((p) => !p)}>{paused ? "Resume" : "Pause"}</button>
        <button onClick={clear} disabled={rows.length === 0}>
          Clear
        </button>
      </div>

      <div className="card">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Method</th>
              <th>Path</th>
              <th>Model</th>
              <th>Account</th>
              <th>Status</th>
              <th>Proxy</th>
              <th>Command Code</th>
              <th>Total</th>
              <th>Client</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td colSpan={10} className="muted">
                  Waiting for model-API traffic… send a request through the gateway to see it here.
                </td>
              </tr>
            ) : (
              rows.map((r) => (
                <tr key={r.key}>
                  <td className="timestamp">{formatTime(r.time)}</td>
                  <td>{r.method}</td>
                  <td>
                    <code>{r.path}</code>
                    {r.stream ? <span className="pill variant-pill">stream</span> : null}
                  </td>
                  <td>{r.model || "—"}</td>
                  <td>{r.account || "—"}</td>
                  <td className={r.status >= 400 ? "error-text" : undefined}>{r.status}</td>
                  <td>{formatMicros(r.proxy_us)}</td>
                  <td>{formatMicros(r.upstream_us)}</td>
                  <td className="muted">{formatMicros(r.total_us)}</td>
                  <td className="muted">{r.client_ip || "—"}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
