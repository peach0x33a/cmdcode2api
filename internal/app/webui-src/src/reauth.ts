import type { ReauthSession, ReauthStatus } from "./types";

// The set of ReauthStatus values that mean "stop polling" — defined once,
// typed against the real ReauthStatus union (from types.ts, which mirrors
// internal/app/reauth.go's ReauthStatus exactly) so that if the backend ever
// grows a new status and this set isn't updated, it's a compile error here
// rather than a silently-stuck polling loop. There is no "expired" status:
// the backend resolves an expired-while-pending session to "error" (see
// ReauthManager.Get), so "error" alone covers it.
export const TERMINAL_REAUTH_STATUSES: Set<ReauthStatus> = new Set(["success", "error"]);

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// errorSession builds the placeholder ReauthSession used to surface a local
// (client-side) failure — e.g. a failed fetch — in the same shape as a real
// session from the backend, so AccountRow can render it uniformly.
export function errorSession(name: string, err: unknown): ReauthSession {
  return {
    name,
    status: "error",
    error: errorMessage(err),
    auth_url: "",
    started_at: "",
    expires_at: "",
  };
}
