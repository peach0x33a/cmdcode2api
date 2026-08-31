package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withBillingURLs points the package-level billing endpoint vars at srv for
// the duration of the calling test, restoring the real commandcode.ai URLs
// on cleanup.
func withBillingURLs(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origSubs, origCredits, origSession := billingSubscriptionsURL, billingCreditsURL, sessionInfoURL
	billingSubscriptionsURL = srv.URL + "/internal/billing/subscriptions"
	billingCreditsURL = srv.URL + "/internal/billing/credits"
	sessionInfoURL = srv.URL + "/auth/get-session"
	t.Cleanup(func() {
		billingSubscriptionsURL, billingCreditsURL, sessionInfoURL = origSubs, origCredits, origSession
	})
}

func TestRefreshOneFetchesAllThreeEndpoints(t *testing.T) {
	rotatedToken := "rotated-token-value"
	var requestTokens []string
	var requestTokensMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		requestTokensMu.Lock()
		if err == nil {
			requestTokens = append(requestTokens, cookie.Value)
		}
		requestTokensMu.Unlock()
		if err != nil {
			t.Errorf("request %s missing expected session cookie: %v", r.URL.Path, err)
		}

		switch r.URL.Path {
		case "/internal/billing/subscriptions":
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: rotatedToken, Secure: true})
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": "sub_1", "status": "active", "planId": "individual-go",
					"currentPeriodEnd": "2026-09-26T01:04:30.000Z",
				},
			})
		case "/internal/billing/credits":
			json.NewEncoder(w).Encode(map[string]any{
				"credits": map[string]any{"monthlyCredits": 9.9},
				"windowLimits": map[string]any{
					"fiveHour": map[string]any{"used": 0, "cap": 3},
					"weekly":   map[string]any{"used": 1, "cap": 6},
				},
			})
		case "/auth/get-session":
			json.NewEncoder(w).Encode(map[string]any{
				"session": map[string]any{"expiresAt": "2026-09-02T20:59:19.438Z"},
				"user":    map[string]any{"email": "jasonb.194@gmail.com"},
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "original-token"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	report := tracker.Report()
	if len(report) != 1 {
		t.Fatalf("report length = %d, want 1", len(report))
	}
	row := report[0]
	if row.LastError != "" {
		t.Fatalf("unexpected LastError: %s", row.LastError)
	}
	if row.Subscription == nil || row.Subscription.PlanID != "individual-go" {
		t.Fatalf("subscription = %+v, want planId individual-go", row.Subscription)
	}
	if row.Credits == nil || row.Credits.Credits.MonthlyCredits != 9.9 {
		t.Fatalf("credits = %+v, want monthlyCredits 9.9", row.Credits)
	}
	if row.SessionEmail != "jasonb.194@gmail.com" {
		t.Fatalf("sessionEmail = %q", row.SessionEmail)
	}
	if row.SessionExpiresAt == nil || row.SessionExpiresAt.IsZero() {
		t.Fatal("sessionExpiresAt was not parsed")
	}

	accounts := store.Accounts()
	if len(accounts) != 1 || accounts[0].SessionToken != rotatedToken {
		t.Fatalf("session token not persisted after rotation: %+v", accounts)
	}
	wantPlanEnd, _ := time.Parse(time.RFC3339, "2026-09-26T01:04:30.000Z")
	if got := accounts[0].PlanExpiresAt; got == nil || !got.Equal(wantPlanEnd) {
		t.Fatalf("persisted PlanExpiresAt = %v, want %v", got, wantPlanEnd)
	}
	requestTokensMu.Lock()
	gotTokens := append([]string(nil), requestTokens...)
	requestTokensMu.Unlock()
	if want := []string{"original-token", rotatedToken, rotatedToken}; !reflect.DeepEqual(gotTokens, want) {
		t.Fatalf("request tokens = %v, want %v", gotTokens, want)
	}
}

func TestRefreshAllSkipsAccountsWithoutSessionToken(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	store := testStore(t)
	if err := store.Upsert(Account{Name: "no-token"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	if hits != 0 {
		t.Fatalf("expected no requests for an account with no session token, got %d", hits)
	}
	if len(tracker.Report()) != 0 {
		t.Fatalf("report = %+v, want empty", tracker.Report())
	}
}

func TestRefreshOneRecordsErrorOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "bad-token"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	report := tracker.Report()
	if len(report) != 1 || report[0].LastError == "" {
		t.Fatalf("expected a recorded LastError, got %+v", report)
	}
}

// TestRefreshOnePersistsPlanExpiry checks a successful subscriptions fetch
// caches currentPeriodEnd into config.yaml so it survives past the session
// token's lifetime.
func TestRefreshOnePersistsPlanExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/billing/subscriptions":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": "sub_1", "status": "active", "planId": "individual-go",
					"currentPeriodEnd": "2026-09-26T01:04:30.000Z",
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "token"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	want, _ := time.Parse(time.RFC3339, "2026-09-26T01:04:30.000Z")
	if got := tracker.Report()[0].PlanExpiresAt; got == nil || !got.Equal(want) {
		t.Fatalf("report PlanExpiresAt = %v, want %v", got, want)
	}
	stored := store.Accounts()[0].PlanExpiresAt
	if stored == nil || !stored.Equal(want) {
		t.Fatalf("persisted PlanExpiresAt = %v, want %v", stored, want)
	}
}

// TestRefreshOneFallsBackToStoredPlanExpiry checks that when every billing
// call fails (an expired session token), the plan expiration date cached in
// config.yaml is still reported instead of being dropped.
func TestRefreshOneFallsBackToStoredPlanExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	stored, _ := time.Parse(time.RFC3339, "2026-09-26T01:04:30Z")
	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "expired-token", PlanExpiresAt: &stored}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	row := tracker.Report()[0]
	if row.LastError == "" {
		t.Fatal("expected a recorded LastError for the failed refresh")
	}
	if row.PlanExpiresAt == nil || !row.PlanExpiresAt.Equal(stored) {
		t.Fatalf("PlanExpiresAt = %v, want stored fallback %v", row.PlanExpiresAt, stored)
	}
}

