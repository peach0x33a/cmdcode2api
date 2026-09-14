package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

func redirectUsageFile(t *testing.T) {
	t.Helper()
	old := usageFile
	usageFile = filepath.Join(t.TempDir(), "usage.json")
	t.Cleanup(func() { usageFile = old })
}

// ====== plan mapping ======

func TestPlanInfoLongestPrefix(t *testing.T) {
	cases := []struct {
		planID  string
		name    string
		credits float64
		known   bool
	}{
		{"individual-pro", "Pro", 30, true},
		{"individual-pro-v1", "Pro", 80, true},
		{"individual_pro", "Pro", 30, true},
		{"individual-goat", "GOAT", 70, true},
		{"teams-pro", "Teams Pro", 40, true},
		{"individual-max", "Max", 150, true},
		{"enterprise-unknown", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range cases {
		spec, ok := planInfo(tc.planID)
		if ok != tc.known {
			t.Fatalf("planInfo(%q) known = %v, want %v", tc.planID, ok, tc.known)
		}
		if !tc.known {
			continue
		}
		if spec.Name != tc.name || spec.MonthlyCredits != tc.credits {
			t.Fatalf("planInfo(%q) = %+v, want %s/%.0f", tc.planID, spec, tc.name, tc.credits)
		}
	}
}

func TestEpochTimeVariants(t *testing.T) {
	seconds := epochTime(float64(1700000000))
	millis := epochTime(float64(1700000000000))
	if seconds == nil || millis == nil {
		t.Fatal("numeric timestamps not parsed")
	}
	if !seconds.Equal(*millis) {
		t.Fatalf("seconds=%v millis=%v", seconds, millis)
	}
	iso := epochTime("2030-01-02T03:04:05Z")
	if iso == nil || iso.UTC().Year() != 2030 {
		t.Fatalf("ISO timestamp = %v", iso)
	}
	if numeric := epochTime("1700000000"); numeric == nil || !numeric.Equal(*seconds) {
		t.Fatalf("numeric string = %v", epochTime("1700000000"))
	}
	if epochTime(0) != nil || epochTime("") != nil || epochTime("garbage") != nil {
		t.Fatal("invalid timestamps must be nil")
	}
}

// ====== fetch/parse ======

type quotaTestUpstream struct {
	mu       sync.Mutex
	requests []string
	auths    []string
	agents   []string
	accepts  []string
}

func (u *quotaTestUpstream) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	target := r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	u.requests = append(u.requests, target)
	u.auths = append(u.auths, r.Header.Get("Authorization"))
	u.agents = append(u.agents, r.Header.Get("User-Agent"))
	u.accepts = append(u.accepts, r.Header.Get("Accept"))
}

func (u *quotaTestUpstream) paths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func TestFetchQuotaSnapshotHappyPath(t *testing.T) {
	up := &quotaTestUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/whoami":
			fmt.Fprint(w, `{"user":{"userName":"alice"},"org":{"id":"org_123"}}`)
		case "/alpha/billing/credits":
			fmt.Fprint(w, `{"credits":{"monthlyCredits":12.5,"purchasedCredits":3.25,"freeCredits":1.5,"belowThreshold":true,"creditThreshold":5},"windowLimits":{"limited":true,"exceeded":"weekly","fiveHour":{"used":4,"cap":10,"resetAt":1800000000000},"weekly":{"used":25,"cap":20,"resetAt":1800000000}}}`)
		case "/alpha/billing/subscriptions":
			fmt.Fprint(w, `{"data":{"planId":"individual-pro-v1","status":"active","currentPeriodEnd":1767225600000,"cancelAtPeriodEnd":true}}`)
		case "/alpha/usage/summary":
			fmt.Fprint(w, `{"data":{"totalCount":42,"totalCost":9.5,"averageCost":0.22,"successRate":0.98,"completedCount":40,"failedCount":2,"totalTokensIn":1000,"totalTokensOut":2000,"totalCredits":8.5,"periodBasis":"billing_period"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	snap, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL, "cc-key")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(snap.Failures) != 0 || snap.LastError != "" {
		t.Fatalf("unexpected failures: %+v", snap)
	}
	if snap.MonthlyCredits == nil || *snap.MonthlyCredits != 12.5 ||
		snap.PurchasedCredits == nil || *snap.PurchasedCredits != 3.25 ||
		snap.FreeCredits == nil || *snap.FreeCredits != 1.5 {
		t.Fatalf("balances = %+v", snap)
	}
	if !snap.BelowThreshold || snap.CreditThreshold == nil || *snap.CreditThreshold != 5 {
		t.Fatalf("threshold fields = %+v", snap)
	}
	if snap.FiveHour == nil || snap.FiveHour.Used != 4 || snap.FiveHour.Cap != 10 ||
		snap.FiveHour.Exceeded || snap.FiveHour.ResetAt == nil {
		t.Fatalf("five hour window = %+v", snap.FiveHour)
	}
	if snap.Weekly == nil || snap.Weekly.Used != 25 || snap.Weekly.Cap != 20 || !snap.Weekly.Exceeded {
		t.Fatalf("weekly window = %+v", snap.Weekly)
	}
	if !snap.Limited || snap.Exceeded != "weekly" {
		t.Fatalf("limit flags = limited:%v exceeded:%q", snap.Limited, snap.Exceeded)
	}
	if snap.Plan == nil || snap.Plan.PlanID != "individual-pro-v1" || snap.Plan.Name != "Pro" ||
		snap.Plan.MonthlyCredits == nil || *snap.Plan.MonthlyCredits != 80 ||
		snap.Plan.Status != "active" || !snap.Plan.CancelAtPeriodEnd || snap.Plan.CurrentPeriodEnd == nil {
		t.Fatalf("plan = %+v", snap.Plan)
	}
	if snap.Usage == nil || snap.Usage.TotalCount != 42 || snap.Usage.SuccessRate != 0.98 ||
		snap.Usage.TotalTokensOut != 2000 || snap.Usage.PeriodBasis != "billing_period" {
		t.Fatalf("usage = %+v", snap.Usage)
	}

	want := []string{
		"/alpha/whoami",
		"/alpha/billing/credits",
		"/alpha/billing/subscriptions?orgId=org_123",
		"/alpha/usage/summary",
	}
	if got := up.paths(); !equalStrings(got, want) {
		t.Fatalf("requested paths = %v, want %v", got, want)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	for i := range up.auths {
		if up.auths[i] != "Bearer cc-key" {
			t.Fatalf("request %d auth = %q", i, up.auths[i])
		}
		if up.accepts[i] != "application/json" {
			t.Fatalf("request %d accept = %q", i, up.accepts[i])
		}
		if !strings.HasPrefix(up.agents[i], "cmdcode2api/") {
			t.Fatalf("request %d user-agent = %q", i, up.agents[i])
		}
	}
}

func TestFetchQuotaSnapshotSnakeCaseAndNested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/whoami":
			fmt.Fprint(w, `{"data":{"user":{"userName":"bob"},"org":{"id":"org snake"}}}`)
		case "/alpha/billing/credits":
			fmt.Fprint(w, `{"data":{"credits":{"monthly_credits":12.5,"purchased_credits":3,"free_credits":1,"below_threshold":true,"credit_threshold":5,"plan_id":"individual-go"},"window_limits":{"five_hour":{"usage":4,"limit":10,"reset_at":"2030-01-02T03:04:05Z"},"weekly":{"usage":1,"cap_credits":20,"exceeded":"true"}}}}`)
		case "/alpha/billing/subscriptions":
			fmt.Fprint(w, `{"subscription":{"plan_id":"unknown-plan","status":"trialing","current_period_end":"2030-02-01T00:00:00Z"}}`)
		case "/alpha/usage/summary":
			fmt.Fprint(w, `{"total_count":7,"total_cost":0.7,"success_rate":0.5,"completed_count":3,"failed_count":4,"total_tokens_in":5,"total_tokens_out":6,"total_credits":0.3,"period_basis":"month"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	snap, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL+"/", "cc-key")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if snap.MonthlyCredits == nil || *snap.MonthlyCredits != 12.5 || !snap.BelowThreshold {
		t.Fatalf("snake_case balances = %+v", snap)
	}
	if snap.FiveHour == nil || snap.FiveHour.Used != 4 || snap.FiveHour.Cap != 10 || snap.FiveHour.ResetAt == nil {
		t.Fatalf("five_hour window = %+v", snap.FiveHour)
	}
	if snap.Weekly == nil || snap.Weekly.Used != 1 || snap.Weekly.Cap != 20 || !snap.Weekly.Exceeded {
		t.Fatalf("weekly window = %+v", snap.Weekly)
	}
	if snap.Plan == nil || snap.Plan.Name != "unknown-plan" || snap.Plan.MonthlyCredits != nil {
		t.Fatalf("unknown plan = %+v", snap.Plan)
	}
	if snap.Plan.CurrentPeriodEnd == nil || snap.Plan.CurrentPeriodEnd.UTC().Year() != 2030 {
		t.Fatalf("period end = %v", snap.Plan.CurrentPeriodEnd)
	}
	if snap.Usage == nil || snap.Usage.TotalCount != 7 || snap.Usage.FailedCount != 4 || snap.Usage.PeriodBasis != "month" {
		t.Fatalf("usage = %+v", snap.Usage)
	}
}

func TestFetchQuotaSnapshotPartialFailureKeepsData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/billing/credits":
			fmt.Fprint(w, `{"credits":{"monthlyCredits":7,"purchasedCredits":2,"freeCredits":0,"planId":"individual-max"},"windowLimits":{"fiveHour":{"used":1,"cap":9}}}`)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	snap, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL, "cc-key")
	if err != nil {
		t.Fatalf("partial failure must still succeed: %v", err)
	}
	if snap.MonthlyCredits == nil || *snap.MonthlyCredits != 7 {
		t.Fatalf("credits lost: %+v", snap)
	}
	if snap.FiveHour == nil || snap.FiveHour.Cap != 9 {
		t.Fatalf("window lost: %+v", snap.FiveHour)
	}
	// subscriptions failed, so the plan is reconstructed from the credits planId.
	if snap.Plan == nil || snap.Plan.PlanID != "individual-max" || snap.Plan.Name != "Max" {
		t.Fatalf("plan fallback = %+v", snap.Plan)
	}
	if len(snap.Failures) != 3 {
		t.Fatalf("failures = %v, want 3 entries", snap.Failures)
	}
}

