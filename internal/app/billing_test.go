package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
