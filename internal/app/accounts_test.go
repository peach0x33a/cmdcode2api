package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// testAccountEntry names a pre-built *CCClient (e.g. pointed at an
// httptest.Server) for use with newTestAccountPool.
type testAccountEntry struct {
	Name   string
	Client *CCClient
}

// newTestAccountPool builds an AccountPool directly from pre-built clients,
// bypassing NewAccountPool's normal construction-from-config path. Tests use
// this to point accounts at fake httptest servers.
func newTestAccountPool(entries ...testAccountEntry) *AccountPool {
	pool := &AccountPool{}
	for _, e := range entries {
		pool.accounts = append(pool.accounts, &accountState{
			Account: Account{Name: e.Name, APIKey: e.Client.APIKey, BaseURL: e.Client.BaseURL},
			client:  e.Client,
			status:  StatusUnknown,
		})
	}
	return pool
}

func singleAccountPool(client *CCClient) *AccountPool {
	return newTestAccountPool(testAccountEntry{Name: "default", Client: client})
}

// testModelEnabledConfig returns a *Config with an explicit ModelOverrides
// entry enabling "test/test-model" and "test-model" — the model IDs used
// throughout the dispatch tests in this package that exercise behavior past
// the enabled-model gate (failover, usage crediting, rate limits, etc.) and
// aren't themselves testing that gate. Every model is disabled by default,
// so those tests need an explicit override to reach the code they're
// actually testing.
func testModelEnabledConfig() *Config {
	return &Config{ModelOverrides: map[string]bool{
		"test/test-model": true,
		"test-model":      true,
	}}
}

func TestAccountPoolRoundRobinsHealthyAndUnknown(t *testing.T) {
	pool := newTestAccountPool(
		testAccountEntry{Name: "a", Client: NewCCClient("ka", "http://a.example")},
		testAccountEntry{Name: "b", Client: NewCCClient("kb", "http://b.example")},
		testAccountEntry{Name: "c", Client: NewCCClient("kc", "http://c.example")},
	)

	var order []string
	for i := 0; i < 6; i++ {
		acct, err := pool.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		order = append(order, acct.Name)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, name := range want {
		if order[i] != name {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestAccountPoolSkipsStaleAccounts(t *testing.T) {
	pool := newTestAccountPool(
		testAccountEntry{Name: "a", Client: NewCCClient("ka", "http://a.example")},
		testAccountEntry{Name: "b", Client: NewCCClient("kb", "http://b.example")},
	)
	pool.MarkStale("a", "401")

	for i := 0; i < 4; i++ {
		acct, err := pool.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if acct.Name != "b" {
			t.Fatalf("Next() = %q, want b (a is stale)", acct.Name)
		}
	}
}

func TestAccountPoolNextErrorsWhenAllAccountsStale(t *testing.T) {
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("ka", "http://a.example")})
	pool.MarkStale("a", "401")

	if _, err := pool.Next(); err == nil {
		t.Fatal("Next() error = nil, want error when all accounts are stale")
	}
}

func TestAccountPoolNextErrorsOnEmptyPool(t *testing.T) {
	pool := newTestAccountPool()
	if _, err := pool.Next(); err == nil {
		t.Fatal("Next() error = nil, want error on empty pool")
	}
}

func TestAccountPoolMarkHealthyRecoversAccount(t *testing.T) {
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("ka", "http://a.example")})
	pool.MarkStale("a", "401")
	if _, err := pool.Next(); err == nil {
		t.Fatal("Next() error = nil, want error while stale")
	}

	pool.MarkHealthy("a")
	acct, err := pool.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if acct.Name != "a" {
		t.Fatalf("Next() = %q, want a", acct.Name)
	}
}

func TestAccountPoolMarkStaleSetsStatusAndError(t *testing.T) {
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("ka", "http://a.example")})
	pool.MarkStale("a", "invalid api key")

	snapshot := pool.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("len(snapshot) = %d, want 1", len(snapshot))
	}
	if snapshot[0].Status != StatusStale {
		t.Fatalf("status = %q, want %q", snapshot[0].Status, StatusStale)
	}
	if snapshot[0].LastError != "invalid api key" {
		t.Fatalf("lastError = %q, want %q", snapshot[0].LastError, "invalid api key")
	}
	if snapshot[0].LastChecked == nil || snapshot[0].LastChecked.IsZero() {
		t.Fatal("lastChecked was not set")
	}
}

func TestAccountPoolSnapshotOmitsAPIKey(t *testing.T) {
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("super-secret-key", "http://a.example")})

	snapshot := pool.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name != "a" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "super-secret-key") {
		t.Fatalf("snapshot leaked api key: %s", data)
	}
}
