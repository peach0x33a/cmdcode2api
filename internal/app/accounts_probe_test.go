package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeOneMarksHealthyOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusHealthy {
		t.Fatalf("status = %q, want healthy", snap[0].Status)
	}
	if snap[0].LastChecked == nil || snap[0].LastChecked.IsZero() {
		t.Fatal("lastChecked was not set")
	}
}

func TestProbeOneMarksStaleOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusStale {
		t.Fatalf("status = %q, want stale", snap[0].Status)
	}
	if snap[0].LastError == "" {
		t.Fatal("lastError was not set")
	}
}

func TestProbeOneMarksStaleOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusStale {
		t.Fatalf("status = %q, want stale", snap[0].Status)
	}
	if snap[0].LastError == "" {
		t.Fatal("lastError was not set")
	}
}

// A 500 (or any non-2xx, non-401/403 response) is a transient upstream
// problem, not proof the key itself is dead — probeOne must not flap the
// account's status either way, but it should still record the error so
// operators can see it via /accounts.

func TestProbeOneLeavesUnknownStatusUnchangedOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusUnknown {
		t.Fatalf("status = %q, want unchanged (unknown)", snap[0].Status)
	}
	if snap[0].LastError == "" {
		t.Fatal("lastError was not set even though status did not flip")
	}
}

func TestProbeOneLeavesHealthyStatusUnchangedOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	pool.MarkHealthy("a")
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusHealthy {
		t.Fatalf("status = %q, want unchanged (healthy)", snap[0].Status)
	}
	if snap[0].LastError == "" {
		t.Fatal("lastError was not set even though status did not flip")
	}
}

// TestProbeOneTimesOutWithoutChangingStatus points probeOne at a server that
// never responds. It uses a short parent-context deadline (rather than the
// hardcoded 5s healthCheckTimeout) so the test doesn't actually wait out the
// real timeout: probeOne derives its request context from ctx via
// context.WithTimeout(ctx, healthCheckTimeout), so an already-short parent
// deadline wins.
func TestProbeOneTimesOutWithoutChangingStatus(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond until the test unblocks it during cleanup
	}))
	defer srv.Close()
	defer close(block)

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		pool.probeOne(ctx, acct)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probeOne did not return promptly after its context deadline")
	}

	snap := pool.Snapshot()
	if snap[0].Status != StatusUnknown {
		t.Fatalf("status = %q, want unchanged (unknown)", snap[0].Status)
	}
}

func TestProbeAllUpdatesEachAccountIndependently(t *testing.T) {
	staleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer staleSrv.Close()
	healthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthySrv.Close()

	pool := newTestAccountPool(
		testAccountEntry{Name: "stale", Client: NewCCClient("key", staleSrv.URL)},
		testAccountEntry{Name: "healthy", Client: NewCCClient("key", healthySrv.URL)},
	)

	pool.ProbeAll(context.Background())

	var staleStatus, healthyStatus AccountStatus
	for _, v := range pool.Snapshot() {
		switch v.Name {
		case "stale":
			staleStatus = v.Status
		case "healthy":
			healthyStatus = v.Status
		}
	}
	if staleStatus != StatusStale {
		t.Fatalf("stale account status = %q, want stale (must not be affected by the other account's response)", staleStatus)
	}
	if healthyStatus != StatusHealthy {
		t.Fatalf("healthy account status = %q, want healthy (must not be affected by the other account's response)", healthyStatus)
	}
}

// TestStartHealthChecksProbesImmediatelyAndStopsOnCancel checks the two
// documented behaviors without waiting out a real (10-minute) ticker
// interval: a short interval is passed instead, and every wait in this test
// is guarded by an explicit deadline so a regression can't hang the suite.
func TestStartHealthChecksProbesImmediatelyAndStopsOnCancel(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})

	ctx, cancel := context.WithCancel(context.Background())
	pool.StartHealthChecks(ctx, 15*time.Millisecond)

	waitUntil := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&hits) < 1 {
		if time.Now().After(waitUntil) {
			t.Fatal("StartHealthChecks did not probe immediately at startup")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Let the ticker fire a few more times before cancelling, so a failure to
	// stop is distinguishable from "never ticked in the first place".
	time.Sleep(60 * time.Millisecond)
	cancel()
	countAtCancel := atomic.LoadInt32(&hits)

	time.Sleep(100 * time.Millisecond)
	countAfter := atomic.LoadInt32(&hits)
	if countAfter > countAtCancel+1 {
		t.Fatalf("StartHealthChecks kept probing after ctx was cancelled: hits at cancel=%d, after=%d", countAtCancel, countAfter)
	}
}
