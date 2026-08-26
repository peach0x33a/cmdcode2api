package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// AccountStatus is the health of one Command Code account as observed by the
// background probe. Command Code API keys have no refresh mechanism, so
// "stale" means the account needs a fresh --oauth --account <name> run, not
// that the gateway can recover it on its own.
type AccountStatus string

const (
	StatusUnknown AccountStatus = "unknown"
	StatusHealthy AccountStatus = "healthy"
	StatusStale   AccountStatus = "stale"
	// StatusLimited means the account's key still authenticates, but the most
	// recent real completion request came back as a rate-limit/quota-exhaustion
	// error (Command Code's "you've reached your N-hour usage limit" 429).
	// Unlike StatusStale, this is not a dead key — reauthorizing would not
	// help — so it is surfaced distinctly rather than folded into "healthy"
	// (which was silently hiding it) or "stale" (which would tell the user to
	// reauthorize for no reason).
	StatusLimited AccountStatus = "limited"
)

// healthCheckTimeout bounds a single account probe request.
const healthCheckTimeout = 5 * time.Second

var healthCheckClient = &http.Client{Timeout: healthCheckTimeout}

var errNoEligibleAccounts = errors.New("no healthy command code accounts available")

// timePtr returns a pointer to a copy of t. Used to set lastChecked (a
// *time.Time so JSON omits it entirely until an account has actually been
// probed, rather than serializing the zero value) from time.Now(), whose
// result cannot be addressed directly.
func timePtr(t time.Time) *time.Time {
	return &t
}

type accountState struct {
	Account
	client *CCClient

	mu          sync.Mutex
	status      AccountStatus
	lastChecked *time.Time
	lastError   string
}

// AccountPool round-robins requests across configured Command Code accounts
// and tracks which ones are known to be stale so requests can fail over.
type AccountPool struct {
	mu       sync.Mutex
	accounts []*accountState
	next     int
}

// NewAccountPool builds one CCClient per account. Accounts start in
// StatusUnknown so they are eligible immediately, before the first health
// check has had a chance to run.
func NewAccountPool(accounts []Account) *AccountPool {
	pool := &AccountPool{}
	for _, acct := range accounts {
		pool.accounts = append(pool.accounts, newAccountState(acct))
	}
	return pool
}

// newAccountState builds the CCClient + bookkeeping struct for one account,
// starting it in StatusUnknown. Shared by NewAccountPool and UpdateAccount so
// there is one place that knows how to construct an accountState from an
// Account.
func newAccountState(acct Account) *accountState {
	return &accountState{
		Account: acct,
		client:  NewCCClient(acct.APIKey, acct.BaseURL),
		status:  StatusUnknown,
	}
}

// Len reports the number of configured accounts, regardless of status.
func (p *AccountPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// Next round-robins over accounts that are Healthy, Unknown, or Limited —
// untested accounts get a chance to prove themselves, and rate-limited
// accounts stay in rotation since the limit may already have reset —
// skipping only accounts marked Stale, since a dead key cannot recover on
// its own. A Limited account only clears back to Healthy via an actual
// successful request (see MarkHealthy in sendWithFailover), so it must stay
// eligible here or it would never get the chance to prove the limit lifted.
func (p *AccountPool) Next() (*accountState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.accounts)
	if n == 0 {
		return nil, errNoEligibleAccounts
	}
	for i := 0; i < n; i++ {
		idx := (p.next + i) % n
		acct := p.accounts[idx]
		acct.mu.Lock()
		status := acct.status
		acct.mu.Unlock()
		if status == StatusHealthy || status == StatusUnknown || status == StatusLimited {
			p.next = (idx + 1) % n
			return acct, nil
		}
	}
	return nil, errNoEligibleAccounts
}

// MarkStale flags an account as needing reauthorization. The [WARN] log line
// is the operator-facing signal — there is no way to silently refresh a
// Command Code key, so this always points at the exact command to fix it.
func (p *AccountPool) MarkStale(name, reason string) {
	acct := p.find(name)
	if acct == nil {
		return
	}
	acct.mu.Lock()
	acct.status = StatusStale
	acct.lastError = reason
	acct.lastChecked = timePtr(time.Now())
	acct.mu.Unlock()
	log.Printf("[WARN] account %q marked stale: %s — run --oauth --account %s or POST /accounts/reauth {\"name\":\"%s\"} to reauthorize", name, reason, name, name)
}

