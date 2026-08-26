import type {
  AccountUsage,
  AccountView,
  AdminModel,
  ApiErrorBody,
  ConnectionInfo,
  ReauthSession,
  UsageReport,
} from "./types";

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
    typeof value.api_key === "string"
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

async function jsonFetch(url: string, options?: RequestInit): Promise<unknown> {
  const res = await fetch(url, options);
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

export async function fetchConnection(): Promise<ConnectionInfo> {
  return assertConnectionInfo(await jsonFetch("/admin/connection"));
}

export async function fetchUsage(): Promise<UsageReport> {
  return assertUsageReport(await jsonFetch("/admin/usage"));
}
