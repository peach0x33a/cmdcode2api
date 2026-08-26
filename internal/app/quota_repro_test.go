package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeOneMissesQuotaExhaustionByDesign documents the bug's root cause:
// probeOne only ever calls the unmetered /provider/v1/models endpoint, so it
// reports an account healthy even when every real completion against it
// would fail with a 429 "usage limit" response (the real Command Code
// quota-exhaustion shape — see TestCCClientSendParsesTopLevelRateLimitError
// in cc_test.go). The health check simply has no way to observe quota state.
func TestProbeOneMissesQuotaExhaustionByDesign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/alpha/generate":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"You've reached your 5-hour usage limit for your plan.","type":"server_error"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusHealthy {
		t.Fatalf("probeOne status = %q, want healthy (the model-list probe genuinely succeeds)", snap[0].Status)
	}
	if snap[0].LastError != "" {
		t.Fatalf("lastError = %q, want empty before any real completion has been attempted", snap[0].LastError)
	}
}

// TestSendWithFailoverRecordsQuotaExhaustionWithoutFlippingStatus is the fix:
// a real completion request that hits Command Code's 429 usage-limit
// response now shows up in /accounts (LastError) and flips status to
// StatusLimited — not "healthy" (the original bug) and not StatusStale,
// since MarkStale's reauthorize guidance would be wrong for a rate limit
// that resets on its own.
func TestSendWithFailoverRecordsQuotaExhaustionWithoutFlippingStatus(t *testing.T) {
	const quotaMessage = "You've reached your 5-hour usage limit for your plan."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"` + quotaMessage + `","type":"server_error"}`))
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	pool.MarkHealthy("a") // simulate a health check that already ran and passed

	_, err := sendWithFailover(context.Background(), pool, &ChatRequest{
		Model:    "test/test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})
	if err == nil {
		t.Fatal("sendWithFailover succeeded, want a 429 quota-exhausted error")
	}
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusTooManyRequests {
		t.Fatalf("error = %#v, want a 429 upstreamAPIError", err)
	}

	snap := pool.Snapshot()
	if snap[0].Status != StatusLimited {
		t.Fatalf("status = %q, want %q — this is the user-visible bug: a usage-limit 429 must not look like \"healthy\"", snap[0].Status, StatusLimited)
	}
	if !strings.Contains(snap[0].LastError, quotaMessage) {
		t.Fatalf("lastError = %q, want it to contain the real upstream quota message %q", snap[0].LastError, quotaMessage)
	}
}

// TestProbeOneDoesNotClearLimitedStatus proves the fix for the second half of
// the bug: the background probe only ever hits the unmetered
// /provider/v1/models endpoint, so a 200 there is not proof completion quota
// recovered. Without this, a limited account's status/lastError would be
// silently wiped within one health-check interval (as little as a few
// minutes) even though every real completion would still 429.
func TestProbeOneDoesNotClearLimitedStatus(t *testing.T) {
	const quotaMessage = "You've reached your 5-hour usage limit for your plan."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	acct := pool.find("a")
	pool.RecordError("a", &upstreamAPIError{
		Status:  http.StatusTooManyRequests,
		Type:    "rate_limit_error",
		Code:    "rate_limit_exceeded",
		Message: quotaMessage,
	})
	if pool.Snapshot()[0].Status != StatusLimited {
		t.Fatalf("precondition: status = %q, want %q", pool.Snapshot()[0].Status, StatusLimited)
	}

	pool.probeOne(context.Background(), acct)

	snap := pool.Snapshot()
	if snap[0].Status != StatusLimited {
		t.Fatalf("status after probeOne = %q, want unchanged (%q) — a 200 from the unmetered model-listing endpoint does not prove completion quota recovered", snap[0].Status, StatusLimited)
	}
	if !strings.Contains(snap[0].LastError, quotaMessage) {
		t.Fatalf("lastError = %q, want the original quota message preserved, got wiped by probeOne", snap[0].LastError)
	}
	if snap[0].LastChecked == nil || snap[0].LastChecked.IsZero() {
		t.Fatal("lastChecked was not updated even though a probe ran")
	}
}

// TestSendWithFailoverSuccessClearsLimitedStatus proves the third leg of the
// fix: only a real successful completion request — not a blind health-check
// probe — is allowed to clear StatusLimited back to healthy.
func TestSendWithFailoverSuccessClearsLimitedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n")))
	}))
	defer srv.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", srv.URL)})
	pool.RecordError("a", &upstreamAPIError{
		Status:  http.StatusTooManyRequests,
		Type:    "rate_limit_error",
		Code:    "rate_limit_exceeded",
		Message: "You've reached your 5-hour usage limit for your plan.",
	})
	if pool.Snapshot()[0].Status != StatusLimited {
		t.Fatalf("precondition: status = %q, want %q", pool.Snapshot()[0].Status, StatusLimited)
	}

	disp, err := sendWithFailover(context.Background(), pool, &ChatRequest{
		Model:    "test/test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})
	if err != nil {
		t.Fatalf("sendWithFailover: %v", err)
	}
	disp.resp.Body.Close()

	snap := pool.Snapshot()
	if snap[0].Status != StatusHealthy {
		t.Fatalf("status = %q, want %q after a real successful completion", snap[0].Status, StatusHealthy)
	}
	if snap[0].LastError != "" {
		t.Fatalf("lastError = %q, want cleared once the account is proven healthy again", snap[0].LastError)
	}
}

// TestSendWithFailoverRecordsNetworkErrorWithoutChangingStatus proves the
// first fix's remaining gap: a network-level failure (here, a connection
// refused because the server is already closed) never produces an
// *upstreamAPIError, but RecordError must still be called with it so
// /accounts doesn't silently show a blank lastError next to "healthy".
func TestSendWithFailoverRecordsNetworkErrorWithoutChangingStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // nothing is listening on deadURL anymore

	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", deadURL)})
	pool.MarkHealthy("a")

	_, err := sendWithFailover(context.Background(), pool, &ChatRequest{
		Model:    "test/test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})
	if err == nil {
		t.Fatal("sendWithFailover succeeded, want a connection error")
	}
	var upstreamErr *upstreamAPIError
	if errors.As(err, &upstreamErr) {
		t.Fatalf("error = %#v, want a plain network error (not *upstreamAPIError)", upstreamErr)
	}

	snap := pool.Snapshot()
	if snap[0].Status != StatusHealthy {
		t.Fatalf("status = %q, want unchanged (%q) — a network error does not mean the key is dead or rate-limited", snap[0].Status, StatusHealthy)
	}
	if snap[0].LastError == "" {
		t.Fatal("lastError was not recorded for a network-level failure")
	}
}
