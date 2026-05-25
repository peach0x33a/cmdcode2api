package main

import (
	"encoding/json"
	"os"
	"sync/atomic"
)

type UsageTracker struct {
	TotalRequests    atomic.Int64  `json:"total_requests"`
	PromptTokens     atomic.Int64  `json:"prompt_tokens"`
	CompletionTokens atomic.Int64  `json:"completion_tokens"`
	CacheReadTokens  atomic.Int64  `json:"cache_read_tokens"`
	CacheWriteTokens atomic.Int64  `json:"cache_write_tokens"`
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

// EstimatedCredits returns rough credit estimate for a model.
// Credits = cost per 1M tokens. Go plan: e.g. deepseek-v4-pro is heavily discounted.
// These are just estimates — actual CC billing may differ.
func (s UsageSnapshot) EstimatedCredits() float64 {
	// Budget rate: ~$0.0001 per 1K prompt tokens (rough average for open-source models)
	// More accurate: use deepseek-v4-flash pricing: $0.14/1M input, $0.28/1M output
	inputCost := float64(s.PromptTokens) / 1_000_000 * 0.14
	outputCost := float64(s.CompletionTokens) / 1_000_000 * 0.28
	cacheCost := float64(s.CacheReadTokens)/1_000_000*0.01 +
		float64(s.CacheWriteTokens)/1_000_000*0.01
	return inputCost + outputCost + cacheCost
}

// ====== persistence ======

const usageFile = "usage.json"

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
	return u
}

func (u *UsageTracker) save() error {
	snap := u.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(usageFile, data, 0644)
}
