// CreditBar renders remaining/total as a shrinking bar: full when nothing
// has been used, emptying as usage grows — the inverse of a typical "usage"
// progress bar, since what matters here is how much is left.
//
// The amount text (e.g. "3.50 / 10") is intentionally not rendered inside
// the bar — callers show it elsewhere (next to the field label) so that
// its variable width doesn't shrink the bar's available flex width and
// make bars of different rows render as different lengths. `label` is
// still accepted, but only as an accessible name for the bar element.
export interface CreditBarProps {
  remaining: number;
  total: number;
  label?: string;
}

export default function CreditBar({ remaining, total, label }: CreditBarProps) {
  const pct = total > 0 ? Math.max(0, Math.min(100, (remaining / total) * 100)) : 0;
  return (
    <div className="credit-bar" role="progressbar" aria-label={label} aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div className="credit-bar-track">
        <div className={`credit-bar-fill${pct < 20 ? " low" : ""}`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}