// TestRefreshOneKeepsStoredPlanExpiryWhenSubscriptionsFail covers the mixed
// case: get-session still works (identity refreshes) but the subscriptions
// endpoint fails. The cached plan expiration date must not be cleared by the
// session-identity write.
func TestRefreshOneKeepsStoredPlanExpiryWhenSubscriptionsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/get-session":
			json.NewEncoder(w).Encode(map[string]any{
				"session": map[string]any{"expiresAt": "2026-09-02T20:59:19.438Z"},
				"user":    map[string]any{"email": "jasonb.194@gmail.com"},
			})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	stored, _ := time.Parse(time.RFC3339, "2026-09-26T01:04:30Z")
	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "tok", PlanExpiresAt: &stored}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	if got := tracker.Report()[0].PlanExpiresAt; got == nil || !got.Equal(stored) {
		t.Fatalf("report PlanExpiresAt = %v, want preserved %v", got, stored)
	}
	acct := store.Accounts()[0]
	if acct.PlanExpiresAt == nil || !acct.PlanExpiresAt.Equal(stored) {
		t.Fatalf("persisted PlanExpiresAt = %v, want preserved %v", acct.PlanExpiresAt, stored)
	}
	if acct.SessionEmail != "jasonb.194@gmail.com" {
		t.Fatalf("SessionEmail = %q, want the refreshed identity", acct.SessionEmail)
	}
}

// TestRefreshOneSkipsConfigWriteWhenUnchanged checks that a refresh which
// learns nothing new — every endpoint fails and the cached fields already
// match config.yaml — does not rewrite the file on each tick.
func TestRefreshOneSkipsConfigWriteWhenUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	sessExp, _ := time.Parse(time.RFC3339, "2026-09-02T20:59:19Z")
	planExp, _ := time.Parse(time.RFC3339, "2026-09-26T01:04:30Z")
	path := t.TempDir() + "/config.yaml"
	store := NewConfigStore(path, &Config{})
	if err := store.Upsert(Account{
		Name: "a", SessionToken: "expired", SessionEmail: "me@example.com",
		SessionExpiresAt: &sessExp, PlanExpiresAt: &planExp,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	backdate := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, backdate, backdate); err != nil {
		t.Fatalf("backdate config: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	tracker := NewBillingTracker()
	tracker.RefreshAll(context.Background(), store)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("config.yaml was rewritten (mtime %v -> %v) despite nothing changing", before.ModTime(), after.ModTime())
	}
}

// TestStartAutoRefreshRunsImmediatelyAndStopsOnCancel mirrors
// TestStartHealthChecksProbesImmediatelyAndStopsOnCancel in
// accounts_probe_test.go: a short interval and explicit deadlines keep this
// from waiting out the real 10-minute interval or hanging on a regression.
func TestStartAutoRefreshRunsImmediatelyAndStopsOnCancel(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	withBillingURLs(t, srv)

	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", SessionToken: "token"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tracker := NewBillingTracker()
	ctx, cancel := context.WithCancel(context.Background())
	tracker.StartAutoRefresh(ctx, store, 15*time.Millisecond)

	waitUntil := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&hits) < 3 { // 3 endpoints hit per refresh
		if time.Now().After(waitUntil) {
			t.Fatal("StartAutoRefresh did not refresh immediately at startup")
		}
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(60 * time.Millisecond)
	cancel()
	countAtCancel := atomic.LoadInt32(&hits)

	time.Sleep(100 * time.Millisecond)
	countAfter := atomic.LoadInt32(&hits)
	if countAfter > countAtCancel+3 {
		t.Fatalf("StartAutoRefresh kept refreshing after ctx was cancelled: hits at cancel=%d, after=%d", countAtCancel, countAfter)
	}
}
