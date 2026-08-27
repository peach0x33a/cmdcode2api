// Toggle is a small switch control used by the Models tab for both a
// single model's enabled state and a whole family's (which can be
// "mixed" — some members enabled, some not). It is a controlled,
// presentational component: callers own the enabled state and pass the
// next value back through onChange.
export interface ToggleProps {
  checked: boolean;
  /** True when the underlying state is a mix of on/off (e.g. a family with
   * some but not all members enabled) — renders a third, in-between visual
   * state instead of committing to on or off. */
  mixed?: boolean;
  disabled?: boolean;
  /** Accessible name; not rendered visibly. */
  label: string;
  onChange: (next: boolean) => void;
}

export default function Toggle({ checked, mixed, disabled, label, onChange }: ToggleProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={mixed ? "mixed" : checked}
      aria-label={label}
      disabled={disabled}
      className={`toggle${checked ? " on" : ""}${mixed ? " mixed" : ""}`}
      onClick={() => onChange(mixed ? false : !checked)}
    >
      <span className="toggle-thumb" />
    </button>
  );
}
