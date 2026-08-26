import { useCallback, useState } from "react";

export const COPIED_RESET_MS = 1500;

// useCopyToClipboard returns [copied, copy]. Calling copy(text) writes text
// to the clipboard and flips copied to true for COPIED_RESET_MS, so callers
// can render a transient "Copied" state on a button.
export function useCopyToClipboard(): [boolean, (text: string) => Promise<void>] {
  const [copied, setCopied] = useState(false);

  const copy = useCallback(async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), COPIED_RESET_MS);
    } catch {
      // clipboard access denied/unavailable — nothing useful to do here
    }
  }, []);

  return [copied, copy];
}