func TestFetchQuotaSnapshotRejectsKeyWithoutFurtherCalls(t *testing.T) {
	up := &quotaTestUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		http.Error(w, `{"message":"invalid key"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL, "cc-bad"); err == nil {
		t.Fatal("rejected key must fail")
	}
	if got := up.paths(); len(got) != 1 || got[0] != "/alpha/whoami" {
		t.Fatalf("requests after 403 = %v, want only whoami", got)
	}
}

func TestFetchQuotaSnapshotNonJSONFailsWhenNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "not json at all")
	}))
	defer srv.Close()

	_, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL, "cc-key")
	if err == nil || !strings.Contains(err.Error(), "所有额度接口均无法访问") {
		t.Fatalf("err = %v, want aggregate failure", err)
	}
}

func TestFetchQuotaSnapshotMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/billing/credits":
			fmt.Fprint(w, `{}`)
		case "/alpha/usage/summary":
			fmt.Fprint(w, `{"data":{}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	snap, err := fetchQuotaSnapshot(context.Background(), srv.Client(), srv.URL, "cc-key")
	if err != nil {
		t.Fatalf("empty-but-200 responses should still parse: %v", err)
	}
	if snap.MonthlyCredits != nil || snap.FiveHour != nil || snap.Weekly != nil || snap.Plan != nil {
		t.Fatalf("empty response produced fields: %+v", snap)
	}
	if snap.Usage == nil || snap.Usage.TotalCount != 0 {
		t.Fatalf("usage = %+v", snap.Usage)
	}
}

// ====== refresh service ======

func TestRefreshAccountKeepsSnapshotOnFailure(t *testing.T) {
	redirectUsageFile(t)
	var fail bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		down := fail
		mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeQuotaFixture(w, r)
	}))
	defer srv.Close()

	pool := NewAccountPool(nil)
	acct, err := pool.Add("main", "cc-key", true)
	if err != nil {
		t.Fatal(err)
	}
	cc := NewCCClientWithPool(pool, srv.URL)
	usage := &UsageTracker{}
	svc := NewQuotaService(cc, pool, usage)

	first := svc.RefreshAccount(context.Background(), acct)
	if first == nil || first.MonthlyCredits == nil || *first.MonthlyCredits != 12.5 {
		t.Fatalf("first refresh = %+v", first)
	}
	if first.LastChecked == nil || first.LastError != "" {
		t.Fatalf("first refresh metadata = %+v", first)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	second := svc.RefreshAccount(context.Background(), acct)
	if second == nil || second.LastError == "" {
		t.Fatalf("second refresh must record the error: %+v", second)
	}
	if second.MonthlyCredits == nil || *second.MonthlyCredits != 12.5 {
		t.Fatalf("previous quota not preserved: %+v", second)
	}
	if second.LastChecked == nil || !second.LastChecked.After(*first.LastChecked) {
		t.Fatalf("last_checked not advanced: %+v", second.LastChecked)
	}
	if cached := usage.Quota(acct.ID); cached == nil || cached.LastError == "" {
		t.Fatalf("cached snapshot not updated: %+v", cached)
	}
}

