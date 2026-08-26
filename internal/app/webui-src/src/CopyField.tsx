import { useCallback, useState } from "react";
import { useCopyToClipboard } from "./useCopyToClipboard";

const MASK = "••••••••••••";

export interface CopyFieldProps {
  label: string;
  value: string;
  /** When true, the value is masked behind a "Show" toggle until clicked. */
  secret?: boolean;
}

export default function CopyField({ label, value, secret }: CopyFieldProps) {
  const [copied, copy] = useCopyToClipboard();
  const [revealed, setRevealed] = useState(!secret);

  const handleCopy = useCallback(() => copy(value), [copy, value]);

  return (
    <div className="copy-field">
      <div className="copy-field-label">{label}</div>
      <div className="copy-field-row">
        <code>{revealed ? value : MASK}</code>
        {secret ? (
          <button type="button" onClick={() => setRevealed((r) => !r)}>
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
