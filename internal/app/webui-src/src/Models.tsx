import { useCallback, useEffect, useMemo, useState } from "react";
import { fetchModels, setFamilyOverride, setModelOverrides } from "./api";
import ListPanel from "./ListPanel";
import ModelFamilyGroup from "./ModelFamilyGroup";
import { errorMessage } from "./reauth";
import type { AdminModel } from "./types";

interface FamilyGroup {
  family: string;
  familyLabel: string;
  models: AdminModel[];
}

// groupByFamily groups models by their Family field, preserving the
// catalog's original order both across groups and within each group.
function groupByFamily(models: AdminModel[]): FamilyGroup[] {
  const order: string[] = [];
  const byFamily = new Map<string, AdminModel[]>();
  for (const m of models) {
    if (!byFamily.has(m.family)) {
      byFamily.set(m.family, []);
      order.push(m.family);
    }
    byFamily.get(m.family)!.push(m);
  }
  return order.map((family) => {
    const members = byFamily.get(family)!;
    return { family, familyLabel: members[0].family_label, models: members };
  });
}

export default function Models() {
  const [models, setModels] = useState<AdminModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [busyIds, setBusyIds] = useState<Set<string>>(new Set());
  const [busyFamilies, setBusyFamilies] = useState<Set<string>>(new Set());

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

  // Both handlers below optimistically flip the affected row(s) so the
  // toggle feels instant, then replace local state with the server's fresh
  // response on success. On failure they drop the optimistic update by
  // re-fetching the authoritative list — mirroring Accounts.tsx's
  // handleDelete, which resyncs via loadAccounts() in its `finally` rather
  // than hand-rolling a snapshot restore.
  const handleToggleModel = useCallback(
    async (id: string, enabled: boolean) => {
      setModels((prev) => prev.map((m) => (m.id === id ? { ...m, excluded: !enabled, overridden: true } : m)));
      setBusyIds((prev) => new Set(prev).add(id));
      try {
        const fresh = await setModelOverrides([id], enabled);
        setModels(fresh);
        setError("");
      } catch (err) {
        setError("Failed to update " + id + ": " + errorMessage(err));
        load();
      } finally {
        setBusyIds((prev) => {
          const next = new Set(prev);
          next.delete(id);
          return next;
        });
      }
    },
    [load]
  );

  const handleToggleFamily = useCallback(
    async (family: string, enabled: boolean) => {
      setModels((prev) =>
        prev.map((m) => (m.family === family ? { ...m, excluded: !enabled, overridden: true } : m))
      );
      setBusyFamilies((prev) => new Set(prev).add(family));
      try {
        const fresh = await setFamilyOverride(family, enabled);
        setModels(fresh);
        setError("");
      } catch (err) {
        setError("Failed to update " + family + ": " + errorMessage(err));
        load();
      } finally {
        setBusyFamilies((prev) => {
          const next = new Set(prev);
          next.delete(family);
          return next;
        });
      }
    },
    [load]
  );

  const groups = useMemo(() => groupByFamily(models), [models]);

  return (
    <div>
      <div className="callout info">
        Every model starts disabled. Enable only what your plan actually serves — check your plan's model list
        first.{" "}
        <a href="https://commandcode.ai/docs/plans/go#models" target="_blank" rel="noopener noreferrer">
          Go plan models
        </a>
      </div>

      <p className="subtitle">
        Models available from the configured Command Code account(s). Disabled models — including every model
        that has never been explicitly enabled — are hidden from /v1/models and rejected by chat/completions and
        responses calls.
      </p>

      <ListPanel error={error} loading={loading} refreshing={refreshing} refreshLabel="Refresh" onRefresh={handleRefresh}>
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Name</th>
              <th>Context length</th>
              <th>Owned by</th>
              <th>Enabled</th>
            </tr>
          </thead>
          {models.length === 0 ? (
            <tbody>
              <tr>
                <td colSpan={5} className="muted">
                  No models loaded yet — add a Command Code account to populate this list.
                </td>
              </tr>
            </tbody>
          ) : (
            groups.map((g) => (
              <ModelFamilyGroup
                key={g.family || g.models[0].id}
                family={g.family}
                familyLabel={g.familyLabel}
                models={g.models}
                busyIds={busyIds}
                busyFamilies={busyFamilies}
                onToggleModel={handleToggleModel}
                onToggleFamily={handleToggleFamily}
              />
            ))
          )}
        </table>
      </ListPanel>
    </div>
  );
}
