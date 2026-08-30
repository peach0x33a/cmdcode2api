import { getStoredKey } from "./auth";
import type {
  AccountBilling,
  AccountUsage,
  AccountView,
  AdminModel,
  ApiErrorBody,
  ConnectionInfo,
  ReauthSession,
  UsageReport,
  DiscordAlertsConfig,
  DiscordAlertsUpdate,
} from "./types";

// UnauthorizedError distinguishes a 401 (missing/wrong API key) from any
// other request failure, so AuthGate can show a login prompt instead of a
// generic error banner.
export class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
    this.name = "UnauthorizedError";
  }
}

function assertDiscordAlerts(value: unknown): DiscordAlertsConfig {
  if (!isRecord(value) || typeof value.enabled !== "boolean" || typeof value.webhook_url !== "string" || typeof value.mention_everyone !== "boolean" || typeof value.hourly_cap !== "number" || typeof value.weekly_cap !== "number" || typeof value.monthly_cap !== "number") throw new Error("unexpected response shape: expected alert settings");
  return value as unknown as DiscordAlertsConfig;
}

export async function fetchDiscordAlerts(): Promise<DiscordAlertsConfig> { return assertDiscordAlerts(await jsonFetch("/admin/alerts")); }
export async function saveDiscordAlerts(config: DiscordAlertsUpdate): Promise<DiscordAlertsConfig> {
  return assertDiscordAlerts(await jsonFetch("/admin/alerts", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(config) }));
}

// isRecord narrows unknown to a non-null, non-array object — the shared
// precondition for reading any expected field off a parsed JSON body.
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// isAccountView does a minimal, non-exhaustive shape check against
// AccountView (see types.ts): enough to catch a wrong endpoint or a backend
// contract change, not a full schema validator.
function isAccountView(value: unknown): value is AccountView {
  return isRecord(value) && typeof value.name === "string" && typeof value.status === "string";
}

function assertAccountViewArray(value: unknown): AccountView[] {
  if (!Array.isArray(value) || !value.every(isAccountView)) {
    throw new Error("unexpected response shape: expected an array of accounts");
  }
  return value;
}

// isReauthSession does a minimal shape check against ReauthSession (see
// types.ts) — just the fields callers actually read before anything else.
function isReauthSession(value: unknown): value is ReauthSession {
  return (
    isRecord(value) &&
    typeof value.name === "string" &&
    typeof value.auth_url === "string" &&
    typeof value.status === "string"
  );
}

function assertReauthSession(value: unknown): ReauthSession {
  if (!isReauthSession(value)) {
    throw new Error("unexpected response shape: expected a reauth session");
  }
  return value;
}

// isAdminModel does a minimal shape check against AdminModel (see types.ts).
function isAdminModel(value: unknown): value is AdminModel {
  return isRecord(value) && typeof value.id === "string" && typeof value.excluded === "boolean";
}

function assertAdminModelList(value: unknown): AdminModel[] {
  if (!isRecord(value) || !Array.isArray(value.data) || !value.data.every(isAdminModel)) {
    throw new Error("unexpected response shape: expected a model list");
  }
  return value.data;
}

// isConnectionInfo does a minimal shape check against ConnectionInfo (see
// types.ts).
function isConnectionInfo(value: unknown): value is ConnectionInfo {
  return (
    isRecord(value) &&
    typeof value.base_url === "string" &&
    typeof value.host === "string" &&
    typeof value.port === "number" &&
    typeof value.api_key === "string" &&
    (value.lan_base_url === undefined || typeof value.lan_base_url === "string") &&
    (value.tailscale_base_url === undefined || typeof value.tailscale_base_url === "string")
  );
}

function assertConnectionInfo(value: unknown): ConnectionInfo {
  if (!isConnectionInfo(value)) {
    throw new Error("unexpected response shape: expected connection info");
  }
  return value;
}

// isAccountUsage does a minimal shape check against AccountUsage (see
// types.ts).
function isAccountUsage(value: unknown): value is AccountUsage {
  return (
    isRecord(value) && typeof value.account === "string" && typeof value.total_requests === "number"
  );
}

// isUsageReport does a minimal shape check against UsageReport (see
// types.ts).
function isUsageReport(value: unknown): value is UsageReport {
  return (
    isRecord(value) &&
    typeof value.total_requests === "number" &&
    (value.accounts === undefined || (Array.isArray(value.accounts) && value.accounts.every(isAccountUsage)))
  );
}

