package app

import (
	"net/http"
	"testing"
)

func TestAdminModelsExposureRoundTrip(t *testing.T) {
	srv, _, _, cfg, _, _ := newAdminTestEnv(t)
	modelCatalog = []ModelInfo{
		{ID: "model-a", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "model-b", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "vendor/model-c", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	}
	t.Cleanup(func() { modelCatalog = nil })

	// Default: everything exposed when exclude_models is empty.
	_, payload := adminRequest(t, srv, "GET", "/admin/api/models", "admin-pass-123", nil)
	for _, m := range payload["models"].([]any) {
		if !m.(map[string]any)["exposed"].(bool) {
			t.Fatalf("expected all exposed, got %v", payload)
		}
	}

	// Hide model-b and model-c via the exposed list.
	resp, payload := adminRequest(t, srv, "PUT", "/admin/api/models", "admin-pass-123",
		map[string]any{"exposed": []string{"model-a"}})
	if resp.StatusCode != 200 {
		t.Fatalf("put status = %d: %v", resp.StatusCode, payload)
	}
	got := cfg.Excludes()
	if len(got) != 2 || got[0] != "model-b" || got[1] != "vendor/model-c" {
		t.Fatalf("excludes = %v, want [model-b vendor/model-c]", got)
	}

	// GET reflects the new exposure states.
	_, payload = adminRequest(t, srv, "GET", "/admin/api/models", "admin-pass-123", nil)
	states := map[string]bool{}
	for _, m := range payload["models"].([]any) {
		entry := m.(map[string]any)
		states[entry["id"].(string)] = entry["exposed"].(bool)
	}
	if !states["model-a"] || states["model-b"] || states["vendor/model-c"] {
		t.Fatalf("states = %v", states)
	}
}

func TestAdminModelsKeepsNonCatalogPrefixes(t *testing.T) {
	srv, _, _, cfg, _, _ := newAdminTestEnv(t)
	cfg.SetExcludes([]string{"gpt-", "stale-"})

	modelCatalog = []ModelInfo{
		{ID: "gpt-4", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "model-a", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	}
	t.Cleanup(func() { modelCatalog = nil })

	// Expose everything from the catalog; "gpt-" materializes into the
	// concrete catalog IDs it matches, "stale-" (matches nothing loaded) is
	// preserved for chat-time filtering.
	resp, _ := adminRequest(t, srv, "PUT", "/admin/api/models", "admin-pass-123",
		map[string]any{"exposed": []string{"gpt-4", "model-a"}})
	if resp.StatusCode != 200 {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	got := cfg.Excludes()
	if len(got) != 1 || got[0] != "stale-" {
		t.Fatalf("excludes = %v, want only stale-", got)
	}
}

func TestAdminAccountPatchKeyMovesUsage(t *testing.T) {
	srv, pool, _, _, usage, _ := newAdminTestEnv(t)
	acct, _ := pool.Add("main", "cc-old-key", true)
	oldID := acct.ID
	usage.Recorder(oldID, "").Record(9, 8, 0, 0)

	resp, payload := adminRequest(t, srv, "PATCH", "/admin/api/accounts/"+oldID, "admin-pass-123",
		map[string]any{"api_key": "cc-new-key"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch status = %d: %v", resp.StatusCode, payload)
	}
	newID := payload["id"].(string)
	if newID != accountID("cc-new-key") {
		t.Fatalf("new id = %q", newID)
	}
	if usage.AccountUsage(oldID).Requests != 0 {
		t.Fatal("old ID still has counters")
	}
	if got := usage.AccountUsage(newID); got.Requests != 1 || got.PromptTokens != 9 {
		t.Fatalf("moved usage = %+v", got)
	}

	saved := loadConfigForTest(t)
	found := false
	for _, a := range saved.CommandCode.Accounts {
		if a.APIKey == "cc-new-key" && a.Name == "main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("persisted accounts missing updated key: %+v", saved.CommandCode.Accounts)
	}

	// Patching to a key owned by another account is rejected.
	if _, err := pool.Add("other", "cc-taken-key", true); err != nil {
		t.Fatal(err)
	}
	resp, _ = adminRequest(t, srv, "PATCH", "/admin/api/accounts/"+newID, "admin-pass-123",
		map[string]any{"api_key": "cc-taken-key"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate key patch status = %d, want 409", resp.StatusCode)
	}
}

func TestOAuthDisplayNamePrefersUser(t *testing.T) {
	cb := oauthCallback{UserName: "dev@example.com", KeyName: "my-cli-key"}
	if got := cb.displayName(); got != "dev@example.com" {
		t.Fatalf("displayName = %q", got)
	}
	if got := (oauthCallback{KeyName: "only-key"}).displayName(); got != "only-key" {
		t.Fatalf("displayName fallback = %q", got)
	}
	if got := (oauthCallback{}).displayName(); got != "" {
		t.Fatalf("empty displayName = %q", got)
	}
}

func loadConfigForTest(t *testing.T) *Config {
	t.Helper()
	cfg, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
