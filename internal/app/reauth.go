package app

import (
	"sync"
	"time"
)

// ReauthStatus is the lifecycle of one async reauth attempt started via
// POST /accounts/reauth.
type ReauthStatus string

const (
	ReauthPending ReauthStatus = "pending"
	ReauthSuccess ReauthStatus = "success"
	ReauthError   ReauthStatus = "error"
)

// ReauthSession tracks one in-flight (or resolved) reauthorization for a
// single account. It is also the JSON shape returned by
// POST/GET /accounts/reauth. Status and ErrorMsg are mutated from the
// background goroutine that waits on the OAuth callback, so reads/writes of
// those two fields go through mu; the other fields are set once at creation
// and never change.
type ReauthSession struct {
	AccountName string    `json:"name"`
	AuthURL     string    `json:"auth_url"`
	StartedAt   time.Time `json:"started_at"`
	ExpiresAt   time.Time `json:"expires_at"`

	mu       sync.Mutex
	Status   ReauthStatus `json:"status"`
	ErrorMsg string       `json:"error,omitempty"`
	// cancelled is set by ReauthManager.Forget when the account this session
	// belongs to is deleted. The background goroutine in Start checks it
	// right before calling onSuccess so a delete that races a nearly-finished
	// OAuth callback can't resurrect the account by re-persisting it after
	// Forget already dropped the session.
	cancelled bool
}

// Snapshot returns a value copy of the session's exported fields, safe to
// read or JSON-encode without racing the goroutine that resolves it.
func (s *ReauthSession) Snapshot() ReauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ReauthSession{
		AccountName: s.AccountName,
		AuthURL:     s.AuthURL,
		StartedAt:   s.StartedAt,
		ExpiresAt:   s.ExpiresAt,
		Status:      s.Status,
		ErrorMsg:    s.ErrorMsg,
	}
}

// ReauthManager starts and tracks async, HTTP-triggered OAuth reauths — the
// foundation for a future web UI's "Reauthorize" button, and the same
// mechanism POST /accounts/reauth uses today. Unlike --oauth --account
// <name>, starting a session never blocks the caller or the running server:
// the OAuth listener runs in the background and onSuccess is invoked once
// the browser completes the flow.
type ReauthManager struct {
	mu        sync.Mutex
	sessions  map[string]*ReauthSession
	onSuccess func(name string, cb oauthCallback)
}

func NewReauthManager(onSuccess func(name string, cb oauthCallback)) *ReauthManager {
	return &ReauthManager{
		sessions:  make(map[string]*ReauthSession),
		onSuccess: onSuccess,
	}
}

// Start begins reauthorizing name. If a session for name is already pending
// and not yet expired, that same session is returned instead of starting a
// second competing OAuth listener — this makes Start safe to call more than
// once in a row, e.g. from a future web UI's Reauthorize button being
// double-clicked.
func (m *ReauthManager) Start(name string) (*ReauthSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.sessions[name]; ok {
		existing.mu.Lock()
		stillPending := existing.Status == ReauthPending && time.Now().Before(existing.ExpiresAt)
		existing.mu.Unlock()
		if stillPending {
			return existing, nil
		}
	}

	l, err := startOAuthListener(OAuthOptions{})
	if err != nil {
		return nil, err
	}

	session := &ReauthSession{
		AccountName: name,
		AuthURL:     l.AuthURL,
		StartedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(oauthTimeout),
		Status:      ReauthPending,
	}
	m.sessions[name] = session

	go func() {
		cb, err := l.WaitCallback()
		if err != nil {
			session.mu.Lock()
			if !session.cancelled {
				session.Status = ReauthError
				session.ErrorMsg = err.Error()
			}
			session.mu.Unlock()
			return
		}

		session.mu.Lock()
		cancelled := session.cancelled
		if !cancelled {
			session.Status = ReauthSuccess
		}
		session.mu.Unlock()
		// The account may have been deleted (via /accounts/delete ->
		// Forget) while this OAuth flow was still in flight. If so, do not
		// call onSuccess — that would re-persist and re-add the very
		// account the user just deleted.
		if cancelled {
			return
		}
		if m.onSuccess != nil {
			m.onSuccess(name, cb)
		}
	}()

	return session, nil
}

// Get returns the current session for name, for polling from
// GET /accounts/reauth. Sessions are kept in the map after they resolve —
// there's no need for expiry sweeping in a single local process.
//
// If the session is still pending but its ExpiresAt has passed — the user
// never completed the browser OAuth flow in time — it is flipped to
// ReauthError here so a polling client doesn't sit at "pending" forever.
// There is no separate "expired" status: an expired session is just an
// error outcome with an explanatory message.
func (m *ReauthManager) Get(name string) (*ReauthSession, bool) {
	m.mu.Lock()
	s, ok := m.sessions[name]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}

	s.mu.Lock()
	if s.Status == ReauthPending && time.Now().After(s.ExpiresAt) {
		s.Status = ReauthError
		s.ErrorMsg = "authorization link expired, please try again"
	}
	s.mu.Unlock()

	return s, true
}

// Forget drops any tracked reauth session for name. Called when an account
// is deleted so a later add-back under the same name doesn't pick up a stale
// session (e.g. an old AuthURL that a lingering polling client might still
// be hitting). It also marks the session cancelled so that if its
// background goroutine is already past WaitCallback and about to call
// onSuccess, it skips doing so instead of re-persisting a deleted account.
func (m *ReauthManager) Forget(name string) {
	m.mu.Lock()
	s, ok := m.sessions[name]
	delete(m.sessions, name)
	m.mu.Unlock()

	if ok {
		s.mu.Lock()
		s.cancelled = true
		s.mu.Unlock()
	}
}
