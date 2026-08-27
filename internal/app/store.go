package app

import (
	"sync"
	"time"
)

// ConfigStore is the single writer for the on-disk config file once the
// server is running. Both --oauth-driven CLI flows and the HTTP reauth/UI
// flows funnel through it so config.yaml is never written from two
// goroutines at once.
type ConfigStore struct {
	mu   sync.Mutex
	path string
	cfg  *Config
}

// NewConfigStore wraps cfg (already loaded from path) for serialized
// read/modify/write access. cfg must not be mutated by callers outside the
// store once this is constructed — go through Upsert/Remove instead.
func NewConfigStore(path string, cfg *Config) *ConfigStore {
	return &ConfigStore{path: path, cfg: cfg}
}

// Upsert adds or replaces the account with acct.Name and persists the
// result to disk.
func (s *ConfigStore) Upsert(acct Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	upsertAccount(s.cfg, acct)
	return saveConfig(s.path, s.cfg)
}

// Remove drops the account named name and persists the result to disk. It
// reports whether an account with that name existed to remove.
func (s *ConfigStore) Remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i, acct := range s.cfg.Accounts {
		if acct.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil
	}
	s.cfg.Accounts = append(s.cfg.Accounts[:idx], s.cfg.Accounts[idx+1:]...)
	if err := saveConfig(s.path, s.cfg); err != nil {
		return false, err
	}
	return true, nil
}

// SetSessionToken updates the billing session token for the account named
// name and persists it, without touching any other account field. It
// reports whether an account with that name exists.
func (s *ConfigStore) SetSessionToken(name, token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.cfg.Accounts {
		if s.cfg.Accounts[i].Name == name {
			s.cfg.Accounts[i].SessionToken = token
			return true, saveConfig(s.path, s.cfg)
		}
	}
	return false, nil
}

// SetSessionIdentity updates the billing session's identity metadata
// (SessionEmail/SessionExpiresAt, from a successful /auth/get-session fetch)
// for the account named name and persists it. It reports whether an account
// with that name exists.
func (s *ConfigStore) SetSessionIdentity(name, email string, expiresAt *time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.cfg.Accounts {
		if s.cfg.Accounts[i].Name == name {
			s.cfg.Accounts[i].SessionEmail = email
			s.cfg.Accounts[i].SessionExpiresAt = expiresAt
			return true, saveConfig(s.path, s.cfg)
		}
	}
	return false, nil
}

// SetModelOverrides replaces the config's model_overrides map wholesale with
// overrides and persists it. Callers (ModelPolicy.SetModelOverrides) pass
// the already-merged map, so this is a plain overwrite-and-save — mirroring
// SetSessionToken's mutate-then-save pattern above.
func (s *ConfigStore) SetModelOverrides(overrides map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg.ModelOverrides = overrides
	return saveConfig(s.path, s.cfg)
}

// SetDiscordAlerts replaces the alert configuration and persists it.
func (s *ConfigStore) SetDiscordAlerts(alerts DiscordAlertConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.DiscordAlerts = alerts
	// Clear the pre-discord_alerts field in the same serialized update. Leaving
	// it behind lets startup logic resurrect a disabled legacy configuration.
	s.cfg.DiscordWebhookURL = ""
	return saveConfig(s.path, s.cfg)
}

// DiscordAlerts returns snapshots of the current alert configuration and the
// legacy top-level webhook, while holding the store lock for both reads.
func (s *ConfigStore) DiscordAlerts() (DiscordAlertConfig, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.DiscordAlerts, s.cfg.DiscordWebhookURL
}

// Accounts returns a defensive copy of the current account list, so callers
// like BillingTracker's refresh loop always see the live, current accounts
// (including tokens set/updated after startup) without duplicating config
// state elsewhere.
func (s *ConfigStore) Accounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Account(nil), s.cfg.Accounts...)
}

// Snapshot returns the base URL configured for name, or defaultBaseURL if
// the account isn't known yet (e.g. a brand new "Add account" name that
// hasn't been persisted). Used so a reauth for an existing account keeps
// its configured base_url instead of silently resetting it.
func (s *ConfigStore) BaseURLFor(name, defaultBaseURL string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, acct := range s.cfg.Accounts {
		if acct.Name == name {
			return acct.BaseURL
		}
	}
	return defaultBaseURL
}
