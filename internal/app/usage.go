package app

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
)

// usageRecorder is what the stream handlers consume so they can record usage
// either globally or bound to the account that served the request.
type usageRecorder interface {
	Record(prompt, completion, cacheRead, cacheWrite int)
}

type UsageTracker struct {
	TotalRequests    atomic.Int64 `json:"total_requests"`
	PromptTokens     atomic.Int64 `json:"prompt_tokens"`
	CompletionTokens atomic.Int64 `json:"completion_tokens"`
	CacheReadTokens  atomic.Int64 `json:"cache_read_tokens"`
	CacheWriteTokens atomic.Int64 `json:"cache_write_tokens"`
	saveMu           sync.Mutex

	accMu    sync.Mutex
	accounts map[string]*AccountUsageCounters
}

type AccountUsageCounters struct {
	Requests         atomic.Int64
	PromptTokens     atomic.Int64
	CompletionTokens atomic.Int64
	CacheReadTokens  atomic.Int64
	CacheWriteTokens atomic.Int64
}

func (u *UsageTracker) Record(prompt, completion, cacheRead, cacheWrite int) {
	u.TotalRequests.Add(1)
	u.PromptTokens.Add(int64(prompt))
	u.CompletionTokens.Add(int64(completion))
	if cacheRead > 0 {
		u.CacheReadTokens.Add(int64(cacheRead))
	}
	if cacheWrite > 0 {
		u.CacheWriteTokens.Add(int64(cacheWrite))
	}
}

// ForAccount returns a recorder that mirrors usage into the per-account
// counters. A nil account records globally only.
func (u *UsageTracker) ForAccount(a *Account) usageRecorder {
	if a == nil {
		return u
	}
	return &accountUsageRecorder{tracker: u, accountID: a.ID}
}

type accountUsageRecorder struct {
	tracker   *UsageTracker
	accountID string
}

func (r *accountUsageRecorder) Record(prompt, completion, cacheRead, cacheWrite int) {
	r.tracker.Record(prompt, completion, cacheRead, cacheWrite)
	c := r.tracker.accountCounters(r.accountID)
	c.Requests.Add(1)
	c.PromptTokens.Add(int64(prompt))
	c.CompletionTokens.Add(int64(completion))
	if cacheRead > 0 {
		c.CacheReadTokens.Add(int64(cacheRead))
	}
	if cacheWrite > 0 {
		c.CacheWriteTokens.Add(int64(cacheWrite))
	}
}

func (u *UsageTracker) accountCounters(id string) *AccountUsageCounters {
	u.accMu.Lock()
	defer u.accMu.Unlock()
	if u.accounts == nil {
		u.accounts = make(map[string]*AccountUsageCounters)
	}
	c, ok := u.accounts[id]
	if !ok {
		c = &AccountUsageCounters{}
		u.accounts[id] = c
	}
	return c
}

// AccountUsage returns a snapshot of one account's durable counters.
func (u *UsageTracker) AccountUsage(id string) AccountUsageSnapshot {
	u.accMu.Lock()
	c := u.accounts[id]
	u.accMu.Unlock()
	snap := AccountUsageSnapshot{}
	if c != nil {
		snap = AccountUsageSnapshot{
			Requests:         c.Requests.Load(),
			PromptTokens:     c.PromptTokens.Load(),
			CompletionTokens: c.CompletionTokens.Load(),
			CacheReadTokens:  c.CacheReadTokens.Load(),
			CacheWriteTokens: c.CacheWriteTokens.Load(),
		}
	}
	return snap
}

// DropAccount forgets a removed account's counters so they stop persisting.
func (u *UsageTracker) DropAccount(id string) {
	u.accMu.Lock()
	defer u.accMu.Unlock()
	delete(u.accounts, id)
}

func (u *UsageTracker) Snapshot() UsageSnapshot {
	snap := UsageSnapshot{
		TotalRequests:    u.TotalRequests.Load(),
		PromptTokens:     u.PromptTokens.Load(),
		CompletionTokens: u.CompletionTokens.Load(),
		CacheReadTokens:  u.CacheReadTokens.Load(),
		CacheWriteTokens: u.CacheWriteTokens.Load(),
	}
	u.accMu.Lock()
	defer u.accMu.Unlock()
	if len(u.accounts) > 0 {
		snap.Accounts = make(map[string]AccountUsageSnapshot, len(u.accounts))
		for id, c := range u.accounts {
			snap.Accounts[id] = AccountUsageSnapshot{
				Requests:         c.Requests.Load(),
				PromptTokens:     c.PromptTokens.Load(),
				CompletionTokens: c.CompletionTokens.Load(),
				CacheReadTokens:  c.CacheReadTokens.Load(),
				CacheWriteTokens: c.CacheWriteTokens.Load(),
			}
		}
	}
	return snap
}

type UsageSnapshot struct {
	TotalRequests    int64 `json:"total_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`

	Accounts map[string]AccountUsageSnapshot `json:"accounts,omitempty"`
}

type AccountUsageSnapshot struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// TotalTokens returns prompt + completion (not counting cache separately)
func (s UsageSnapshot) TotalTokens() int64 {
	return s.PromptTokens + s.CompletionTokens
}

// ====== persistence ======

// usageFile is a var so tests can redirect persistence to a temp dir.
var usageFile = "usage.json"

func loadUsage() *UsageTracker {
	u := &UsageTracker{}
	data, err := os.ReadFile(usageFile)
	if err != nil {
		return u
	}
	var snap UsageSnapshot
	if json.Unmarshal(data, &snap) != nil {
		return u
	}
	u.TotalRequests.Store(snap.TotalRequests)
	u.PromptTokens.Store(snap.PromptTokens)
	u.CompletionTokens.Store(snap.CompletionTokens)
	u.CacheReadTokens.Store(snap.CacheReadTokens)
	u.CacheWriteTokens.Store(snap.CacheWriteTokens)
	for id, acc := range snap.Accounts {
		c := u.accountCounters(id)
		c.Requests.Store(acc.Requests)
		c.PromptTokens.Store(acc.PromptTokens)
		c.CompletionTokens.Store(acc.CompletionTokens)
		c.CacheReadTokens.Store(acc.CacheReadTokens)
		c.CacheWriteTokens.Store(acc.CacheWriteTokens)
	}
	return u
}

func (u *UsageTracker) save() error {
	u.saveMu.Lock()
	defer u.saveMu.Unlock()

	snap := u.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := usageFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, usageFile)
}