func TestRefreshAllRefreshesEveryAccount(t *testing.T) {
	redirectUsageFile(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeQuotaFixture(w, r)
	}))
	defer srv.Close()

	pool := NewAccountPool(nil)
	for _, key := range []string{"key-1", "key-2", "key-3"} {
		if _, err := pool.Add(key, key, true); err != nil {
			t.Fatal(err)
		}
	}
	cc := NewCCClientWithPool(pool, srv.URL)
	usage := &UsageTracker{}
	svc := NewQuotaService(cc, pool, usage)

	if n := svc.RefreshAll(context.Background()); n != 3 {
		t.Fatalf("RefreshAll returned %d, want 3", n)
	}
	for _, acct := range pool.List() {
		if usage.Quota(acct.ID) == nil {
			t.Fatalf("account %s missing snapshot", acct.Name)
		}
	}
}

// ====== persistence ======

func TestUsageSnapshotPersistsQuotas(t *testing.T) {
	redirectUsageFile(t)
	usage := &UsageTracker{}
	usage.SetQuota("a1", &QuotaSnapshot{
		MonthlyCredits: floatPtr(12.5),
		Plan:           &QuotaPlan{Name: "Pro", MonthlyCredits: floatPtr(80)},
		FiveHour:       &QuotaWindow{Used: 4, Cap: 10},
	})
	if err := usage.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadUsage()
	snap := reloaded.Quota("a1")
	if snap == nil {
		t.Fatal("quota not persisted")
	}
	if snap.MonthlyCredits == nil || *snap.MonthlyCredits != 12.5 {
		t.Fatalf("monthly credits = %+v", snap.MonthlyCredits)
	}
	if snap.Plan == nil || snap.Plan.Name != "Pro" || snap.Plan.MonthlyCredits == nil || *snap.Plan.MonthlyCredits != 80 {
		t.Fatalf("plan = %+v", snap.Plan)
	}
	if snap.FiveHour == nil || snap.FiveHour.Cap != 10 {
		t.Fatalf("five hour = %+v", snap.FiveHour)
	}
}

