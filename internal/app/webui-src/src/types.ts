// Mirrors internal/app/accounts.go's AccountView and internal/app/reauth.go's
// ReauthSession JSON shapes exactly — keep these in sync with the Go structs
// if their `json:` tags ever change.

export type AccountStatus = "unknown" | "healthy" | "stale" | "limited";

export interface AccountView {
  name: string;
  email?: string;
  user_id?: string;
  key_name?: string;
  status: AccountStatus;
  last_checked?: string | null; // RFC 3339, omitted/null until the account has been probed at least once
  last_error?: string;
}

export type ReauthStatus = "pending" | "success" | "error";

export interface ReauthSession {
  name: string;
  auth_url: string;
  started_at: string;
  expires_at: string;
  status: ReauthStatus;
  error?: string;
}

// The Go backend's error envelope, e.g. { "error": { "message": "..." } }.
export interface ApiErrorBody {
  error?: { message?: string };
  message?: string;
}

// Mirrors internal/app/types.go's AdminModel, AdminModelList, and
// ConnectionInfo JSON shapes exactly — keep these in sync if their `json:`
// tags ever change.

export interface AdminModel {
  id: string;
  name: string;
  context_length: number;
  owned_by: string;
  excluded: boolean;
}

export interface AdminModelList {
  object: string;
  data: AdminModel[];
}

export interface ConnectionInfo {
  base_url: string;
  host: string;
  port: number;
  api_key: string;
}

// Mirrors internal/app/usage.go's AccountUsage and UsageReport (embedding
// UsageSnapshot) JSON shapes exactly — keep these in sync if their `json:`
// tags ever change.

export interface AccountUsage {
  account: string;
  total_requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface UsageReport {
  total_requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  accounts?: AccountUsage[];
}
