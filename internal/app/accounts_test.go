package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func poolWithKeys(keys ...string) *AccountPool {
	list := make([]AccountConfig, 0, len(keys))
	for i, key := range keys {
		list = append(list, AccountConfig{Name: fmt.Sprintf("acct-%d", i), APIKey: key})
	}
	return NewAccountPool(list)
}

func chatRequestForTest() *ChatRequest {
	return &ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	}
}

func TestAccountPoolRoundRobin(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b", "key-c")
	var order []string
	for i := 0; i < 6; i++ {
		acct := pool.Acquire()
		if acct == nil {
			t.Fatalf("Acquire() = nil at %d", i)
		}
		order = append(order, acct.APIKey)
	}
	want := []string{"key-a", "key-b", "key-c", "key-a", "key-b", "key-c"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rotation order = %v, want %v", order, want)
		}
	}
}

func TestAccountPoolSkipsDisabledAndRateLimited(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b", "key-c")
	pool.SetEnabled(accountID("key-a"), false)
	// Rate-limit key-b; only key-c remains eligible.
	b := pool.Get(accountID("key-b"))
	b.RecordFailure(&upstreamAPIError{Status: http.StatusTooManyRequests})

	for i := 0; i < 3; i++ {
		acct := pool.Acquire()
		if acct == nil || acct.APIKey != "key-c" {
			t.Fatalf("Acquire() = %v, want key-c", acct)
		}
	}
}

func TestAccountPoolAllLimitedReturnsNil(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b")
	for _, key := range []string{"key-a", "key-b"} {
		pool.Get(accountID(key)).RecordFailure(&upstreamAPIError{Status: http.StatusTooManyRequests})
	}
	if acct := pool.Acquire(); acct != nil {
		t.Fatalf("Acquire() = %v, want nil when all accounts are limited", acct)
	}
	wait := pool.EarliestRateLimitWait(time.Now())
	if wait <= 0 || wait > defaultRateLimitCooldown+time.Second {
		t.Fatalf("EarliestRateLimitWait() = %v", wait)
	}
}

func TestAccountRecordFailureTracksAuthFailures(t *testing.T) {
	a := newAccount("x", "key-x", true)
	a.RecordFailure(&upstreamAPIError{Status: http.StatusUnauthorized, Message: "bad key"})
	a.RecordFailure(&upstreamAPIError{Status: http.StatusForbidden, Message: "no"})
	view := a.View()
	if view.Status != "ok" || view.AuthFailures != 2 {
		t.Fatalf("view = %+v, want ok with 2 auth failures", view)
	}
	a.RecordSuccess()
	view = a.View()
	if view.AuthFailures != 0 || view.LastError != "" {
		t.Fatalf("success did not reset state: %+v", view)
	}
}

func TestAccountPoolConfigRoundTrip(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b")
	pool.SetEnabled(accountID("key-b"), false)
	pool.Rename(accountID("key-a"), "primary")

	cfg := &Config{}
	pool.SyncToConfig(cfg)
	if len(cfg.CommandCode.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cfg.CommandCode.Accounts))
	}
	if cfg.CommandCode.Accounts[0].Name != "primary" {
		t.Fatalf("name = %q", cfg.CommandCode.Accounts[0].Name)
	}
	if cfg.CommandCode.Accounts[1].IsEnabled() {
		t.Fatalf("disabled account did not round-trip")
	}

	reloaded := NewAccountPool(cfg.CommandCode.Accounts)
	if reloaded.Len() != 2 || reloaded.EnabledCount() != 1 {
		t.Fatalf("reloaded pool = %d/%d, want 2/1", reloaded.Len(), reloaded.EnabledCount())
	}
}

func TestLoadConfigMigratesLegacySingleKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlData := "api_key: ccgw-local\ncommandcode:\n  api_key: cc-legacy\n  base_url: https://api.commandcode.ai\n"
	if err := os.WriteFile(path, []byte(yamlData), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CommandCode.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.CommandCode.Accounts))
	}
	if cfg.CommandCode.Accounts[0].APIKey != "cc-legacy" || cfg.CommandCode.Accounts[0].Name != "default" {
		t.Fatalf("migrated account = %+v", cfg.CommandCode.Accounts[0])
	}
	if !cfg.CommandCode.Accounts[0].IsEnabled() {
		t.Fatalf("migrated account should be enabled")
	}
}

func TestSaveConfigClearsLegacyKeyWhenAccountsExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{APIKey: "ccgw-local"}
	cfg.CommandCode.APIKey = "cc-legacy"
	cfg.CommandCode.Accounts = []AccountConfig{{Name: "main", APIKey: "cc-new"}}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "cc-legacy") {
		t.Fatalf("legacy key should not be written when accounts exist:\n%s", data)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CommandCode.Accounts) != 1 || loaded.CommandCode.Accounts[0].APIKey != "cc-new" {
		t.Fatalf("reloaded accounts = %+v", loaded.CommandCode.Accounts)
	}
}

