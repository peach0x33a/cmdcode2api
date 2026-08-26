package app

import (
	"os"
	"sync"
	"testing"
)

func TestRecordForInitializesNilMapOnZeroValueTracker(t *testing.T) {
	u := &UsageTracker{}

	u.RecordFor("acct1", 10, 5, 0, 0)

	report := u.Report()
	if len(report.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want 1 entry", report.Accounts)
	}
	got := report.Accounts[0]
	if got.Account != "acct1" || got.TotalRequests != 1 || got.PromptTokens != 10 || got.CompletionTokens != 5 {
		t.Fatalf("account entry = %#v", got)
	}
}

func TestRecordForEmptyAccountOnlyUpdatesGlobals(t *testing.T) {
	u := &UsageTracker{}

	u.RecordFor("", 10, 5, 0, 0)

	report := u.Report()
	if len(report.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want empty", report.Accounts)
	}
	snap := u.Snapshot()
	if snap.TotalRequests != 1 || snap.PromptTokens != 10 || snap.CompletionTokens != 5 {
		t.Fatalf("snapshot = %#v", snap)
	}
}

func TestReportSortsAccountsByName(t *testing.T) {
	u := &UsageTracker{}

	u.RecordFor("zebra", 1, 1, 0, 0)
	u.RecordFor("alpha", 1, 1, 0, 0)
	u.RecordFor("mike", 1, 1, 0, 0)

	report := u.Report()
	if len(report.Accounts) != 3 {
		t.Fatalf("accounts = %#v, want 3 entries", report.Accounts)
	}
	names := []string{report.Accounts[0].Account, report.Accounts[1].Account, report.Accounts[2].Account}
	want := []string{"alpha", "mike", "zebra"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("accounts order = %v, want %v", names, want)
		}
	}
}

func TestLoadUsageAcceptsLegacyFlatFile(t *testing.T) {
	t.Chdir(t.TempDir())

	legacy := `{
  "total_requests": 3,
  "prompt_tokens": 100,
  "completion_tokens": 50,
  "cache_read_tokens": 10,
  "cache_write_tokens": 5
}`
	if err := os.WriteFile(usageFile, []byte(legacy), 0644); err != nil {
		t.Fatalf("write legacy usage.json: %v", err)
	}

	u := loadUsage()

	snap := u.Snapshot()
	if snap.TotalRequests != 3 || snap.PromptTokens != 100 || snap.CompletionTokens != 50 || snap.CacheReadTokens != 10 || snap.CacheWriteTokens != 5 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if report := u.Report(); len(report.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want empty", report.Accounts)
	}
}

func TestSaveLoadRoundTripsPerAccount(t *testing.T) {
	t.Chdir(t.TempDir())

	u := &UsageTracker{}
	u.RecordFor("acct-a", 10, 5, 1, 2)
	u.RecordFor("acct-b", 20, 15, 3, 4)
	u.RecordFor("acct-a", 1, 1, 0, 0)

	if err := u.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := loadUsage()

	wantSnap := u.Snapshot()
	gotSnap := loaded.Snapshot()
	if gotSnap != wantSnap {
		t.Fatalf("loaded snapshot = %#v, want %#v", gotSnap, wantSnap)
	}

	wantReport := u.Report()
	gotReport := loaded.Report()
	if len(gotReport.Accounts) != len(wantReport.Accounts) {
		t.Fatalf("loaded accounts = %#v, want %#v", gotReport.Accounts, wantReport.Accounts)
	}
	for i := range wantReport.Accounts {
		if gotReport.Accounts[i] != wantReport.Accounts[i] {
			t.Fatalf("loaded account[%d] = %#v, want %#v", i, gotReport.Accounts[i], wantReport.Accounts[i])
		}
	}
}

func TestRecordForConcurrent(t *testing.T) {
	const goroutines = 20
	const perGoroutine = 50
	accounts := []string{"a", "b", "c", "d"}

	u := &UsageTracker{}
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		account := accounts[i%len(accounts)]
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				u.RecordFor(account, 1, 2, 0, 0)
			}
		}(account)
	}
	wg.Wait()

	report := u.Report()
	totalRequests := int64(0)
	for _, acct := range report.Accounts {
		totalRequests += acct.TotalRequests
		if acct.PromptTokens != acct.TotalRequests {
			t.Fatalf("account %s prompt tokens = %d, want %d", acct.Account, acct.PromptTokens, acct.TotalRequests)
		}
		if acct.CompletionTokens != acct.TotalRequests*2 {
			t.Fatalf("account %s completion tokens = %d, want %d", acct.Account, acct.CompletionTokens, acct.TotalRequests*2)
		}
	}
	if want := int64(goroutines * perGoroutine); totalRequests != want {
		t.Fatalf("total per-account requests = %d, want %d", totalRequests, want)
	}
	if snap := u.Snapshot(); snap.TotalRequests != int64(goroutines*perGoroutine) {
		t.Fatalf("global total requests = %d, want %d", snap.TotalRequests, goroutines*perGoroutine)
	}
}
