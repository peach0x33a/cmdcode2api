package app

import "sync"

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
