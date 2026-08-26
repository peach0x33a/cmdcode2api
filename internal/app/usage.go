package app

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

type UsageTracker struct {
	TotalRequests    atomic.Int64 `json:"total_requests"`
	PromptTokens     atomic.Int64 `json:"prompt_tokens"`
	CompletionTokens atomic.Int64 `json:"completion_tokens"`
	CacheReadTokens  atomic.Int64 `json:"cache_read_tokens"`
	CacheWriteTokens atomic.Int64 `json:"cache_write_tokens"`
	saveMu           sync.Mutex

	perAccountMu sync.Mutex
	perAccount   map[string]*accountUsage
}

// accountUsage holds per-account counters. Unlike UsageTracker's global
// fields these are plain int64s guarded entirely by perAccountMu rather than
// atomics, since the write path runs once per completed request and there is
// no benefit to lock-free counters here.
type accountUsage struct {
	TotalRequests    int64
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
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

// RecordFor records usage for a specific account in addition to the global
// counters. account == "" (e.g. callers that predate per-account tracking,
// or contexts with no known serving account) updates only the globals,
// making RecordFor("", ...) behaviorally identical to Record(...).
func (u *UsageTracker) RecordFor(account string, prompt, completion, cacheRead, cacheWrite int) {
	u.Record(prompt, completion, cacheRead, cacheWrite)
	if account == "" {
		return
	}

	u.perAccountMu.Lock()
	defer u.perAccountMu.Unlock()
	if u.perAccount == nil {
		u.perAccount = make(map[string]*accountUsage)
	}
	entry, ok := u.perAccount[account]
	if !ok {
		entry = &accountUsage{}
		u.perAccount[account] = entry
	}
	entry.TotalRequests++
	entry.PromptTokens += int64(prompt)
	entry.CompletionTokens += int64(completion)
	if cacheRead > 0 {
		entry.CacheReadTokens += int64(cacheRead)
	}
	if cacheWrite > 0 {
		entry.CacheWriteTokens += int64(cacheWrite)
	}
}

func (u *UsageTracker) Snapshot() UsageSnapshot {
	return UsageSnapshot{
		TotalRequests:    u.TotalRequests.Load(),
		PromptTokens:     u.PromptTokens.Load(),
		CompletionTokens: u.CompletionTokens.Load(),
		CacheReadTokens:  u.CacheReadTokens.Load(),
		CacheWriteTokens: u.CacheWriteTokens.Load(),
	}
}

type UsageSnapshot struct {
	TotalRequests    int64 `json:"total_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// TotalTokens returns prompt + completion (not counting cache separately)
func (s UsageSnapshot) TotalTokens() int64 {
	return s.PromptTokens + s.CompletionTokens
}

// AccountUsage is a single account's row in a UsageReport.
type AccountUsage struct {
	Account          string `json:"account"`
	TotalRequests    int64  `json:"total_requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
}

// UsageReport combines the global UsageSnapshot with a per-account
// breakdown. UsageSnapshot's global totals may exceed the sum of Accounts
// rows, because usage recorded before per-account tracking existed (or with
// an empty account name) only increments the globals — callers must not
// compute totals by summing the per-account rows.
type UsageReport struct {
	UsageSnapshot
	Accounts []AccountUsage `json:"accounts,omitempty"`
}

// Report builds a UsageReport from the current global and per-account
// counters. Accounts are sorted by name for deterministic output.
func (u *UsageTracker) Report() UsageReport {
	report := UsageReport{UsageSnapshot: u.Snapshot()}

	u.perAccountMu.Lock()
	if len(u.perAccount) > 0 {
		report.Accounts = make([]AccountUsage, 0, len(u.perAccount))
		for account, entry := range u.perAccount {
			report.Accounts = append(report.Accounts, AccountUsage{
				Account:          account,
				TotalRequests:    entry.TotalRequests,
				PromptTokens:     entry.PromptTokens,
				CompletionTokens: entry.CompletionTokens,
				CacheReadTokens:  entry.CacheReadTokens,
				CacheWriteTokens: entry.CacheWriteTokens,
			})
		}
	}
	u.perAccountMu.Unlock()

	sort.Slice(report.Accounts, func(i, j int) bool {
		return report.Accounts[i].Account < report.Accounts[j].Account
	})
	return report
}

// ====== persistence ======

const usageFile = "usage.json"

func loadUsage() *UsageTracker {
	u := &UsageTracker{}
	data, err := os.ReadFile(usageFile)
	if err != nil {
		return u
	}
	var snap UsageReport
	if json.Unmarshal(data, &snap) != nil {
		return u
	}
	u.TotalRequests.Store(snap.TotalRequests)
	u.PromptTokens.Store(snap.PromptTokens)
	u.CompletionTokens.Store(snap.CompletionTokens)
	u.CacheReadTokens.Store(snap.CacheReadTokens)
	u.CacheWriteTokens.Store(snap.CacheWriteTokens)
	if len(snap.Accounts) > 0 {
		u.perAccount = make(map[string]*accountUsage, len(snap.Accounts))
		for _, acct := range snap.Accounts {
			u.perAccount[acct.Account] = &accountUsage{
				TotalRequests:    acct.TotalRequests,
				PromptTokens:     acct.PromptTokens,
				CompletionTokens: acct.CompletionTokens,
				CacheReadTokens:  acct.CacheReadTokens,
				CacheWriteTokens: acct.CacheWriteTokens,
			}
		}
	}
	return u
}

func (u *UsageTracker) save() error {
	u.saveMu.Lock()
	defer u.saveMu.Unlock()

	report := u.Report()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	tmp := usageFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, usageFile)
}
