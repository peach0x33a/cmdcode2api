package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func billingRow(name string, used, cap float64) AccountBilling {
	now := time.Now()
	return AccountBilling{
		Account: name,
		BillingInfo: BillingInfo{
			FetchedAt: &now,
			Credits: &BillingCreditsResponse{Credits: BillingCredits{MonthlyCredits: 10}, WindowLimits: BillingWindowLimits{
				FiveHour: BillingWindow{Used: used, Cap: cap, ResetAt: 123},
				Weekly:   BillingWindow{Used: used, Cap: cap, ResetAt: 456},
			}},
		},
	}
}

func TestDiscordAlerterPostsAndPersistsSuccessfulDedupe(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["content"] == "" {
			t.Errorf("invalid webhook body: %#v, err=%v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "alerts.json")
	a := NewDiscordAlerter(server.URL, path)
	a.Evaluate([]AccountBilling{billingRow("one", 9, 10)})
	waitForDiscordPosts(t, &posts, 2)
	a.Evaluate([]AccountBilling{billingRow("one", 9, 10)})
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("posts after duplicate evaluation = %d, want 2", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("state file mode = %o, want 600", info.Mode().Perm())
	}
	b := NewDiscordAlerter(server.URL, path)
	b.Evaluate([]AccountBilling{billingRow("one", 9, 10)})
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("posts after restart = %d, want 2", got)
	}
}

func TestDiscordAlerterRetriesFailedDelivery(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&posts, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	a := NewDiscordAlerterWithConfig(DiscordAlertConfig{
		WebhookURL: server.URL,
		StateFile:  filepath.Join(t.TempDir(), "alerts.json"),
		HourlyCap:  10,
	})
	row := []AccountBilling{billingRow("one", 9, 10)}
	a.Evaluate(row)
	waitForDiscordPosts(t, &posts, 1)
	a.Evaluate(row)
	waitForDiscordPosts(t, &posts, 2)
}

func TestDiscordAlerterSuppressesUsageWhenAnotherAccountUsesSameWindow(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	a := NewDiscordAlerter(server.URL, filepath.Join(t.TempDir(), "alerts.json"))
	a.Evaluate([]AccountBilling{billingRow("alert", 9, 10), billingRow("other", 1, 10)})
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("got %d posts, want suppression", posts)
	}
}

func TestDiscordAlerterNotifiesSessionExpiry(t *testing.T) {
	var posts int32
	var messages []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		messages = append(messages, body["content"])
		mu.Unlock()
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	expires := time.Now().Add(24 * time.Hour)
	row := AccountBilling{Account: "one", BillingInfo: BillingInfo{FetchedAt: ptrTime(time.Now()), SessionExpiresAt: &expires}}
	a := NewDiscordAlerter(server.URL, filepath.Join(t.TempDir(), "alerts.json"))
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 || !strings.Contains(messages[0], "Session token") {
		t.Fatalf("messages = %v, want session expiry alert", messages)
	}
}

func TestDiscordAlerterMentionEveryoneToggle(t *testing.T) {
	var contents []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		contents = append(contents, body["content"])
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	row := AccountBilling{Account: "one", BillingInfo: BillingInfo{FetchedAt: ptrTime(time.Now()), SessionExpiresAt: ptrTime(time.Now().Add(24 * time.Hour))}}
	for _, mention := range []bool{false, true} {
		a := NewDiscordAlerterWithConfig(DiscordAlertConfig{WebhookURL: server.URL, StateFile: filepath.Join(t.TempDir(), "alerts.json"), MentionEveryone: mention})
		a.Evaluate([]AccountBilling{row})
	}
	waitForDiscordPosts(t, new(int32), 0)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 2 || strings.HasPrefix(contents[0], "@everyone ") || !strings.HasPrefix(contents[1], "@everyone ") {
		t.Fatalf("contents = %v, want disabled then enabled mention", contents)
	}
}

func TestDiscordAlerterSubscriptionExpiryDedupesAcrossRestart(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "alerts.json")
	end := time.Now().Add(6 * 24 * time.Hour).UTC()
	row := AccountBilling{Account: "one", BillingInfo: BillingInfo{
		FetchedAt:    ptrTime(time.Now()),
		Subscription: &BillingSubscription{CurrentPeriodEnd: end.Format(time.RFC3339)},
	}}
	a := NewDiscordAlerter(server.URL, path)
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
	b := NewDiscordAlerter(server.URL, path)
	b.Evaluate([]AccountBilling{row})
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("subscription expiry posts after restart = %d, want 1", got)
	}
}

