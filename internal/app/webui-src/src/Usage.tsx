import { useCallback, useEffect, useState } from "react";
import { fetchUsage } from "./api";
import ListPanel from "./ListPanel";
import { errorMessage } from "./reauth";
import type { AccountUsage, UsageReport } from "./types";

function formatCount(value: number): string {
  if (!value) return "—";
  return value.toLocaleString();
}

export default function Usage() {
  const [report, setReport] = useState<UsageReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const data = await fetchUsage();
      setReport(data);
      setError("");
    } catch (err) {
      setError("Failed to load usage: " + errorMessage(err));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const handleRefresh = useCallback(() => {
    setRefreshing(true);
    load();
  }, [load]);

  const accounts: AccountUsage[] = report?.accounts || [];

  return (
    <div>
      <p className="subtitle">
        Lifetime totals: {formatCount(report?.total_requests ?? 0)} requests,{" "}
        {formatCount(report?.prompt_tokens ?? 0)} prompt tokens, {formatCount(report?.completion_tokens ?? 0)}{" "}
        completion tokens, {formatCount(report?.cache_read_tokens ?? 0)} cache read tokens,{" "}
        {formatCount(report?.cache_write_tokens ?? 0)} cache write tokens.
      </p>
      <p className="muted">
        These lifetime totals are not the sum of the per-account rows below — usage recorded before
        per-account tracking existed only counts toward the totals above.
      </p>

      <ListPanel error={error} loading={loading} refreshing={refreshing} refreshLabel="Refresh" onRefresh={handleRefresh}>
        <table>
          <thead>
            <tr>
              <th>Account</th>
              <th>Requests</th>
              <th>Prompt Tokens</th>
              <th>Completion Tokens</th>
              <th>Cache Read Tokens</th>
              <th>Cache Write Tokens</th>
            </tr>
          </thead>
          <tbody>
            {accounts.length === 0 ? (
              <tr>
                <td colSpan={6} className="muted">
                  No per-account usage recorded yet — send a request through an account to populate this list.
                </td>
              </tr>
            ) : (
              accounts.map((a) => (
                <tr key={a.account}>
                  <td>{a.account}</td>
                  <td>{formatCount(a.total_requests)}</td>
                  <td>{formatCount(a.prompt_tokens)}</td>
                  <td>{formatCount(a.completion_tokens)}</td>
                  <td>{formatCount(a.cache_read_tokens)}</td>
                  <td>{formatCount(a.cache_write_tokens)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </ListPanel>
    </div>
  );
}
