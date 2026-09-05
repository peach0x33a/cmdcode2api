package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientKeyPoolBasics(t *testing.T) {
	pool := NewClientKeyPool(nil)

	generated, err := pool.Add("auto", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(generated.Key, "ccgw-") {
		t.Fatalf("generated key = %q, want ccgw- prefix", generated.Key)
	}

	custom, err := pool.Add("mine", "ccgw-custom-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if custom.ID != clientKeyID("ccgw-custom-1") {
		t.Fatalf("id = %q", custom.ID)
	}

	if _, err := pool.Add("dup", "ccgw-custom-1", true); err != errDuplicateClientKey {
		t.Fatalf("duplicate error = %v", err)
	}

	if pool.Lookup("ccgw-custom-1") != custom {
		t.Fatal("Lookup lost the custom key")
	}
	if pool.Lookup("nope") != nil {
		t.Fatal("Lookup returned a key for unknown value")
	}

	if !pool.SetEnabled(custom.ID, false) {
		t.Fatal("SetEnabled failed")
	}
	if custom.Enabled {
		t.Fatal("custom should be disabled")
	}

	if !pool.Rename(custom.ID, "renamed") {
		t.Fatal("Rename failed")
	}

	cfg := &Config{}
	pool.SyncToConfig(cfg)
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[1].Name != "renamed" || cfg.APIKeys[1].IsEnabled() {
		t.Fatalf("synced config = %+v", cfg.APIKeys)
	}
	if cfg.APIKey != "" {
		t.Fatal("SyncToConfig must clear the legacy field")
	}

	reloaded := NewClientKeyPool(cfg.APIKeys)
	if reloaded.Len() != 2 || reloaded.EnabledCount() != 1 {
		t.Fatalf("reloaded pool = %d/%d, want 2/1", reloaded.Len(), reloaded.EnabledCount())
	}

	if !pool.Remove(generated.ID) || pool.Len() != 1 {
		t.Fatal("Remove failed")
	}
}

func TestAuthMiddlewareUsesClientKeyPool(t *testing.T) {
	pool := NewClientKeyPool(nil)
	k1, _ := pool.Add("first", "ccgw-key-1", true)
	k2, _ := pool.Add("second", "ccgw-key-2", true)
	pool.SetEnabled(k2.ID, false)

	var seenKeyID string
	handler := authMiddleware(nil, pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKeyID = r.Context().Value(ctxKeyClientKeyID).(string)
		w.WriteHeader(200)
	}))

	get := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get("ccgw-key-1"); code != 200 {
		t.Fatalf("valid key status = %d, want 200", code)
	}
	if seenKeyID != k1.ID {
		t.Fatalf("context key id = %q, want %q", seenKeyID, k1.ID)
	}
	if code := get("ccgw-key-2"); code != 401 {
		t.Fatalf("disabled key status = %d, want 401", code)
	}
	if code := get("ccgw-unknown"); code != 401 {
		t.Fatalf("unknown key status = %d, want 401", code)
	}
	if code := get(""); code != 401 {
		t.Fatalf("missing key status = %d, want 401", code)
	}
	if k1.View().LastUsedAt == nil {
		t.Fatal("RecordUsed not called for the accepted key")
	}
}

func TestAuthMiddlewareLegacyFallback(t *testing.T) {
	cfg := &Config{APIKey: "legacy-secret"}
	handler := authMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer legacy-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("legacy key status = %d, want 200", rec.Code)
	}
}

func TestLoadConfigMigratesLegacyClientKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlData := "api_key: ccgw-legacy-local\ncommandcode:\n  api_key: cc-legacy\n"
	if err := os.WriteFile(path, []byte(yamlData), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != "ccgw-legacy-local" || cfg.APIKeys[0].Name != "default" {
		t.Fatalf("migrated keys = %+v", cfg.APIKeys)
	}
	if len(cfg.CommandCode.Accounts) != 1 {
		t.Fatalf("account migration broke: %+v", cfg.CommandCode.Accounts)
	}
}

func TestSaveConfigClearsLegacyClientKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{APIKey: "ccgw-legacy-local"}
	cfg.APIKeys = []ClientKeyConfig{
		{Name: "default", Key: "ccgw-a"},
		{Name: "extra", Key: "ccgw-b"},
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "ccgw-legacy-local") {
		t.Fatalf("legacy client key should not persist alongside the list:\n%s", data)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.APIKeys) != 2 {
		t.Fatalf("reloaded keys = %+v", loaded.APIKeys)
	}
}

func TestUsageRecorderClientKeyDimension(t *testing.T) {
	usage := &UsageTracker{}

	usage.Recorder("acct-1", "key-1").Record(10, 20, 5, 0)
	usage.Recorder("acct-2", "key-1").Record(1, 2, 0, 0)
	usage.Recorder("", "key-2").Record(100, 100, 0, 0)
	usage.Recorder("acct-1", "").Record(7, 7, 0, 0)

	snap := usage.Snapshot()
	if snap.TotalRequests != 4 || snap.PromptTokens != 118 {
		t.Fatalf("totals = %+v", snap)
	}
	a1 := usage.AccountUsage("acct-1")
	if a1.Requests != 2 || a1.PromptTokens != 17 {
		t.Fatalf("acct-1 = %+v", a1)
	}
	k1 := usage.ClientKeyUsage("key-1")
	if k1.Requests != 2 || k1.PromptTokens != 11 || k1.CacheReadTokens != 5 {
		t.Fatalf("key-1 = %+v", k1)
	}
	k2 := usage.ClientKeyUsage("key-2")
	if k2.Requests != 1 || k2.CompletionTokens != 100 {
		t.Fatalf("key-2 = %+v", k2)
	}

	usage.DropClientKey("key-1")
	if got := usage.ClientKeyUsage("key-1"); got.Requests != 0 {
		t.Fatal("DropClientKey did not drop")
	}
	if got := usage.AccountUsage("acct-1"); got.Requests != 2 {
		t.Fatal("DropClientKey must not touch account counters")
	}
}

func TestUsageSnapshotPersistsClientKeys(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })

	usage := &UsageTracker{}
	usage.Recorder("", "key-1").Record(5, 6, 0, 0)
	if err := usage.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadUsage()
	if got := reloaded.ClientKeyUsage("key-1"); got.Requests != 1 || got.PromptTokens != 5 || got.CompletionTokens != 6 {
		t.Fatalf("persisted client key usage = %+v", got)
	}
}

func TestGenAdminPasswordShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		pw, err := genAdminPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 12 {
			t.Fatalf("password len = %d, want 12", len(pw))
		}
		if strings.ContainsAny(pw, "-O0oIl1") {
			t.Fatalf("password contains ambiguous characters: %q", pw)
		}
		seen[pw] = true
	}
	if len(seen) < 18 {
		t.Fatalf("passwords not sufficiently random: %d unique out of 20", len(seen))
	}
}

func TestClientKeyRecorderFlowThroughHandler(t *testing.T) {
	// A chat request made with a client key must count usage under that key.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":4,\"totalTokens\":7}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	usage := &UsageTracker{}
	pool := NewClientKeyPool(nil)
	key, _ := pool.Add("client", "ccgw-handler-key", true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer ccgw-handler-key")
	ctx := context.WithValue(req.Context(), ctxKeyClientKeyID, key.ID)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	cc := NewCCClient("cc-smoke", upstream.URL)
	handleChatCompletions(cc, &Config{}, usage).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handler status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := usage.ClientKeyUsage(key.ID); got.Requests != 1 || got.PromptTokens != 3 || got.CompletionTokens != 4 {
		t.Fatalf("client key usage = %+v", got)
	}
}