func TestCCClientFailoverOnRateLimit(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, key)
		if key == "key-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":1,\"completionTokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)

	resp, acct, err := client.Send(context.Background(), chatRequestForTest())
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	resp.Body.Close()
	if acct == nil || acct.APIKey != "key-b" {
		t.Fatalf("served by %v, want key-b", acct)
	}
	if len(seen) != 2 || seen[0] != "key-a" || seen[1] != "key-b" {
		t.Fatalf("upstream saw %v, want [key-a key-b]", seen)
	}
	if !client.Pool.Get(accountID("key-a")).RateLimited(time.Now()) {
		t.Fatalf("key-a should be rate limited after a 429")
	}

	// Second request must skip the cooling account entirely.
	seen = nil
	resp, acct, err = client.Send(context.Background(), chatRequestForTest())
	if err != nil {
		t.Fatalf("second Send error: %v", err)
	}
	resp.Body.Close()
	if acct == nil || acct.APIKey != "key-b" {
		t.Fatalf("second request served by %v, want key-b", acct)
	}
	if len(seen) != 1 || seen[0] != "key-b" {
		t.Fatalf("second request hit upstream %v, want [key-b]", seen)
	}
}

func TestCCClientNoFailoverOnInvalidRequest(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad messages"}`))
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)
	_, acct, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 upstreamAPIError", err)
	}
	if acct == nil || acct.APIKey != "key-a" {
		t.Fatalf("account = %v, want key-a", acct)
	}
	if calls != 1 {
		t.Fatalf("upstream called %d times, want 1 (no failover on 400)", calls)
	}
}

func TestCCClientAllAccountsRateLimited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called when every account is cooling down")
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)
	for _, key := range []string{"key-a", "key-b"} {
		client.Pool.Get(accountID(key)).RecordFailure(&upstreamAPIError{
			Status:     http.StatusTooManyRequests,
			RetryAfter: "42",
		})
	}

	_, _, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %v, want upstreamAPIError", err)
	}
	if upstreamErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", upstreamErr.Status)
	}
	if upstreamErr.RetryAfter != "42" {
		t.Fatalf("RetryAfter = %q, want 42 (earliest recovery)", upstreamErr.RetryAfter)
	}
}

func TestCCClientNoEnabledAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called")
	}))
	defer upstream.Close()

	pool := poolWithKeys("key-a")
	pool.SetEnabled(accountID("key-a"), false)
	client := NewCCClientWithPool(pool, upstream.URL)

	_, _, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want 503 upstreamAPIError", err)
	}
}

func TestShouldFailoverMatrix(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&upstreamAPIError{Status: 401}, true},
		{&upstreamAPIError{Status: 403}, true},
		{&upstreamAPIError{Status: 429}, true},
		{&upstreamAPIError{Status: 500}, true},
		{&upstreamAPIError{Status: 503}, true},
		{&upstreamAPIError{Status: 400}, false},
		{&upstreamAPIError{Status: 404}, false},
		{&upstreamAPIError{Status: 422}, false},
		{&invalidRequestError{message: "bad"}, false},
		{fmt.Errorf("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := shouldFailover(tc.err); got != tc.want {
			t.Fatalf("shouldFailover(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestUsageForAccountRecordsSeparately(t *testing.T) {
	usage := &UsageTracker{}
	acct := newAccount("main", "key-a", true)

	usage.ForAccount(acct).Record(10, 20, 0, 0)
	usage.ForAccount(acct).Record(1, 2, 3, 0)
	usage.ForAccount(nil).Record(100, 100, 0, 0)

	snap := usage.Snapshot()
	if snap.TotalRequests != 3 || snap.PromptTokens != 111 || snap.CompletionTokens != 122 {
		t.Fatalf("totals = %+v", snap)
	}
	acc := usage.AccountUsage(acct.ID)
	if acc.Requests != 2 || acc.PromptTokens != 11 || acc.CompletionTokens != 22 || acc.CacheReadTokens != 3 {
		t.Fatalf("account usage = %+v", acc)
	}

	usage.DropAccount(acct.ID)
	if got := usage.AccountUsage(acct.ID); got.Requests != 0 {
		t.Fatalf("dropped account usage = %+v", got)
	}
}

func TestUsageSnapshotPersistsAccounts(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })

	usage := &UsageTracker{}
	usage.ForAccount(newAccount("main", "key-a", true)).Record(5, 6, 0, 0)
	if err := usage.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadUsage()
	acc := reloaded.AccountUsage(accountID("key-a"))
	if acc.Requests != 1 || acc.PromptTokens != 5 || acc.CompletionTokens != 6 {
		t.Fatalf("persisted account usage = %+v", acc)
	}
}

func TestLogRingAfterSequence(t *testing.T) {
	ring := newLogRing()
	fmt.Fprint(ring, "first line\n")
	fmt.Fprint(ring, "\x1b[31mred line\x1b[0m\n")
	fmt.Fprint(ring, "third line\n")

	entries, last := ring.After(0)
	if len(entries) != 3 || last != 3 {
		t.Fatalf("entries = %d, last = %d, want 3/3", len(entries), last)
	}
	if entries[1].Line != "red line" {
		t.Fatalf("ANSI not stripped: %q", entries[1].Line)
	}

	entries, _ = ring.After(2)
	if len(entries) != 1 || entries[0].Line != "third line" {
		t.Fatalf("After(2) = %+v", entries)
	}
}
