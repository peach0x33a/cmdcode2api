package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestWebOAuthCallbackFlow walks the full custom-callback OAuth flow: start
// with a browser-reachable callback URL, deliver the credential through the
// public callback endpoint, and observe the account appear.
func TestWebOAuthCallbackFlow(t *testing.T) {
	srv, pool, _, cfg, _, _ := newAdminTestEnv(t)
	t.Cleanup(func() {
		webOAuthMu.Lock()
		webOAuthFlow = nil
		webOAuthMu.Unlock()
	})

	callback := "http://my-server.example:11434/admin/api/oauth/callback"
	resp, payload := adminRequest(t, srv, "POST", "/admin/api/oauth/start", "admin-pass-123",
		map[string]any{"callback_url": callback})
	if resp.StatusCode != 200 {
		t.Fatalf("start status = %d: %v", resp.StatusCode, payload)
	}
	authURL := payload["auth_url"].(string)
	if !strings.HasPrefix(authURL, studioBaseURL+"/studio/auth/cli") || !strings.Contains(authURL, url.QueryEscape(callback)) {
		t.Fatalf("auth url = %q", authURL)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatalf("auth url carries no state: %q", authURL)
	}

	// Wrong state is rejected and leaves the flow pending.
	bad := strings.Replace(state, state[:4], "zzzz", 1)
	ccPost := func(state string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/admin/api/oauth/callback",
			strings.NewReader(`{"apiKey":"cc-oauth-key","state":"`+state+`","userName":"dev@example.com","keyName":"cli"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	if code, _ := ccPost(bad); code != http.StatusBadRequest {
		t.Fatalf("bad state status = %d, want 400", code)
	}

	code, body := ccPost(state)
	if code != 200 || body["success"] != true {
		t.Fatalf("callback = %d %v", code, body)
	}

	_, payload = adminRequest(t, srv, "GET", "/admin/api/oauth/status", "admin-pass-123", nil)
	if payload["state"] != "success" {
		t.Fatalf("status = %v, want success", payload)
	}
	if payload["account_name"] != "dev@example.com" {
		t.Fatalf("account_name = %v (OAuth accounts take the user name)", payload["account_name"])
	}
	if pool.Get(accountID("cc-oauth-key")) == nil {
		t.Fatal("account not in pool")
	}

	saved := loadConfigForTest(t)
	if len(saved.CommandCode.Accounts) != 1 || saved.CommandCode.Accounts[0].APIKey != "cc-oauth-key" {
		t.Fatalf("persisted accounts = %+v", saved.CommandCode.Accounts)
	}
	_ = cfg

	// A flow that is no longer pending rejects further callbacks.
	if code, _ := ccPost(state); code != http.StatusBadRequest {
		t.Fatalf("post-completion callback status = %d, want 400", code)
	}

	// Cancel a pending flow via the API.
	resp, _ = adminRequest(t, srv, "POST", "/admin/api/oauth/start", "admin-pass-123",
		map[string]any{"callback_url": callback})
	if resp.StatusCode != 200 {
		t.Fatalf("second start status = %d", resp.StatusCode)
	}
	resp, _ = adminRequest(t, srv, "POST", "/admin/api/oauth/cancel", "admin-pass-123", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	_, payload = adminRequest(t, srv, "GET", "/admin/api/oauth/status", "admin-pass-123", nil)
	if payload["state"] != "failed" {
		t.Fatalf("status after cancel = %v, want failed", payload)
	}
}

// The callback endpoint must not be reachable through the admin-auth wrapper
// with an invalid password but must stay public without one (state is the
// proof), which the routing handles by registering a more specific pattern.
func TestWebOAuthCallbackIsStateProtected(t *testing.T) {
	srv, _, _, _, _, _ := newAdminTestEnv(t)
	t.Cleanup(func() {
		webOAuthMu.Lock()
		webOAuthFlow = nil
		webOAuthMu.Unlock()
	})

	// No flow started: callback reports no pending flow, without auth.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/api/oauth/callback",
		strings.NewReader(`{"apiKey":"x","state":"y"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no pending flow)", resp.StatusCode)
	}
}