func TestDropAccountClearsQuota(t *testing.T) {
	usage := &UsageTracker{}
	usage.SetQuota("a1", &QuotaSnapshot{MonthlyCredits: floatPtr(1)})
	usage.DropAccount("a1")
	if usage.Quota("a1") != nil {
		t.Fatal("quota not cleared with the account")
	}
}

func TestKeyChangeDoesNotMigrateQuota(t *testing.T) {
	usage := &UsageTracker{}
	usage.SetQuota("old", &QuotaSnapshot{MonthlyCredits: floatPtr(1)})
	usage.MoveAccount("old", "new")
	if usage.Quota("new") != nil {
		t.Fatal("quota must not migrate to the new key")
	}
	if usage.Quota("old") == nil {
		t.Fatal("old quota should remain until explicitly dropped")
	}
	usage.DropQuota("old")
	if usage.Quota("old") != nil {
		t.Fatal("quota not dropped")
	}
}

// ====== admin API ======

type quotaUpstreamStub struct {
	mu       sync.Mutex
	fail     bool
	requests []string
}

func (s *quotaUpstreamStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	down := s.fail
	s.mu.Unlock()
	if down {
		http.Error(w, "upstream down", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeQuotaFixture(w, r)
}

func (s *quotaUpstreamStub) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func writeQuotaFixture(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/alpha/whoami":
		fmt.Fprint(w, `{"org":{"id":"org_123"}}`)
	case "/alpha/billing/credits":
		fmt.Fprint(w, `{"credits":{"monthlyCredits":12.5,"purchasedCredits":3,"freeCredits":1},"windowLimits":{"fiveHour":{"used":4,"cap":10},"weekly":{"used":25,"cap":20}}}`)
	case "/alpha/billing/subscriptions":
		fmt.Fprint(w, `{"data":{"planId":"individual-pro","status":"active","currentPeriodEnd":1767225600000}}`)
	case "/alpha/usage/summary":
		fmt.Fprint(w, `{"totalCount":5,"totalCost":1.5,"successRate":1,"totalTokensIn":10,"totalTokensOut":20,"totalCredits":1}`)
	default:
		http.NotFound(w, r)
	}
}

func newQuotaAdminTestEnv(t *testing.T) (*httptest.Server, *AccountPool, *UsageTracker, *quotaUpstreamStub) {
	t.Helper()

	oldConfigFile := configFile
	configFile = filepath.Join(t.TempDir(), "config.yaml")
	t.Cleanup(func() { configFile = oldConfigFile })
	redirectUsageFile(t)

	stub := &quotaUpstreamStub{}
	upstream := httptest.NewServer(stub)
	t.Cleanup(upstream.Close)

	cfg := &Config{APIKey: "client-key", Host: "localhost", Port: 11434}
	cfg.SetUpstreamBaseURL(upstream.URL)
	cfg.setAdminPassword("admin-pass-123")
	pool := NewAccountPool(nil)
	keys := NewClientKeyPool(nil)
	cc := NewCCClientWithPool(pool, upstream.URL)
	usage := &UsageTracker{}
	ring := newLogRing()
	quotas := NewQuotaService(cc, pool, usage)

	mux := http.NewServeMux()
	registerAdminRoutes(mux, cc, pool, keys, cfg, usage, ring, quotas)
	root := http.NewServeMux()
	root.HandleFunc("POST /admin/api/oauth/callback", handleWebOAuthCallback())
	root.Handle("/admin/", adminAuth(cfg, nil)(mux))
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	t.Cleanup(quotas.Wait)
	return srv, pool, usage, stub
}

func waitForQuota(t *testing.T, usage *UsageTracker, id string) *QuotaSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if snap := usage.Quota(id); snap != nil {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota for %s not refreshed in time", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdminAddAccountTriggersQuotaRefresh(t *testing.T) {
	srv, _, usage, _ := newQuotaAdminTestEnv(t)

	resp, _ := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "main", "api_key": "cc-key-quota"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}

	snap := waitForQuota(t, usage, accountID("cc-key-quota"))
	if snap.MonthlyCredits == nil || *snap.MonthlyCredits != 12.5 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Plan == nil || snap.Plan.Name != "Pro" {
		t.Fatalf("plan = %+v", snap.Plan)
	}
}

