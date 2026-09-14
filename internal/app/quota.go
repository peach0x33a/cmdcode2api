package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Command Code exposes quota data through its undocumented /alpha/* endpoints.
// Every shape below tolerates the field drift documented by the community
// commandcode-usage project: camelCase or snake_case keys, epoch seconds,
// epoch milliseconds, or ISO timestamps, and either a flat response or one
// wrapped in a "data" object.
const (
	quotaRefreshInterval = 5 * time.Minute
	quotaRequestTimeout  = 15 * time.Second
	quotaRefreshWorkers  = 4
	quotaUserAgent       = "cmdcode2api/" + Version
)

// QuotaWindow is one server-side rolling limit (the 5-hour and weekly windows).
type QuotaWindow struct {
	Used     float64    `json:"used"`
	Cap      float64    `json:"cap"`
	Exceeded bool       `json:"exceeded"`
	ResetAt  *time.Time `json:"reset_at,omitempty"`
}

// QuotaPlan is the account's subscription and billing period. MonthlyCredits is
// the plan's estimated monthly cap: the API exposes no monthly window object,
// so the cap is derived from the plan mapping and is nil for unknown plans,
// in which case the UI falls back to balance-only display.
type QuotaPlan struct {
	PlanID             string     `json:"plan_id,omitempty"`
	Name               string     `json:"name,omitempty"`
	Status             string     `json:"status,omitempty"`
	MonthlyCredits     *float64   `json:"monthly_credits,omitempty"`
	CurrentPeriodStart *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool       `json:"cancel_at_period_end,omitempty"`
	PendingPhase       string     `json:"pending_phase,omitempty"`
}

// QuotaUsage is the billing-period aggregate from /alpha/usage/summary.
type QuotaUsage struct {
	TotalCount     int64    `json:"total_count"`
	TotalCost      float64  `json:"total_cost"`
	AverageCost    *float64 `json:"average_cost,omitempty"`
	SuccessRate    float64  `json:"success_rate"`
	CompletedCount int64    `json:"completed_count"`
	FailedCount    int64    `json:"failed_count"`
	TotalTokensIn  int64    `json:"total_tokens_in"`
	TotalTokensOut int64    `json:"total_tokens_out"`
	TotalCredits   float64  `json:"total_credits"`
	PeriodBasis    string   `json:"period_basis,omitempty"`
}

// QuotaSnapshot is one account's cached quota view. Failures holds the
// per-endpoint errors of a partially successful query; LastError holds the
// error of a query that returned nothing, in which case the previous snapshot
// is retained and only LastError/LastChecked are updated.
type QuotaSnapshot struct {
	MonthlyCredits   *float64     `json:"monthly_credits,omitempty"`
	PurchasedCredits *float64     `json:"purchased_credits,omitempty"`
	FreeCredits      *float64     `json:"free_credits,omitempty"`
	FiveHour         *QuotaWindow `json:"five_hour,omitempty"`
	Weekly           *QuotaWindow `json:"weekly,omitempty"`
	Limited          bool         `json:"limited,omitempty"`
	Exceeded         string       `json:"exceeded,omitempty"`
	BelowThreshold   bool         `json:"below_threshold,omitempty"`
	CreditThreshold  *float64     `json:"credit_threshold,omitempty"`
	Plan             *QuotaPlan   `json:"plan,omitempty"`
	Usage            *QuotaUsage  `json:"usage,omitempty"`

	LastChecked *time.Time `json:"last_checked,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Failures    []string   `json:"failures,omitempty"`
}

// Blocked reports whether a window currently blocks the account.
func (q *QuotaSnapshot) Blocked() bool {
	if q == nil {
		return false
	}
	if q.Exceeded != "" {
		return true
	}
	return (q.FiveHour != nil && q.FiveHour.Exceeded) || (q.Weekly != nil && q.Weekly.Exceeded)
}

// ====== plan mapping ======

type planSpec struct {
	Name           string
	MonthlyCredits float64
}

// knownPlans mirrors the community CLI's planId mapping. Unknown plans are
// reported by ID with no monthly cap, so the UI hides the derived monthly bar.
var knownPlans = map[string]planSpec{
	"individual-go":       {Name: "Go", MonthlyCredits: 10},
	"individual-goat":     {Name: "GOAT", MonthlyCredits: 70},
	"individual-pro":      {Name: "Pro", MonthlyCredits: 30},
	"individual-pro-v1":   {Name: "Pro", MonthlyCredits: 80},
	"individual-provider": {Name: "Provider", MonthlyCredits: 15},
	"individual-max":      {Name: "Max", MonthlyCredits: 150},
	"individual-ultra":    {Name: "Ultra", MonthlyCredits: 300},
	"teams-pro":           {Name: "Teams Pro", MonthlyCredits: 40},
}

// planPrefixes holds the mapping keys longest-first so individual-pro-v1 wins
// over individual-pro.
var planPrefixes = func() []string {
	keys := make([]string, 0, len(knownPlans))
	for key := range knownPlans {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}()

func planInfo(planID string) (planSpec, bool) {
	if planID == "" {
		return planSpec{}, false
	}
	normalized := strings.ToLower(strings.ReplaceAll(planID, "_", "-"))
	for _, prefix := range planPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return knownPlans[prefix], true
		}
	}
	return planSpec{}, false
}

// ====== defensive JSON helpers ======

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func numOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func numField(m map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if f, ok := numOf(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func numOr(m map[string]any, keys ...string) float64 {
	v, _ := numField(m, keys...)
	return v
}

func numPtr(m map[string]any, keys ...string) *float64 {
	if v, ok := numField(m, keys...); ok {
		return &v
	}
	return nil
}

func strField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func boolField(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch b := v.(type) {
			case bool:
				if b {
					return true
				}
			case string:
				if b == "true" {
					return true
				}
			}
		}
	}
	return false
}

func mapField(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if rec, ok := m[key].(map[string]any); ok {
			return rec
		}
	}
	return nil
}

func timeField(m map[string]any, keys ...string) *time.Time {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if t := epochTime(v); t != nil {
				return t
			}
		}
	}
	return nil
}

// epochTime accepts epoch seconds, epoch milliseconds, or an ISO string.
func epochTime(v any) *time.Time {
	switch t := v.(type) {
	case float64:
		return millisToTime(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return millisToTime(f)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return &parsed
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return millisToTime(f)
		}
	}
	return nil
}

func millisToTime(value float64) *time.Time {
	if value <= 0 {
		return nil
	}
	ms := value
	if value < 1e12 {
		ms = value * 1000
	}
	t := time.UnixMilli(int64(ms))
	return &t
}

// ====== parsing ======

func parseQuotaWindow(raw map[string]any) *QuotaWindow {
	if raw == nil {
		return nil
	}
	used, hasUsed := numField(raw, "used", "usage", "usedCredits", "used_credits")
	capacity, hasCap := numField(raw, "cap", "limit", "capCredits", "cap_credits")
	window := &QuotaWindow{Used: used, Cap: capacity}
	window.Exceeded = boolField(raw, "exceeded") ||
		(hasUsed && hasCap && capacity > 0 && used >= capacity)
	window.ResetAt = timeField(raw, "resetAt", "reset_at", "resetsAt")
	return window
}

func buildQuotaPlan(planID string, data map[string]any) *QuotaPlan {
	plan := &QuotaPlan{PlanID: planID}
	if data != nil {
		plan.Status = strField(data, "status")
		plan.CurrentPeriodStart = timeField(data, "currentPeriodStart", "current_period_start")
		plan.CurrentPeriodEnd = timeField(data, "currentPeriodEnd", "current_period_end")
		plan.CancelAtPeriodEnd = boolField(data, "cancelAtPeriodEnd", "cancel_at_period_end")
		plan.PendingPhase = strField(data, "pendingPhase", "pending_phase")
	}
	if spec, ok := planInfo(planID); ok {
		plan.Name = spec.Name
		credits := spec.MonthlyCredits
		plan.MonthlyCredits = &credits
	} else {
		plan.Name = planID
	}
	return plan
}

func parseQuotaUsage(raw map[string]any) *QuotaUsage {
	if raw == nil {
		return nil
	}
	return &QuotaUsage{
		TotalCount:     int64(numOr(raw, "totalCount", "total_count")),
		TotalCost:      numOr(raw, "totalCost", "total_cost"),
		AverageCost:    numPtr(raw, "averageCost", "average_cost"),
		SuccessRate:    numOr(raw, "successRate", "success_rate"),
		CompletedCount: int64(numOr(raw, "completedCount", "completed_count")),
		FailedCount:    int64(numOr(raw, "failedCount", "failed_count")),
		TotalTokensIn:  int64(numOr(raw, "totalTokensIn", "total_tokens_in")),
		TotalTokensOut: int64(numOr(raw, "totalTokensOut", "total_tokens_out")),
		TotalCredits:   numOr(raw, "totalCredits", "total_credits"),
		PeriodBasis:    strField(raw, "periodBasis", "period_basis"),
	}
}

func snapshotHasData(q *QuotaSnapshot) bool {
	return q.MonthlyCredits != nil || q.PurchasedCredits != nil || q.FreeCredits != nil ||
		q.FiveHour != nil || q.Weekly != nil || q.Plan != nil || q.Usage != nil
}

// ====== upstream queries ======

type quotaHTTPError struct {
	Status int
	Body   string
}

func (e *quotaHTTPError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200]
	}
	if body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, body)
}

func quotaGet(ctx context.Context, client *http.Client, rawURL, apiKey string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", quotaUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &quotaHTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("non-JSON response (HTTP %d)", resp.StatusCode)
	}
	return out, nil
}

func quotaKeyRejected(err error) bool {
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden
	}
	return false
}

// fetchQuotaSnapshot queries the undocumented /alpha endpoints for one key.
// Partial failures are recorded in the snapshot's Failures and the successful
// parts are kept; the query only fails when the key is rejected or no endpoint
// returned usable data.
func fetchQuotaSnapshot(ctx context.Context, client *http.Client, baseURL, apiKey string) (*QuotaSnapshot, error) {
	base := strings.TrimRight(baseURL, "/")
	snap := &QuotaSnapshot{}
	var failures []string

	// 1. whoami → org id for the subscriptions query.
	orgID := ""
	if who, err := quotaGet(ctx, client, base+"/alpha/whoami", apiKey); err != nil {
		if quotaKeyRejected(err) {
			return nil, fmt.Errorf("API Key 被拒绝（%v）", err)
		}
		failures = append(failures, "whoami: "+err.Error())
	} else {
		org := mapField(who, "org")
		if org == nil {
			org = mapField(obj(who["data"]), "org")
		}
		orgID = strField(org, "id")
	}

	// 2. billing/credits → balances + rolling windows.
	planIDFallback := ""
	if creditsResp, err := quotaGet(ctx, client, base+"/alpha/billing/credits", apiKey); err != nil {
		failures = append(failures, "billing/credits: "+err.Error())
	} else {
		root := creditsResp
		if nested := obj(creditsResp["data"]); nested != nil {
			root = nested
		}
		credits := mapField(creditsResp, "credits")
		if credits == nil {
			credits = mapField(root, "credits")
		}
		windows := mapField(creditsResp, "windowLimits", "window_limits")
		if windows == nil {
			windows = mapField(root, "windowLimits", "window_limits")
		}
		if credits != nil {
			snap.MonthlyCredits = numPtr(credits, "monthlyCredits", "monthly_credits")
			snap.PurchasedCredits = numPtr(credits, "purchasedCredits", "purchased_credits")
			snap.FreeCredits = numPtr(credits, "freeCredits", "free_credits")
			snap.BelowThreshold = boolField(credits, "belowThreshold", "below_threshold")
			snap.CreditThreshold = numPtr(credits, "creditThreshold", "credit_threshold")
			planIDFallback = strField(credits, "planId", "plan_id")
		}
		if windows != nil {
			snap.Limited = boolField(windows, "limited")
			snap.Exceeded = strField(windows, "exceeded")
			snap.FiveHour = parseQuotaWindow(mapField(windows, "fiveHour", "five_hour", "rolling5h", "5h"))
			snap.Weekly = parseQuotaWindow(mapField(windows, "weekly", "week"))
		}
	}

	// 3. billing/subscriptions → plan + billing period.
	subURL := base + "/alpha/billing/subscriptions"
	if orgID != "" {
		subURL += "?orgId=" + url.QueryEscape(orgID)
	}
	if sub, err := quotaGet(ctx, client, subURL, apiKey); err != nil {
		if planIDFallback != "" {
			snap.Plan = buildQuotaPlan(planIDFallback, nil)
		}
		failures = append(failures, "billing/subscriptions: "+err.Error())
	} else {
		data := mapField(sub, "data")
		if data == nil {
			data = mapField(sub, "subscription")
		}
		planID := strField(data, "planId", "plan_id")
		if planID == "" {
			planID = planIDFallback
		}
		if data != nil || planID != "" {
			snap.Plan = buildQuotaPlan(planID, data)
		}
	}

	// 4. usage/summary → billing-period aggregates.
	if usageResp, err := quotaGet(ctx, client, base+"/alpha/usage/summary", apiKey); err != nil {
		failures = append(failures, "usage/summary: "+err.Error())
	} else {
		root := usageResp
		if nested := obj(usageResp["data"]); nested != nil {
			root = nested
		}
		snap.Usage = parseQuotaUsage(root)
	}

	if len(failures) > 0 {
		snap.Failures = failures
	}
	if !snapshotHasData(snap) {
		if len(failures) == 0 {
			return nil, fmt.Errorf("额度接口返回空数据")
		}
		return nil, fmt.Errorf("所有额度接口均无法访问：%s", strings.Join(failures, "; "))
	}
	return snap, nil
}

// ====== refresh service ======

// QuotaService fetches and caches per-account quota data. The background loop
// and the admin refresh endpoints share one instance so upstream requests stay
// bounded by quotaRefreshWorkers. Stored snapshots are never mutated after
// SetQuota; updates replace them wholesale.
type QuotaService struct {
	cc     *CCClient
	pool   *AccountPool
	usage  *UsageTracker
	client *http.Client
	sem    chan struct{}
	wg     sync.WaitGroup
}

func NewQuotaService(cc *CCClient, pool *AccountPool, usage *UsageTracker) *QuotaService {
	return &QuotaService{
		cc:     cc,
		pool:   pool,
		usage:  usage,
		client: &http.Client{Timeout: quotaRequestTimeout},
		sem:    make(chan struct{}, quotaRefreshWorkers),
	}
}

// RefreshAccount queries the upstream for one account and updates the cached
// snapshot. A failed query keeps the previous snapshot and only records the
// error and check time.
func (s *QuotaService) RefreshAccount(ctx context.Context, acct *Account) *QuotaSnapshot {
	if acct == nil {
		return nil
	}
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	snap, err := fetchQuotaSnapshot(ctx, s.client, s.cc.BaseURLValue(), acct.APIKey)
	now := time.Now()
	if err != nil {
		snap = cloneForQuotaError(s.usage.Quota(acct.ID), err.Error(), now)
		s.usage.SetQuota(acct.ID, snap)
		s.persist()
		log.Printf("[WARN] quota refresh for account %s failed: %v", acct.Name, err)
		return snap
	}
	snap.LastChecked = &now
	snap.LastError = ""
	s.usage.SetQuota(acct.ID, snap)
	s.persist()
	return snap
}

// cloneForQuotaError copies a cached snapshot and stamps it with a failed
// check, preserving the last successful quota data. The copy shares the nested
// pointers, which are treated as immutable once stored.
func cloneForQuotaError(prev *QuotaSnapshot, message string, at time.Time) *QuotaSnapshot {
	var snap QuotaSnapshot
	if prev != nil {
		snap = *prev
	}
	snap.LastError = message
	snap.LastChecked = &at
	snap.Failures = nil
	return &snap
}

// RefreshAsync refreshes one account without blocking the caller.
func (s *QuotaService) RefreshAsync(acct *Account) {
	if s == nil || acct == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.RefreshAccount(context.Background(), acct)
	}()
}

// RefreshAll refreshes every account, at most quotaRefreshWorkers in flight.
func (s *QuotaService) RefreshAll(ctx context.Context) int {
	accounts := s.pool.List()
	var wg sync.WaitGroup
	for _, acct := range accounts {
		wg.Add(1)
		go func(a *Account) {
			defer wg.Done()
			s.RefreshAccount(ctx, a)
		}(acct)
	}
	wg.Wait()
	return len(accounts)
}

// Wait blocks until every refresh started by RefreshAsync has finished.
func (s *QuotaService) Wait() {
	s.wg.Wait()
}

// Run performs one immediate refresh and then refreshes on a fixed interval
// until ctx is cancelled.
func (s *QuotaService) Run(ctx context.Context) {
	s.RefreshAll(ctx)
	ticker := time.NewTicker(quotaRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshAll(ctx)
		}
	}
}

func (s *QuotaService) persist() {
	if err := s.usage.save(); err != nil {
		log.Printf("[WARN] save quota data failed: %v", err)
	}
}