// RecordError records the most recent real (non-auth) failure seen for name
// — e.g. a 429 from Command Code's per-window usage limit, a transient 5xx,
// or a network-level failure (timeout, connection reset, DNS error) that
// never even produced an *upstreamAPIError. probeOne only calls the
// lightweight /provider/v1/models endpoint, which does not consume or
// reflect quota, so it can report an account "healthy" even while every
// actual completion is failing with something like "You've reached your
// 5-hour usage limit for your plan." (see cc.go's normalizeUpstreamError).
// sendWithFailover calls this on every such failure so /accounts shows the
// real reason instead of a blank lastError next to a "healthy" status.
//
// If err is specifically a rate-limit/quota-exhaustion signal (an
// *upstreamAPIError normalized to Type "rate_limit_error" — Command Code's
// 429), status is flipped to StatusLimited so the /accounts pill stops
// lying about the account being healthy. Any other kind of error (a
// different upstream error, or a network-level error with no
// *upstreamAPIError at all) leaves status untouched: MarkStale (and its
// "reauthorize" guidance) would be actively wrong advice for a transient 5xx
// or a rate limit that resets on its own, and inventing a distinct status
// for every possible error kind would be noise, not signal.
func (p *AccountPool) RecordError(name string, err error) {
	acct := p.find(name)
	if acct == nil || err == nil {
		return
	}

	var upstreamErr *upstreamAPIError
	isRateLimit := errors.As(err, &upstreamErr) && upstreamErr.Type == "rate_limit_error"

	acct.mu.Lock()
	acct.lastError = err.Error()
	acct.lastChecked = timePtr(time.Now())
	if isRateLimit {
		acct.status = StatusLimited
	}
	acct.mu.Unlock()
}

// MarkHealthy unconditionally marks an account healthy and clears any
// stale/limited status and lastError. It is used both to recover a
// previously-stale account (a successful reauth proves the new key works)
// and, from sendWithFailover, after an actual successful completion request
// — real traffic succeeding is the only thing that can prove a rate limit
// has lifted, so this is intentionally the ONLY path allowed to clear
// StatusLimited. Contrast with probeOne's 200 case (markProbeSucceeded),
// which only proves the unmetered /provider/v1/models endpoint works and
// must NOT clear StatusLimited: do not use that function where this one is
// meant, or vice versa.
func (p *AccountPool) MarkHealthy(name string) {
	acct := p.find(name)
	if acct == nil {
		return
	}
	acct.mu.Lock()
	acct.status = StatusHealthy
	acct.lastError = ""
	acct.lastChecked = timePtr(time.Now())
	acct.mu.Unlock()
}

// markProbeSucceeded is probeOne's 200 handler for /provider/v1/models. That
// endpoint is unmetered and does not exercise completion quota, so a 200
// there is NOT proof a StatusLimited account's quota has recovered — a probe
// succeeding only means the key still authenticates, unlike MarkHealthy
// (called from sendWithFailover), which means an actual completion request
// succeeded. Deliberately named differently from MarkHealthy, and
// deliberately does not clear StatusLimited, so it must never be substituted
// for MarkHealthy or renamed to imply it does. lastChecked is still updated
// so /accounts reflects that a probe ran.
func (p *AccountPool) markProbeSucceeded(name string) {
	acct := p.find(name)
	if acct == nil {
		return
	}
	acct.mu.Lock()
	if acct.status == StatusLimited {
		acct.lastChecked = timePtr(time.Now())
		acct.mu.Unlock()
		return
	}
	acct.status = StatusHealthy
	acct.lastError = ""
	acct.lastChecked = timePtr(time.Now())
	acct.mu.Unlock()
}

// UpdateAccount hot-swaps an account's credentials without a process
// restart. If an account with this name is already in the pool, its
// CCClient is rebuilt from acct and it is marked healthy immediately — a
// successful reauth just proved the new key works, so there is no reason to
// wait for the next background probe. If no account with that name exists
// yet, it is appended as a brand new account; this is what lets
// POST /accounts/reauth double as "add an account" with no restart.
func (p *AccountPool) UpdateAccount(acct Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, existing := range p.accounts {
		if existing.Name == acct.Name {
			existing.mu.Lock()
			existing.Account = acct
			existing.client = NewCCClient(acct.APIKey, acct.BaseURL)
			existing.status = StatusHealthy
			existing.lastError = ""
			existing.lastChecked = timePtr(time.Now())
			existing.mu.Unlock()
			return
		}
	}

	state := newAccountState(acct)
	state.status = StatusHealthy
	state.lastChecked = timePtr(time.Now())
	p.accounts = append(p.accounts, state)
}

