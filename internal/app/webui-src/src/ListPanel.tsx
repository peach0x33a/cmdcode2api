import type { ReactNode } from "react";

export interface ListPanelProps {
  /** Banner text shown above the toolbar when non-empty. */
  error?: string;
  /** True while the initial load is in flight; shows "Loading…" instead of the table. */
  loading: boolean;
  /** True while a manual refresh is in flight; disables the refresh button and relabels it. */
  refreshing: boolean;
  /** Label for the refresh button in its resting state, e.g. "Refresh" or "Refresh status". */
  refreshLabel: string;
  onRefresh: () => void;
  /** Table (or other) content rendered inside the card once loading is done. */
  children: ReactNode;
}

// ListPanel renders the toolbar/card/loading/error chrome shared by the
// Models and Accounts tabs: an error banner, a refresh toolbar, and a card
// that shows a loading placeholder until the caller's content is ready.
export default function ListPanel({
  error,
  loading,
  refreshing,
  refreshLabel,
  onRefresh,
  children,
}: ListPanelProps) {
  return (
    <>
      {error ? <div className="banner error">{error}</div> : null}

      <div className="toolbar">
        <button onClick={onRefresh} disabled={refreshing}>
          {refreshing ? "Refreshing…" : refreshLabel}
        </button>
        <div className="spacer"></div>
      </div>

      <div className="card">{loading ? <p style={{ padding: "1rem" }}>Loading…</p> : children}</div>
    </>
  );
}
