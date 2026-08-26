import { useCallback, useEffect, useState } from "react";
import { fetchModels } from "./api";
import ListPanel from "./ListPanel";
import { errorMessage } from "./reauth";
import type { AdminModel } from "./types";

function formatContextLength(value: number): string {
  if (!value) return "—";
  return value.toLocaleString();
}

export default function Models() {
  const [models, setModels] = useState<AdminModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const data = await fetchModels();
      setModels(data || []);
      setError("");
    } catch (err) {
      setError("Failed to load models: " + errorMessage(err));
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

  return (
    <div>
      <p className="subtitle">Models available from the configured Command Code account(s).</p>

      <ListPanel error={error} loading={loading} refreshing={refreshing} refreshLabel="Refresh" onRefresh={handleRefresh}>
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Name</th>
              <th>Context length</th>
              <th>Owned by</th>
              <th>Excluded</th>
            </tr>
          </thead>
          <tbody>
            {models.length === 0 ? (
              <tr>
                <td colSpan={5} className="muted">
                  No models loaded yet — add a Command Code account to populate this list.
                </td>
              </tr>
            ) : (
              models.map((m) => (
                <tr key={m.id}>
                  <td>{m.id}</td>
                  <td className="muted">{m.name || "—"}</td>
                  <td>{formatContextLength(m.context_length)}</td>
                  <td className="muted">{m.owned_by}</td>
                  <td>
                    {m.excluded ? (
                      <span className="pill stale">excluded</span>
                    ) : (
                      <span className="pill healthy">available</span>
                    )}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </ListPanel>
    </div>
  );
}
