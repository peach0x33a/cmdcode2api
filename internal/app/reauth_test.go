package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// parseAuthURL pulls the callback URL and state token back out of an
// AuthURL, the same way a real browser-side OAuth flow would carry them:
// startOAuthListener embeds both as query params (see oauth.go).
func parseAuthURL(t *testing.T, authURL string) (callbackURL, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	callbackURL = q.Get("callback")
	state = q.Get("state")
	if callbackURL == "" || state == "" {
		t.Fatalf("auth url missing callback/state: %s", authURL)
	}
	return callbackURL, state
}

func TestReauthManagerStartReturnsPendingSessionWithAuthURL(t *testing.T) {
	mgr := NewReauthManager(func(name string, cb oauthCallback) {})

	session, err := mgr.Start("go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if session.AuthURL == "" {
		t.Fatal("AuthURL is empty")
	}
	if session.Status != ReauthPending {
		t.Fatalf("Status = %q, want pending", session.Status)
	}
}

func TestReauthManagerStartIsIdempotentWhilePending(t *testing.T) {
	mgr := NewReauthManager(func(name string, cb oauthCallback) {})

	first, err := mgr.Start("go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := mgr.Start("go")
	if err != nil {
		t.Fatalf("Start (again): %v", err)
	}
	if first.AuthURL != second.AuthURL {
		t.Fatalf("AuthURL differs across calls: %q vs %q (started a second competing listener)", first.AuthURL, second.AuthURL)
	}
	if !first.StartedAt.Equal(second.StartedAt) {
		t.Fatalf("StartedAt differs across calls: %v vs %v (started a second competing listener)", first.StartedAt, second.StartedAt)
	}
}

func TestReauthManagerGetReturnsFalseForUnknownAccount(t *testing.T) {
	mgr := NewReauthManager(func(name string, cb oauthCallback) {})
	if _, ok := mgr.Get("nope"); ok {
		t.Fatal("Get() = true, want false for unknown account")
	}
}

// TestReauthManagerResolvesOnCallback exercises the real callback HTTP
// endpoint the started listener is serving, rather than mocking any
// internals: it derives the callback URL and state token from the session's
// public AuthURL (exactly what a real OAuth redirect would carry) and POSTs
// the same JSON body Command Code's studio would send.
func TestReauthManagerResolvesOnCallback(t *testing.T) {
	type successCall struct {
		name, apiKey string
	}
	successCh := make(chan successCall, 1)
	mgr := NewReauthManager(func(name string, cb oauthCallback) {
		successCh <- successCall{name, cb.APIKey}
	})

	session, err := mgr.Start("go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	callbackURL, state := parseAuthURL(t, session.AuthURL)

	body, err := json.Marshal(map[string]string{
		"apiKey":   "test-api-key",
		"state":    state,
		"userName": "tester",
		"keyName":  "key",
	})
	if err != nil {
		t.Fatalf("marshal callback body: %v", err)
	}
	resp, err := http.Post(callbackURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", resp.StatusCode)
	}

	select {
	case call := <-successCh:
		if call.name != "go" || call.apiKey != "test-api-key" {
			t.Fatalf("onSuccess called with = %#v, want {go test-api-key}", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onSuccess was not invoked after a successful callback")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := mgr.Get("go")
		if !ok {
			t.Fatal(`Get("go") = false after a successful callback`)
		}
		snap := got.Snapshot()
		if snap.Status == ReauthSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session status = %q, want success", snap.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