// RemoveAccount drops name from the pool entirely, e.g. when the user
// deletes an account from the web UI. It reports whether an account with
// that name existed to remove.
func (p *AccountPool) RemoveAccount(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, acct := range p.accounts {
		if acct.Name == name {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			if p.next > i {
				p.next--
			}
			return true
		}
	}
	return false
}

func (p *AccountPool) find(name string) *accountState {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acct := range p.accounts {
		if acct.Name == name {
			return acct
		}
	}
	return nil
}

// AccountView is the external, credential-free view of an account's status.
type AccountView struct {
	Name        string        `json:"name"`
	Email       string        `json:"email,omitempty"`
	UserID      string        `json:"user_id,omitempty"`
	KeyName     string        `json:"key_name,omitempty"`
	Status      AccountStatus `json:"status"`
	LastChecked *time.Time    `json:"last_checked,omitempty"`
	LastError   string        `json:"last_error,omitempty"`
}

// Snapshot never includes the raw API key — it is metadata for /accounts and
// --list-accounts, not a credential store dump.
func (p *AccountPool) Snapshot() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()

	views := make([]AccountView, 0, len(p.accounts))
	for _, acct := range p.accounts {
		acct.mu.Lock()
		views = append(views, AccountView{
			Name:        acct.Name,
			Email:       acct.Email,
			UserID:      acct.UserID,
			KeyName:     acct.KeyName,
			Status:      acct.status,
			LastChecked: acct.lastChecked,
			LastError:   acct.lastError,
		})
		acct.mu.Unlock()
	}
	return views
}

// ProbeAll synchronously checks every account once and blocks until all
// probes finish. Used both as the immediate check at StartHealthChecks
// startup and as the synchronous pass behind --list-accounts.
func (p *AccountPool) ProbeAll(ctx context.Context) {
	p.mu.Lock()
	accounts := append([]*accountState(nil), p.accounts...)
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, acct := range accounts {
		wg.Add(1)
		go func(a *accountState) {
			defer wg.Done()
			p.probeOne(ctx, a)
		}(acct)
	}
	wg.Wait()
}

// StartHealthChecks runs ProbeAll once immediately, then again on every
// tick, until ctx is cancelled. It returns immediately; probing happens in
// the background so it never delays server startup.
func (p *AccountPool) StartHealthChecks(ctx context.Context, interval time.Duration) {
	go func() {
		p.ProbeAll(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.ProbeAll(ctx)
			}
		}
	}()
}

// probeOne makes the same lightweight authenticated call FetchProviderModels
// uses. A 200 confirms the key still authenticates — it says nothing about
// completion quota, so it deliberately cannot clear a StatusLimited account
// (see markProbeSucceeded); 401/403 confirms it is dead and needs
// reauthorization. Any other failure (timeout, 5xx, network blip) is
// recorded but does not flip status — those are transient upstream problems,
// not proof the key itself is stale, and flipping on them would flap a
// perfectly good account in and out of rotation.
func (p *AccountPool) probeOne(ctx context.Context, acct *accountState) {
	reqCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	// BaseURL/APIKey can be swapped out from under us by UpdateAccount, so
	// copy them under the lock and release before making the network call —
	// the lock must never be held across I/O.
	acct.mu.Lock()
	baseURL := acct.BaseURL
	apiKey := acct.APIKey
	acct.mu.Unlock()

	url := baseURL + "/provider/v1/models"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		acct.mu.Lock()
		acct.lastError = err.Error()
		acct.lastChecked = timePtr(time.Now())
		acct.mu.Unlock()
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := healthCheckClient.Do(req)
	if err != nil {
		acct.mu.Lock()
		acct.lastError = err.Error()
		acct.lastChecked = timePtr(time.Now())
		acct.mu.Unlock()
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		p.markProbeSucceeded(acct.Name)
	case http.StatusUnauthorized, http.StatusForbidden:
		p.MarkStale(acct.Name, fmt.Sprintf("http %d", resp.StatusCode))
	default:
		acct.mu.Lock()
		acct.lastError = fmt.Sprintf("http %d", resp.StatusCode)
		acct.lastChecked = timePtr(time.Now())
		acct.mu.Unlock()
	}
}
