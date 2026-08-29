import { useCallback, useState } from "react";

export const COPIED_RESET_MS = 1500;

// writeClipboard copies text with the async Clipboard API when it's usable
// (a "secure context" — HTTPS or localhost), and falls back to a hidden
// <textarea> + document.execCommand("copy") otherwise. The fallback matters
// because navigator.clipboard is undefined on a plain-http origin such as a
// LAN IP or a reverse proxy without TLS, which is exactly how this UI gets
// reached from another machine. Returns true only if a copy actually
// happened.
async function writeClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Permission denied or blocked — try the execCommand path below.
    }
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    // Keep it off-screen so selecting it doesn't scroll the page.
    ta.style.position = "fixed";
    ta.style.top = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}

// useCopyToClipboard returns [copied, copy]. Calling copy(text) writes text
// to the clipboard and flips copied to true for COPIED_RESET_MS, so callers
// can render a transient "Copied" state on a button.
export function useCopyToClipboard(): [boolean, (text: string) => Promise<void>] {
  const [copied, setCopied] = useState(false);

  const copy = useCallback(async (text: string) => {
    if (!(await writeClipboard(text))) return;
    setCopied(true);
    setTimeout(() => setCopied(false), COPIED_RESET_MS);
  }, []);

  return [copied, copy];
}
