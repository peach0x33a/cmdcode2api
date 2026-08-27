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
  family: string;
  family_label: string;
  variant: string;
  overridden: boolean;
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
  // Present only when allow_lan/allow_tailscale is on and the corresponding
  // IP was detected at startup — see handleAdminConnection.
  lan_base_url?: string;
  tailscale_base_url?: string;
}

export interface DiscordAlertsConfig {
  enabled: boolean;
  webhook_url: string;
  webhook_configured: boolean;
  mention_everyone: boolean;
  hourly_cap: number;
  weekly_cap: number;
  monthly_cap: number;
}

export interface DiscordAlertsUpdate extends DiscordAlertsConfig {
  clear_webhook?: boolean;
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

// Mirrors internal/app/billing.go's BillingSubscription, BillingWindow,
// BillingCreditsResponse, and AccountBilling JSON shapes exactly — keep
// these in sync if their `json:` tags ever change.

export interface BillingSubscription {
  id: string;
  status: string;
  planId: string;
  cancelAtPeriodEnd: boolean;
  currentPeriodStart: string;
  currentPeriodEnd: string;
  endedAt?: string | null;
  cancelAt?: string | null;
  canceledAt?: string | null;
}

export interface BillingWindow {
  used: number;
  cap: number;
  exceeded: boolean;
  resetAt: number;
}

export interface BillingCreditsResponse {
  credits: {
    belowThreshold: boolean;
    creditThreshold: number;
    monthlyCredits: number;
    purchasedCredits: number;
    premiumMonthlyCredits: number;
    opensourceMonthlyCredits: number;
  };
  windowLimits: {
    limited: boolean;
    exceeded: boolean | null;
    fiveHour: BillingWindow;
    weekly: BillingWindow;
  };
}

export interface AccountBilling {
  account: string;
  subscription?: BillingSubscription | null;
  credits?: BillingCreditsResponse | null;
  session_email?: string;
  session_expires_at?: string | null;
  fetched_at?: string | null;
  last_error?: string;
}
