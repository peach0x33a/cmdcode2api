import type { AccountStatus } from "./types";

const LABELS: Record<AccountStatus, string> = {
  healthy: "healthy",
  stale: "stale",
  limited: "rate limited",
  unknown: "unknown",
};

export default function StatusPill({ status }: { status?: AccountStatus }) {
  const cls = status && status in LABELS ? status : "unknown";
  return <span className={`pill ${cls}`}>{LABELS[cls]}</span>;
}
