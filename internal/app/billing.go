package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// billingFetchTimeout bounds a single billing/session request.
const billingFetchTimeout = 15 * time.Second

var billingClient = &http.Client{Timeout: billingFetchTimeout}

// sessionCookieName is the cookie commandcode.ai's billing/session API reads
// to authenticate a request, distinct from the Bearer API key used for
// completions.
const sessionCookieName = "__Secure-commandcode_prod_.session_token"

// These are vars, not consts, so tests can point them at an httptest server
// instead of the real commandcode.ai API.
var (
	billingSubscriptionsURL = "https://api.commandcode.ai/internal/billing/subscriptions?withPending=true"
	billingCreditsURL       = "https://api.commandcode.ai/internal/billing/credits"
	sessionInfoURL          = "https://api.commandcode.ai/auth/get-session"
)

// BillingSubscription is the "data" object from the subscriptions endpoint.
// Metadata and PendingPhase are decoded loosely since their shapes aren't
// pinned down by any contract this gateway relies on.
type BillingSubscription struct {
	ID                 string          `json:"id"`
	Status             string          `json:"status"`
	UserID             string          `json:"userId"`
	OrgID              *string         `json:"orgId"`
	CreatedAt          string          `json:"createdAt"`
	PriceID            string          `json:"priceId"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	Quantity           int             `json:"quantity"`
	CancelAtPeriodEnd  bool            `json:"cancelAtPeriodEnd"`
	CurrentPeriodStart string          `json:"currentPeriodStart"`
	CurrentPeriodEnd   string          `json:"currentPeriodEnd"`
	EndedAt            *string         `json:"endedAt"`
	CancelAt           *string         `json:"cancelAt"`
	CanceledAt         *string         `json:"canceledAt"`
	PlanID             string          `json:"planId"`
	PendingPhase       json.RawMessage `json:"pendingPhase,omitempty"`
}

type billingSubscriptionsResponse struct {
	Success bool                 `json:"success"`
	Data    *BillingSubscription `json:"data"`
}

// BillingWindow is one rolling usage window (five-hour or weekly) from the
// credits endpoint.
type BillingWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"`
}

type BillingWindowLimits struct {
	Limited  bool          `json:"limited"`
	Exceeded *bool         `json:"exceeded"`
	FiveHour BillingWindow `json:"fiveHour"`
	Weekly   BillingWindow `json:"weekly"`
}

type BillingCredits struct {
	BelowThreshold           bool    `json:"belowThreshold"`
	CreditThreshold          float64 `json:"creditThreshold"`
	MonthlyCredits           float64 `json:"monthlyCredits"`
	PurchasedCredits         float64 `json:"purchasedCredits"`
	PremiumMonthlyCredits    float64 `json:"premiumMonthlyCredits"`
	OpensourceMonthlyCredits float64 `json:"opensourceMonthlyCredits"`
}

type BillingCreditsResponse struct {
	Credits      BillingCredits      `json:"credits"`
	WindowLimits BillingWindowLimits `json:"windowLimits"`
}

// getSessionResponse is /auth/get-session's body. Only the two fields this
// gateway persists (user.email, session.expiresAt) are decoded — the rest
// (IP, user agent, location, name) is PII with no use here and is
// deliberately dropped by not being decoded at all.
type getSessionResponse struct {
	Session struct {
		ExpiresAt string `json:"expiresAt"`
	} `json:"session"`
	User struct {
		Email string `json:"email"`
	} `json:"user"`
}

