package app

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type oauthWaitResult struct {
	apiKey string
	err    error
}

// TestOAuthListenerWaitReturnsErrorOnErrorCallback exercises the real
// /callback endpoint startOAuthListener serves, the same way
// TestReauthManagerResolvesOnCallback does for the success path: it POSTs
// to the listener's actual CallbackURL, but with an ?error= query param —
// exactly how the studio-side redirect signals a cancelled/denied
// authorization (see the handler in oauth.go) — and expects Wait() to
// surface that as an error promptly rather than waiting out the timeout.
func TestOAuthListenerWaitReturnsErrorOnErrorCallback(t *testing.T) {
	l, err := startOAuthListener(OAuthOptions{})
	if err != nil {
		t.Fatalf("startOAuthListener: %v", err)
	}

	doneCh := make(chan oauthWaitResult, 1)
	go func() {
		apiKey, err := l.Wait()
		doneCh <- oauthWaitResult{apiKey, err}
	}()

	resp, err := http.Post(l.CallbackURL+"?error=access_denied", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", resp.StatusCode)
	}

	select {
	case res := <-doneCh:
		if res.err == nil {
			t.Fatal("Wait() err = nil, want non-nil after an ?error= callback")
		}
		if res.apiKey != "" {
			t.Fatalf("Wait() apiKey = %q, want empty after an ?error= callback", res.apiKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return promptly after an ?error= callback")
	}
}

// TestOAuthListenerWaitTimesOutAndClosesServer lowers the package-level
// oauthTimeout for the duration of this one test (restoring it via defer,
// since it's a shared var other tests must not see mutated), then confirms
// Wait() both returns a timeout error and actually closes the listener's
// server rather than leaking the bound port.
func TestOAuthListenerWaitTimesOutAndClosesServer(t *testing.T) {
	original := oauthTimeout
	oauthTimeout = 10 * time.Millisecond
	defer func() { oauthTimeout = original }()

	l, err := startOAuthListener(OAuthOptions{})
	if err != nil {
		t.Fatalf("startOAuthListener: %v", err)
	}

	doneCh := make(chan oauthWaitResult, 1)
	go func() {
		apiKey, err := l.Wait()
		doneCh <- oauthWaitResult{apiKey, err}
	}()

	select {
	case res := <-doneCh:
		if res.err == nil {
			t.Fatal("Wait() err = nil, want a timeout error")
		}
		if res.apiKey != "" {
			t.Fatalf("Wait() apiKey = %q, want empty on timeout", res.apiKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not time out within the test's guard window")
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port))
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("port %d is still accepting connections after Wait() timed out; server was not closed", l.Port)
	}
}