function assertUsageReport(value: unknown): UsageReport {
  if (!isUsageReport(value)) {
    throw new Error("unexpected response shape: expected a usage report");
  }
  return value;
}

// isAccountBilling does a minimal shape check against AccountBilling (see
// types.ts).
function isAccountBilling(value: unknown): value is AccountBilling {
  return isRecord(value) && typeof value.account === "string";
}

function assertAccountBillingArray(value: unknown): AccountBilling[] {
  if (!Array.isArray(value) || !value.every(isAccountBilling)) {
    throw new Error("unexpected response shape: expected an array of account billing info");
  }
  return value;
}

function assertAccountBilling(value: unknown): AccountBilling {
  if (!isAccountBilling(value)) {
    throw new Error("unexpected response shape: expected account billing info");
  }
  return value;
}

async function jsonFetch(url: string, options?: RequestInit): Promise<unknown> {
  // On loopback, the server's own tokenless bypass makes this header
  // unnecessary; from another device (LAN/Tailscale/anywhere else) it's
  // required, so attach it whenever a key has been stored — see AuthGate
  // and auth.ts for where that key comes from.
  const storedKey = getStoredKey();
  const headers = new Headers(options?.headers);
  if (storedKey && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${storedKey}`);
  }

  const res = await fetch(url, { ...options, headers });
  if (res.status === 401) {
    throw new UnauthorizedError();
  }
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    // no/invalid JSON body — fall through with body = null
  }
  if (!res.ok) {
    const errBody = isRecord(body) ? (body as ApiErrorBody) : null;
    const message =
      errBody?.error?.message || errBody?.message || `${res.status} ${res.statusText}`;
    throw new Error(message);
  }
  return body;
}

export async function fetchAccounts(): Promise<AccountView[]> {
  return assertAccountViewArray(await jsonFetch("/accounts"));
}

export async function probeAccounts(): Promise<AccountView[]> {
  return assertAccountViewArray(
    await jsonFetch("/accounts/probe", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    })
  );
}

export async function startReauth(name: string): Promise<ReauthSession> {
  return assertReauthSession(
    await jsonFetch("/accounts/reauth", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    })
  );
}

export async function pollReauth(name: string): Promise<ReauthSession> {
  return assertReauthSession(await jsonFetch("/accounts/reauth?name=" + encodeURIComponent(name)));
}

export function deleteAccount(name: string): Promise<unknown> {
  return jsonFetch("/accounts/delete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
}

export async function fetchModels(): Promise<AdminModel[]> {
  return assertAdminModelList(await jsonFetch("/admin/models"));
}

// setModelOverrides applies an explicit per-model enabled/disabled override
// for every id in ids, returning the fresh full model list (POST
// /admin/models/toggle with a "models" body).
export async function setModelOverrides(ids: string[], enabled: boolean): Promise<AdminModel[]> {
  return assertAdminModelList(
    await jsonFetch("/admin/models/toggle", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ models: ids, enabled }),
    })
  );
}

// setFamilyOverride asks the server to expand family to current catalog
// children and persist exact per-model overrides.
export async function setFamilyOverride(family: string, enabled: boolean): Promise<AdminModel[]> {
  return assertAdminModelList(
    await jsonFetch("/admin/models/toggle", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ family, enabled }),
    })
  );
}

export async function fetchConnection(): Promise<ConnectionInfo> {
  return assertConnectionInfo(await jsonFetch("/admin/connection"));
}

// fetchVersion reads the build tag from the unauthenticated /health endpoint
// (see internal/app/server.go). Returns "" when the field is missing or the
// request fails, so the footer can just fall back to showing nothing.
export async function fetchVersion(): Promise<string> {
  try {
    const body = await jsonFetch("/health");
    return isRecord(body) && typeof body.version === "string" ? body.version : "";
  } catch {
    return "";
  }
}

export async function fetchUsage(): Promise<UsageReport> {
  return assertUsageReport(await jsonFetch("/admin/usage"));
}

export async function fetchBilling(): Promise<AccountBilling[]> {
  return assertAccountBillingArray(await jsonFetch("/admin/billing"));
}

export async function saveBillingToken(name: string, sessionToken: string): Promise<AccountBilling> {
  return assertAccountBilling(
    await jsonFetch("/accounts/billing-token", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, session_token: sessionToken }),
    })
  );
}
