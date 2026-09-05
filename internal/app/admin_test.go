package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newAdminTestEnv starts an admin API server backed by temp-dir persistence.
func newAdminTestEnv(t *testing.T) (*httptest.Server, *AccountPool, *Config, *UsageTracker, *logRing) {
	t.Helper()

	oldConfigFile := configFile
	configFile = filepath.Join(t.TempDir(), "config.yaml")
	t.Cleanup(func() { configFile = oldConfigFile })

	cfg := &Config{APIKey: "client-key", Host: "localhost", Port: 11434}
	cfg.SetUpstreamBaseURL("https://api.commandcode.test")
	cfg.setAdminPassword("admin-pass-123")
	pool := NewAccountPool(nil)
	cc := NewCCClientWithPool(pool, cfg.UpstreamBaseURL())
	usage := &UsageTracker{}
	ring := newLogRing()

	mux := http.NewServeMux()
	registerAdminRoutes(mux, cc, pool, cfg, usage, ring)
	srv := httptest.NewServer(adminAuth(cfg)(mux))
	t.Cleanup(srv.Close)
	return srv, pool, cfg, usage, ring
}

func adminRequest(t *testing.T, srv *httptest.Server, method, path, password string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp, payload
}

func TestAdminAuthRejectsBadPassword(t *testing.T) {
	srv, _, _, _, _ := newAdminTestEnv(t)

	for _, password := range []string{"", "wrong"} {
		resp, _ := adminRequest(t, srv, "GET", "/admin/api/overview", password, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("password %q: status = %d, want 401", password, resp.StatusCode)
		}
	}
}

func TestAdminAccountLifecycle(t *testing.T) {
	srv, pool, _, usage, _ := newAdminTestEnv(t)

	// Add
	resp, payload := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "main", "api_key": "cc-key-1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d, want 201: %v", resp.StatusCode, payload)
	}
	if payload["key_masked"] == "cc-key-1" || strings.Contains(fmt.Sprint(payload["key_masked"]), "key-1") {
		t.Fatalf("raw key leaked: %v", payload["key_masked"])
	}

	// Duplicate add rejected
	resp, _ = adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "dup", "api_key": "cc-key-1"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", resp.StatusCode)
	}

	// Persisted to config.yaml
	saved, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.CommandCode.Accounts) != 1 || saved.CommandCode.Accounts[0].APIKey != "cc-key-1" {
		t.Fatalf("persisted accounts = %+v", saved.CommandCode.Accounts)
	}

	// List
	_, payload = adminRequest(t, srv, "GET", "/admin/api/accounts", "admin-pass-123", nil)
	list := payload["accounts"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "main" {
		t.Fatalf("list = %v", list)
	}

	id := list[0].(map[string]any)["id"].(string)

	// Disable via PATCH
	_, payload = adminRequest(t, srv, "PATCH", "/admin/api/accounts/"+id, "admin-pass-123",
		map[string]any{"enabled": false})
	if payload["status"] != "disabled" {
		t.Fatalf("patched status = %v, want disabled", payload["status"])
	}
	if pool.Get(id).Enabled {
		t.Fatal("account still enabled in pool")
	}

	// Delete
	resp, _ = adminRequest(t, srv, "DELETE", "/admin/api/accounts/"+id, "admin-pass-123", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if pool.Len() != 0 {
		t.Fatalf("pool len = %d, want 0", pool.Len())
	}
	if got := usage.AccountUsage(id); got.Requests != 0 {
		t.Fatal("usage counters not dropped")
	}

	resp, _ = adminRequest(t, srv, "DELETE", "/admin/api/accounts/"+id, "admin-pass-123", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminOverviewAndLogs(t *testing.T) {
	srv, pool, _, usage, ring := newAdminTestEnv(t)

	pool.Add("main", "cc-key-1", true)
	usage.Record(1, 2, 0, 0)
	modelCatalog = []ModelInfo{{ID: "m1", Object: "model", Created: 1700000000, OwnedBy: "commandcode"}}
	t.Cleanup(func() { modelCatalog = nil })

	resp, payload := adminRequest(t, srv, "GET", "/admin/api/overview", "admin-pass-123", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("overview status = %d", resp.StatusCode)
	}
	if payload["version"] != Version {
		t.Fatalf("version = %v", payload["version"])
	}
	accounts := payload["accounts"].(map[string]any)
	if accounts["total"] != float64(1) || accounts["enabled"] != float64(1) {
		t.Fatalf("accounts summary = %v", accounts)
	}

	// Logs endpoint returns lines written through the registered ring.
	fmt.Fprint(ring, "hello ring\n")
	_, payload = adminRequest(t, srv, "GET", "/admin/api/logs?after=0", "admin-pass-123", nil)
	entries := payload["entries"].([]any)
	if len(entries) != 1 || !strings.Contains(entries[0].(map[string]any)["line"].(string), "hello ring") {
		t.Fatalf("logs payload = %v", payload)
	}
}

func TestAdminSettingsPut(t *testing.T) {
	srv, pool, cfg, _, _ := newAdminTestEnv(t)
	pool.Add("main", "cc-key-1", true)

	resp, payload := adminRequest(t, srv, "PUT", "/admin/api/settings", "admin-pass-123", map[string]any{
		"base_url":       "https://api2.commandcode.test",
		"exclude_models": []string{"gpt-", " claude- "},
		"admin_password": "new-password-9",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("settings status = %d: %v", resp.StatusCode, payload)
	}
	if got := cfg.Excludes(); len(got) != 2 || got[1] != "claude-" {
		t.Fatalf("excludes = %v, want [gpt- claude-]", got)
	}
	if cfg.UpstreamBaseURL() != "https://api2.commandcode.test" {
		t.Fatalf("base_url = %q", cfg.UpstreamBaseURL())
	}
	if !subtleConstantTimeEqual(cfg.adminPassword(), "new-password-9") {
		t.Fatal("admin password not updated")
	}

	saved, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CommandCode.BaseURL != "https://api2.commandcode.test" {
		t.Fatalf("persisted base_url = %q", saved.CommandCode.BaseURL)
	}
	if len(saved.CommandCode.Accounts) != 1 {
		t.Fatalf("settings save must not lose accounts: %+v", saved.CommandCode.Accounts)
	}

	// Old password no longer works.
	resp, _ = adminRequest(t, srv, "GET", "/admin/api/overview", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still accepted, status = %d", resp.StatusCode)
	}

	// Short password rejected.
	resp, _ = adminRequest(t, srv, "PUT", "/admin/api/settings", "new-password-9", map[string]any{
		"admin_password": "short",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("short password status = %d, want 400", resp.StatusCode)
	}
}

func TestNormalizeExcludeModels(t *testing.T) {
	got := normalizeExcludeModels([]string{"gpt-", " claude- ,gemini-", "", ","})
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(got) != len(want) {
		t.Fatalf("got = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}
}

func TestAccountTestKeyProbe(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer cc-good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer upstream.Close()

	result := testAccountKey(upstream.URL, "cc-good")
	if !result.OK || result.Models != 2 || calls != 1 {
		t.Fatalf("good key result = %+v", result)
	}

	result = testAccountKey(upstream.URL, "cc-bad")
	if result.OK || result.Status != http.StatusUnauthorized || result.Error == "" {
		t.Fatalf("bad key result = %+v", result)
	}
}

func TestConfigFileRedirectedForAdminPersistence(t *testing.T) {
	// Guards against admin handlers accidentally writing into the package dir.
	srv, _, _, _, _ := newAdminTestEnv(t)
	adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "x", "api_key": "cc-k"})
	if _, err := os.Stat(configFile); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
}
