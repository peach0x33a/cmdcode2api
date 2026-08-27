import { useCallback, useState } from "react";
import { useCopyToClipboard } from "./useCopyToClipboard";

export const MASK = "••••••••••••";

export interface CopyFieldProps {
  label: string;
  value: string;
  /** When true, the value is masked behind a "Show" toggle until clicked. */
  secret?: boolean;
  /** Controls the reveal state from a parent instead of tracking it locally. */
  revealed?: boolean;
  onToggleRevealed?: () => void;
}

export default function CopyField({
  label,
  value,
  secret,
  revealed: revealedProp,
  onToggleRevealed,
}: CopyFieldProps) {
  const [copied, copy] = useCopyToClipboard();
  const [revealedState, setRevealedState] = useState(!secret);
  const revealed = revealedProp ?? revealedState;
  const toggleRevealed = onToggleRevealed ?? (() => setRevealedState((r) => !r));

  const handleCopy = useCallback(() => copy(value), [copy, value]);

  return (
    <div className="copy-field">
      <div className="copy-field-label">{label}</div>
      <div className="copy-field-row">
        <code>{revealed ? value : MASK}</code>
        {secret ? (
          <button type="button" onClick={toggleRevealed}>
            {revealed ? "Hide" : "Show"}
          </button>
        ) : null}
        <button type="button" onClick={handleCopy}>
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
    </div>
  );
}
