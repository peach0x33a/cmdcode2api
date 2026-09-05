package app

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
)

// usageRecorder is what the stream handlers consume so they can record usage
// globally, per upstream account, per client key, or both.
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

	// Both counter maps are guarded by accMu. Account and client-key
	// counters are independent dimensions: an account aggregates across all
	// client keys and vice versa.
	accMu      sync.Mutex
	accounts   map[string]*UsageCounters
	clientKeys map[string]*UsageCounters
}

type UsageCounters struct {
	Requests         atomic.Int64
	PromptTokens     atomic.Int64
	CompletionTokens atomic.Int64
	CacheReadTokens  atomic.Int64
	CacheWriteTokens atomic.Int64
}

func (c *UsageCounters) add(prompt, completion, cacheRead, cacheWrite int) {
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

func (c *UsageCounters) restore(entry UsageSnapshotEntry) {
	c.Requests.Store(entry.Requests)
	c.PromptTokens.Store(entry.PromptTokens)
	c.CompletionTokens.Store(entry.CompletionTokens)
	c.CacheReadTokens.Store(entry.CacheReadTokens)
	c.CacheWriteTokens.Store(entry.CacheWriteTokens)
}

func (c *UsageCounters) snapshot() UsageSnapshotEntry {
	return UsageSnapshotEntry{
		Requests:         c.Requests.Load(),
		PromptTokens:     c.PromptTokens.Load(),
		CompletionTokens: c.CompletionTokens.Load(),
		CacheReadTokens:  c.CacheReadTokens.Load(),
		CacheWriteTokens: c.CacheWriteTokens.Load(),
	}
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
	return u.Recorder(a.ID, "")
}

// RecorderFor is a nil-safe Recorder wrapper taking the served account.
func (u *UsageTracker) RecorderFor(a *Account, clientKeyID string) usageRecorder {
	if a == nil {
		return u.Recorder("", clientKeyID)
	}
	return u.Recorder(a.ID, clientKeyID)
}

// Recorder returns a recorder that mirrors usage into the per-account and
// per-client-key counters for every non-empty ID.
func (u *UsageTracker) Recorder(accountID, clientKeyID string) usageRecorder {
	if accountID == "" && clientKeyID == "" {
		return u
	}
	return &mirrorUsageRecorder{tracker: u, accountID: accountID, clientKeyID: clientKeyID}
}

type mirrorUsageRecorder struct {
	tracker     *UsageTracker
	accountID   string
	clientKeyID string
}

func (r *mirrorUsageRecorder) Record(prompt, completion, cacheRead, cacheWrite int) {
	r.tracker.Record(prompt, completion, cacheRead, cacheWrite)
	if c := r.tracker.accountCounter(r.accountID); c != nil {
		c.add(prompt, completion, cacheRead, cacheWrite)
	}
	if c := r.tracker.clientKeyCounter(r.clientKeyID); c != nil {
		c.add(prompt, completion, cacheRead, cacheWrite)
	}
}

// counter returns the counter for a non-empty id, creating it on first use.
func (u *UsageTracker) counter(m *map[string]*UsageCounters, id string) *UsageCounters {
	if id == "" {
		return nil
	}
	u.accMu.Lock()
	defer u.accMu.Unlock()
	u.ensureMapsLocked()
	target := *m
	if target == nil {
		target = make(map[string]*UsageCounters)
		*m = target
	}
	c, ok := target[id]
	if !ok {
		c = &UsageCounters{}
		target[id] = c
	}
	return c
}

func (u *UsageTracker) accountCounter(id string) *UsageCounters {
	return u.counter(&u.accounts, id)
}

func (u *UsageTracker) clientKeyCounter(id string) *UsageCounters {
	return u.counter(&u.clientKeys, id)
}

func (u *UsageTracker) ensureMapsLocked() {
	if u.accounts == nil {
		u.accounts = make(map[string]*UsageCounters)
	}
	if u.clientKeys == nil {
		u.clientKeys = make(map[string]*UsageCounters)
	}
}

func (u *UsageTracker) countersFor(m map[string]*UsageCounters, id string) UsageSnapshotEntry {
	u.accMu.Lock()
	c := m[id]
	u.accMu.Unlock()
	if c == nil {
		return UsageSnapshotEntry{}
	}
	return c.snapshot()
}

// AccountUsage returns a snapshot of one account's durable counters.
func (u *UsageTracker) AccountUsage(id string) UsageSnapshotEntry {
	return u.countersFor(u.accounts, id)
}

// ClientKeyUsage returns a snapshot of one client key's durable counters.
func (u *UsageTracker) ClientKeyUsage(id string) UsageSnapshotEntry {
	return u.countersFor(u.clientKeys, id)
}

// DropAccount forgets a removed account's counters so they stop persisting.
func (u *UsageTracker) DropAccount(id string) {
	u.accMu.Lock()
	defer u.accMu.Unlock()
	delete(u.accounts, id)
}

// MoveAccount migrates counters when an account's key (and therefore ID)
// changes. Counters are merged if the target already has any.
func (u *UsageTracker) MoveAccount(oldID, newID string) {
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	u.accMu.Lock()
	defer u.accMu.Unlock()
	u.ensureMapsLocked()
	old := u.accounts[oldID]
	if old == nil {
		return
	}
	delete(u.accounts, oldID)
	target := u.accounts[newID]
	if target == nil {
		u.accounts[newID] = old
		return
	}
	target.Requests.Add(old.Requests.Load())
	target.PromptTokens.Add(old.PromptTokens.Load())
	target.CompletionTokens.Add(old.CompletionTokens.Load())
	target.CacheReadTokens.Add(old.CacheReadTokens.Load())
	target.CacheWriteTokens.Add(old.CacheWriteTokens.Load())
}

// DropClientKey forgets a removed client key's counters.
func (u *UsageTracker) DropClientKey(id string) {
	u.accMu.Lock()
	defer u.accMu.Unlock()
	delete(u.clientKeys, id)
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
		snap.Accounts = make(map[string]UsageSnapshotEntry, len(u.accounts))
		for id, c := range u.accounts {
			snap.Accounts[id] = c.snapshot()
		}
	}
	if len(u.clientKeys) > 0 {
		snap.ClientKeys = make(map[string]UsageSnapshotEntry, len(u.clientKeys))
		for id, c := range u.clientKeys {
			snap.ClientKeys[id] = c.snapshot()
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

	Accounts   map[string]UsageSnapshotEntry `json:"accounts,omitempty"`
	ClientKeys map[string]UsageSnapshotEntry `json:"client_keys,omitempty"`
}

type UsageSnapshotEntry struct {
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

	u.accMu.Lock()
	u.ensureMapsLocked()
	for id, entry := range snap.Accounts {
		c := &UsageCounters{}
		c.restore(entry)
		u.accounts[id] = c
	}
	for id, entry := range snap.ClientKeys {
		c := &UsageCounters{}
		c.restore(entry)
		u.clientKeys[id] = c
	}
	u.accMu.Unlock()
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