func TestAdminAccountListIncludesQuota(t *testing.T) {
	srv, pool, usage, _ := newQuotaAdminTestEnv(t)
	acct, err := pool.Add("main", "cc-key-quota", true)
	if err != nil {
		t.Fatal(err)
	}
	usage.SetQuota(acct.ID, &QuotaSnapshot{MonthlyCredits: floatPtr(9.5), LastChecked: timePtr(time.Now())})

	_, payload := adminRequest(t, srv, "GET", "/admin/api/accounts", "admin-pass-123", nil)
	list := payload["accounts"].([]any)
	if len(list) != 1 {
		t.Fatalf("accounts = %v", list)
	}
	quota, ok := list[0].(map[string]any)["quota"].(map[string]any)
	if !ok {
		t.Fatalf("quota missing from account row: %v", list[0])
	}
	if quota["monthly_credits"] != 9.5 {
		t.Fatalf("quota = %v", quota)
	}
}

func TestAdminQuotaRefreshEndpoints(t *testing.T) {
	srv, pool, usage, _ := newQuotaAdminTestEnv(t)
	acct, err := pool.Add("main", "cc-key-quota", true)
	if err != nil {
		t.Fatal(err)
	}

	// Single-account refresh returns the account row with a fresh snapshot.
	resp, payload := adminRequest(t, srv, "POST", "/admin/api/accounts/"+acct.ID+"/quota/refresh", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single refresh status = %d: %v", resp.StatusCode, payload)
	}
	quota, ok := payload["quota"].(map[string]any)
	if !ok || quota["monthly_credits"] != 12.5 {
		t.Fatalf("single refresh quota = %v", payload["quota"])
	}

	// Unknown account.
	resp, _ = adminRequest(t, srv, "POST", "/admin/api/accounts/nope/quota/refresh", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", resp.StatusCode)
	}

	// Refresh all.
	usage.SetQuota(acct.ID, &QuotaSnapshot{MonthlyCredits: floatPtr(0)})
	resp, payload = adminRequest(t, srv, "POST", "/admin/api/quotas/refresh", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh all status = %d: %v", resp.StatusCode, payload)
	}
	list := payload["accounts"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["quota"].(map[string]any)["monthly_credits"] != 12.5 {
		t.Fatalf("refresh all payload = %v", payload)
	}

	// Refresh one via the body id.
	resp, payload = adminRequest(t, srv, "POST", "/admin/api/quotas/refresh", "admin-pass-123",
		map[string]any{"id": acct.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh by id status = %d: %v", resp.StatusCode, payload)
	}
	resp, _ = adminRequest(t, srv, "POST", "/admin/api/quotas/refresh", "admin-pass-123",
		map[string]any{"id": "missing"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("refresh missing id status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminOverviewQuotaSummary(t *testing.T) {
	srv, pool, usage, _ := newQuotaAdminTestEnv(t)
	acct1, _ := pool.Add("a", "key-1-quota", true)
	acct2, _ := pool.Add("b", "key-2-quota", true)

	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	usage.SetQuota(acct1.ID, &QuotaSnapshot{
		MonthlyCredits: floatPtr(1),
		BelowThreshold: true,
		FiveHour:       &QuotaWindow{Used: 5, Cap: 5, Exceeded: true},
		LastChecked:    timePtr(older),
	})
	usage.SetQuota(acct2.ID, &QuotaSnapshot{MonthlyCredits: floatPtr(50), LastChecked: timePtr(newer)})

	_, payload := adminRequest(t, srv, "GET", "/admin/api/overview", "admin-pass-123", nil)
	summary, ok := payload["quotas"].(map[string]any)
	if !ok {
		t.Fatalf("overview missing quotas: %v", payload)
	}
	if summary["synced"] != float64(2) || summary["exceeded"] != float64(1) || summary["low_balance"] != float64(1) {
		t.Fatalf("quota summary = %v", summary)
	}
	last, ok := summary["last_checked_at"].(string)
	if !ok {
		t.Fatalf("last_checked_at missing: %v", summary)
	}
	parsed, err := time.Parse(time.RFC3339Nano, last)
	if err != nil {
		t.Fatalf("last_checked_at parse: %v", err)
	}
	if parsed.Unix() != newer.Unix() {
		t.Fatalf("last_checked_at = %v, want %v", parsed, newer)
	}
}

func TestAdminOverviewQuotaSummaryEmpty(t *testing.T) {
	srv, pool, _, _ := newQuotaAdminTestEnv(t)
	pool.Add("a", "key-1-quota", true)

	_, payload := adminRequest(t, srv, "GET", "/admin/api/overview", "admin-pass-123", nil)
	summary := payload["quotas"].(map[string]any)
	if summary["synced"] != float64(0) || summary["exceeded"] != float64(0) || summary["low_balance"] != float64(0) {
		t.Fatalf("empty summary = %v", summary)
	}
	if _, present := summary["last_checked_at"]; present {
		t.Fatalf("empty summary should omit last_checked_at: %v", summary)
	}
}

func TestAdminAccountKeyChangeDropsQuota(t *testing.T) {
	srv, pool, usage, _ := newQuotaAdminTestEnv(t)
	acct, err := pool.Add("main", "cc-key-old", true)
	if err != nil {
		t.Fatal(err)
	}
	oldID := acct.ID
	usage.SetQuota(oldID, &QuotaSnapshot{MonthlyCredits: floatPtr(4), LastChecked: timePtr(time.Now())})

	resp, _ := adminRequest(t, srv, "PATCH", "/admin/api/accounts/"+oldID, "admin-pass-123",
		map[string]any{"api_key": "cc-key-new"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", resp.StatusCode)
	}
	if usage.Quota(oldID) != nil {
		t.Fatal("quota for the old credential must be dropped")
	}
	newID := accountID("cc-key-new")
	if newID == oldID {
		t.Fatal("key change did not change the account ID")
	}
	// The new credential gets its own snapshot from the background refresh.
	if snap := waitForQuota(t, usage, newID); snap.MonthlyCredits == nil {
		t.Fatalf("new credential snapshot = %+v", snap)
	}
}

func TestAdminAccountDeleteClearsQuota(t *testing.T) {
	srv, pool, usage, _ := newQuotaAdminTestEnv(t)
	acct, err := pool.Add("main", "cc-key-delete", true)
	if err != nil {
		t.Fatal(err)
	}
	usage.SetQuota(acct.ID, &QuotaSnapshot{MonthlyCredits: floatPtr(4), LastChecked: timePtr(time.Now())})

	resp, _ := adminRequest(t, srv, "DELETE", "/admin/api/accounts/"+acct.ID, "admin-pass-123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if usage.Quota(acct.ID) != nil {
		t.Fatal("quota not cleared with the account")
	}
}

// ====== small helpers ======

func timePtr(t time.Time) *time.Time { return &t }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