func TestDiscordAlerterSessionExpiryDedupesAcrossRestart(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "alerts.json")
	expires := time.Now().Add(12 * time.Hour).UTC()
	row := AccountBilling{Account: "one", BillingInfo: BillingInfo{
		FetchedAt:        ptrTime(time.Now()),
		SessionExpiresAt: &expires,
	}}
	a := NewDiscordAlerter(server.URL, path)
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
	b := NewDiscordAlerter(server.URL, path)
	b.Evaluate([]AccountBilling{row})
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("session expiry posts after restart = %d, want 1", got)
	}
}

func TestDiscordAlerterSessionExpiryWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		wantPost bool
	}{
		{name: "inside window", offset: 24*time.Hour - time.Minute, wantPost: true},
		{name: "outside window", offset: 24*time.Hour + time.Minute, wantPost: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&posts, 1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			expires := time.Now().Add(tc.offset)
			row := AccountBilling{Account: "one", BillingInfo: BillingInfo{FetchedAt: ptrTime(time.Now()), SessionExpiresAt: &expires}}
			a := NewDiscordAlerter(server.URL, filepath.Join(t.TempDir(), "alerts.json"))
			a.Evaluate([]AccountBilling{row})
			if tc.wantPost {
				waitForDiscordPosts(t, &posts, 1)
				return
			}
			time.Sleep(50 * time.Millisecond)
			if got := atomic.LoadInt32(&posts); got != 0 {
				t.Fatalf("posts = %d, want 0 outside 24-hour window", got)
			}
		})
	}
}

func TestDiscordAlerterSessionExpiryAtBoundary(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	expires := time.Now().Add(sessionExpiryAlertWindow)
	row := AccountBilling{Account: "one", BillingInfo: BillingInfo{FetchedAt: ptrTime(time.Now()), SessionExpiresAt: &expires}}
	a := NewDiscordAlerter(server.URL, filepath.Join(t.TempDir(), "alerts.json"))
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
}

func TestDiscordAlerterOnlyAlertsHighestCrossedThresholdAndRearmsAfterReset(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "alerts.json")
	a := NewDiscordAlerter(server.URL, path)
	row := billingRow("one", 9.6, 10)
	// Exercise one billing metric so the threshold-crossing assertion is
	// independent of the separate weekly alert.
	row.Credits.WindowLimits.Weekly = BillingWindow{}
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
	// A jump to 96% must not emit the 90% alert as well.
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("posts after threshold jump = %d, want 1", got)
	}
	row.Credits.WindowLimits.FiveHour.ResetAt = 999
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 2)
}

func TestDiscordAlerterThresholdRearmsBelowNinety(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	a := NewDiscordAlerterWithConfig(DiscordAlertConfig{
		WebhookURL: server.URL,
		StateFile:  filepath.Join(t.TempDir(), "alerts.json"),
		HourlyCap:  10,
	})
	row := billingRow("one", 9, 10)
	row.Credits.WindowLimits.Weekly = BillingWindow{}
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 1)
	row.Credits.WindowLimits.FiveHour.Used = 8
	a.Evaluate([]AccountBilling{row})
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("posts below threshold = %d, want 1", got)
	}
	row.Credits.WindowLimits.FiveHour.Used = 9
	a.Evaluate([]AccountBilling{row})
	waitForDiscordPosts(t, &posts, 2)
}

func TestMonthlyCreditsUseRemainingBalanceForAlerts(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	a := NewDiscordAlerterWithConfig(DiscordAlertConfig{WebhookURL: server.URL, StateFile: filepath.Join(t.TempDir(), "alerts.json"), MonthlyCap: 10})
	for i, remaining := range []float64{1, 0.5, 0} {
		row := AccountBilling{Account: "monthly", BillingInfo: BillingInfo{FetchedAt: ptrTime(time.Now()), Credits: &BillingCreditsResponse{Credits: BillingCredits{MonthlyCredits: remaining}}}}
		a.Evaluate([]AccountBilling{row})
		waitForDiscordPosts(t, &posts, int32(i+1))
	}
}

func TestDiscordAlerterDoesNotDedupeWhenStateSaveFails(t *testing.T) {
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state-dir")
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	a := NewDiscordAlerter(server.URL, statePath)
	row := []AccountBilling{billingRow("one", 9, 10)}
	row[0].Credits.WindowLimits.Weekly = BillingWindow{}
	a.Evaluate(row)
	waitForDiscordPosts(t, &posts, 1)
	a.Evaluate(row)
	waitForDiscordPosts(t, &posts, 2)
}

func ptrTime(value time.Time) *time.Time { return &value }

func waitForDiscordPosts(t *testing.T, posts *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(posts) < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(posts); got < want {
		t.Fatalf("webhook posts = %d, want at least %d", got, want)
	}
}