// BillingInfo is the most recent billing/session snapshot for one account.
type BillingInfo struct {
	Subscription     *BillingSubscription    `json:"subscription,omitempty"`
	Credits          *BillingCreditsResponse `json:"credits,omitempty"`
	SessionEmail     string                  `json:"session_email,omitempty"`
	SessionExpiresAt *time.Time              `json:"session_expires_at,omitempty"`
	// PlanExpiresAt is the subscription's currentPeriodEnd, parsed. It is
	// refreshed from a successful subscriptions fetch and otherwise carried
	// over from the durable copy in config.yaml, so the plan expiration date
	// remains reportable even after the session token expires and Subscription
	// comes back nil.
	PlanExpiresAt *time.Time `json:"plan_expires_at,omitempty"`
	FetchedAt     *time.Time `json:"fetched_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// AccountBilling is one account's row in a billing report.
type AccountBilling struct {
	Account string `json:"account"`
	BillingInfo
}

// BillingTracker holds the most recently fetched billing/session snapshot
// per account. Unlike UsageTracker it has no on-disk persistence: billing
// data is always freshly refetched on startup and on every tick, so there is
// nothing worth surviving a restart except the session token/identity
// itself, which lives in config.yaml via ConfigStore.
type BillingTracker struct {
	mu       sync.Mutex
	data     map[string]*BillingInfo
	alertsMu sync.RWMutex
	alerts   *DiscordAlerter
}

func NewBillingTracker() *BillingTracker {
	return &BillingTracker{data: make(map[string]*BillingInfo)}
}

func (t *BillingTracker) SetDiscordAlerter(alerts *DiscordAlerter) {
	t.alertsMu.Lock()
	t.alerts = alerts
	t.alertsMu.Unlock()
}

func (t *BillingTracker) set(name string, info *BillingInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[name] = info
}

// Report returns every account's current billing snapshot, sorted by name
// for deterministic output.
func (t *BillingTracker) Report() []AccountBilling {
	t.mu.Lock()
	defer t.mu.Unlock()

	report := make([]AccountBilling, 0, len(t.data))
	for name, info := range t.data {
		report = append(report, AccountBilling{Account: name, BillingInfo: *info})
	}
	sort.Slice(report, func(i, j int) bool { return report[i].Account < report[j].Account })
	return report
}

// RefreshAll re-fetches billing/session info for every configured account
// that has a session token, persisting any rotated cookie or refreshed
// identity through store. Accounts without a token are skipped entirely —
// nothing has been configured for them yet.
func (t *BillingTracker) RefreshAll(ctx context.Context, store *ConfigStore) {
	accounts := store.Accounts()

	var wg sync.WaitGroup
	for _, acct := range accounts {
		if strings.TrimSpace(acct.SessionToken) == "" {
			continue
		}
		wg.Add(1)
		go func(a Account) {
			defer wg.Done()
			t.refreshOne(ctx, store, a)
		}(acct)
	}
	wg.Wait()
	t.alertsMu.RLock()
	alerts := t.alerts
	t.alertsMu.RUnlock()
	if alerts != nil {
		// Webhook delivery has its own short timeout and runs independently so
		// a slow or unavailable Discord endpoint cannot delay billing refresh.
		rows := t.Report()
		go func() {
			t.alertsMu.RLock()
			defer t.alertsMu.RUnlock()
			// Do not evaluate an alerter after it has been replaced or disabled.
			if t.alerts == alerts {
				alerts.Evaluate(rows)
			}
		}()
	}
}

// StartAutoRefresh runs RefreshAll once immediately, then again on every
// tick, until ctx is cancelled. It returns immediately; refreshing happens
// in the background so it never delays server startup.
func (t *BillingTracker) StartAutoRefresh(ctx context.Context, store *ConfigStore, interval time.Duration) {
	go func() {
		t.RefreshAll(ctx, store)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.RefreshAll(ctx, store)
			}
		}
	}()
}

// refreshOne fetches subscriptions, credits, and session identity for acct
// using its currently configured session token, records the result (success
// or error) into the tracker, and persists any rotated cookie or refreshed
// identity back through store.
func (t *BillingTracker) refreshOne(ctx context.Context, store *ConfigStore, acct Account) {
	reqCtx, cancel := context.WithTimeout(ctx, billingFetchTimeout)
	defer cancel()

	token := acct.SessionToken
	info := &BillingInfo{}
	// Start with the durable identity so a partial or malformed session
	// response cannot erase fields learned by an earlier refresh. PlanExpiresAt
	// in particular has to survive a fully failed refresh: once the session
	// token expires every billing call 401s, and this cached date is then the
	// only plan expiration left to report.
	info.SessionEmail = acct.SessionEmail
	info.SessionExpiresAt = acct.SessionExpiresAt
	info.PlanExpiresAt = acct.PlanExpiresAt
	var errs []string
	var latestToken string

	var subsResp billingSubscriptionsResponse
	if refreshed, err := fetchBillingJSON(reqCtx, billingSubscriptionsURL, token, &subsResp); err != nil {
		errs = append(errs, "subscriptions: "+err.Error())
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
	} else {
		info.Subscription = subsResp.Data
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
		if subsResp.Data != nil {
			if end, err := time.Parse(time.RFC3339, subsResp.Data.CurrentPeriodEnd); err == nil {
				info.PlanExpiresAt = &end
			}
		}
	}

	var creditsResp BillingCreditsResponse
	if refreshed, err := fetchBillingJSON(reqCtx, billingCreditsURL, token, &creditsResp); err != nil {
		errs = append(errs, "credits: "+err.Error())
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
	} else {
		info.Credits = &creditsResp
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
	}

	var sessionResp getSessionResponse
	if refreshed, err := fetchBillingJSON(reqCtx, sessionInfoURL, token, &sessionResp); err != nil {
		errs = append(errs, "session: "+err.Error())
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
	} else {
		if refreshed != "" {
			latestToken = refreshed
			token = refreshed
		}
		// Only overwrite the seeded fallback identity with values the response
		// actually carried; a decoded-but-blank get-session must not erase what
		// an earlier refresh learned.
		if sessionResp.User.Email != "" {
			info.SessionEmail = sessionResp.User.Email
		}
		if expiresAt, err := time.Parse(time.RFC3339, sessionResp.Session.ExpiresAt); err == nil {
			info.SessionExpiresAt = &expiresAt
		}
	}

	now := time.Now()
	info.FetchedAt = &now
	info.LastError = strings.Join(errs, "; ")
	t.set(acct.Name, info)

	// One config write covers every durable field (session identity + plan
	// expiry) this refresh learned, and is skipped entirely when nothing
	// changed — see persistDurableIdentity.
	if err := persistDurableIdentity(store, acct, info); err != nil {
		log.Printf("[WARN] account %q: failed to persist session identity: %v", acct.Name, err)
	}

	if latestToken != "" && latestToken != acct.SessionToken {
		if _, err := store.SetSessionToken(acct.Name, latestToken); err != nil {
			log.Printf("[WARN] account %q: failed to persist refreshed billing session token: %v", acct.Name, err)
		}
	}
}

// persistDurableIdentity writes info's session email/expiry and plan expiry
// back through store, but only when at least one of those durable fields
// differs from what acct (and therefore config.yaml) already holds. That guard
// keeps a refresh where every endpoint failed — the expired-token case — from
// rewriting config.yaml on every tick, and keeps a blank get-session response
// from clearing values a previous refresh established.
func persistDurableIdentity(store *ConfigStore, acct Account, info *BillingInfo) error {
	if info.SessionEmail == acct.SessionEmail &&
		timePtrEqual(info.SessionExpiresAt, acct.SessionExpiresAt) &&
		timePtrEqual(info.PlanExpiresAt, acct.PlanExpiresAt) {
		return nil
	}
	_, err := store.SetSessionIdentity(acct.Name, info.SessionEmail, info.SessionExpiresAt, info.PlanExpiresAt)
	return err
}

// timePtrEqual reports whether two optional timestamps denote the same instant,
// treating two nils as equal and a nil/non-nil pair as unequal.
func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// fetchBillingJSON makes a cookie-authenticated GET request to url using
// token, decodes a 200 response body into out, and reports any refreshed
// sessionCookieName value found in the response's Set-Cookie headers
// (returned regardless of whether the call ultimately succeeded, since a
// rotated cookie can arrive alongside an otherwise-unrelated error).
func fetchBillingJSON(ctx context.Context, url, token string, out any) (refreshedToken string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})

	resp, err := billingClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			refreshedToken = c.Value
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return refreshedToken, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return refreshedToken, fmt.Errorf("decode response: %w", err)
	}
	return refreshedToken, nil
}
