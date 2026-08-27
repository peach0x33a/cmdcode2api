package app

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthMiddlewareRejectsMissingTokenWithCorsHeaders(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	handler := corsMiddleware(authMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing cors header")
	}
}

func TestCorsPreflightBypassesAuth(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	handler := corsMiddleware(authMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})))

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAdminAlertsGetPreservesPersistedDisabledState(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{DiscordWebhookURL: "https://legacy.example/webhook", DiscordAlerts: DiscordAlertConfig{
		Enabled: false, WebhookURL: "https://discord.example/webhook",
		HourlyCap: 3, WeeklyCap: 6, MonthlyCap: 10,
	}}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	store := NewConfigStore(path, loaded)
	req := httptest.NewRequest(http.MethodGet, "/admin/alerts", nil)
	rec := httptest.NewRecorder()
	handleAdminDiscordAlerts(loaded, store, NewBillingTracker()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got discordAlertsView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Fatalf("enabled = true, want persisted false")
	}
	if got.WebhookURL == cfg.DiscordAlerts.WebhookURL || got.WebhookURL == "" {
		t.Fatalf("webhook_url = %q, want a non-empty masked value", got.WebhookURL)
	}
	if !got.WebhookConfigured {
		t.Fatal("webhook_configured = false, want true")
	}
	if strings.Contains(rec.Body.String(), cfg.DiscordAlerts.WebhookURL) {
		t.Fatal("response exposed the webhook URL")
	}
}

func TestAdminAlertsUpdateBlankWebhookKeepsExisting(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{DiscordAlerts: DiscordAlertConfig{Enabled: true, WebhookURL: "https://discord.example/original", HourlyCap: 3, WeeklyCap: 6, MonthlyCap: 10}}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	store := NewConfigStore(path, loaded)
	body := `{"enabled":true,"webhook_url":"","hourly_cap":3,"weekly_cap":6,"monthly_cap":10}`
	req := httptest.NewRequest(http.MethodPost, "/admin/alerts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAdminDiscordAlerts(loaded, store, NewBillingTracker()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	restarted, err := loadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if restarted.DiscordAlerts.WebhookURL != cfg.DiscordAlerts.WebhookURL {
		t.Fatalf("webhook_url = %q, want %q", restarted.DiscordAlerts.WebhookURL, cfg.DiscordAlerts.WebhookURL)
	}
}

func TestAdminAlertsClearWebhookDisablesConfiguration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{DiscordAlerts: DiscordAlertConfig{Enabled: true, WebhookURL: "https://discord.example/original", HourlyCap: 3, WeeklyCap: 6, MonthlyCap: 10}}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	store := NewConfigStore(path, loaded)
	body := `{"enabled":false,"webhook_url":"","clear_webhook":true,"hourly_cap":3,"weekly_cap":6,"monthly_cap":10}`
	req := httptest.NewRequest(http.MethodPost, "/admin/alerts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAdminDiscordAlerts(loaded, store, NewBillingTracker()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	restarted, err := loadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if restarted.DiscordAlerts.Enabled || restarted.DiscordAlerts.WebhookURL != "" {
		t.Fatalf("alerts after clear = %#v", restarted.DiscordAlerts)
	}
}

func TestAdminAlertsDisableClearsLegacyWebhookAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{DiscordWebhookURL: "https://legacy.example/webhook"}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	store := NewConfigStore(path, loaded)
	body := `{"enabled":false,"webhook_url":"https://legacy.example/webhook","hourly_cap":3,"weekly_cap":6,"monthly_cap":10}`
	req := httptest.NewRequest(http.MethodPost, "/admin/alerts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleAdminDiscordAlerts(loaded, store, NewBillingTracker()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	restarted, err := loadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if restarted.DiscordWebhookURL != "" {
		t.Fatalf("legacy webhook = %q, want empty", restarted.DiscordWebhookURL)
	}
	if restarted.DiscordAlerts.Enabled {
		t.Fatal("alerts enabled after restart")
	}
}

// noopReauthManager builds a ReauthManager with an inert onSuccess callback,
// for tests that only exercise routing/auth and don't care what happens
// after a reauth resolves.
func noopReauthManager() *ReauthManager {
	return NewReauthManager(func(name string, cb oauthCallback) {})
}

// testStore builds a ConfigStore backed by a throwaway config file in a temp
// directory, for tests that only need newHandler's store parameter to be
// non-nil and functional.
func testStore(t *testing.T) *ConfigStore {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	return NewConfigStore(path, &Config{})
}

func TestAccountsRequiresAuth(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAccountsReturnsSnapshotWithValidToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("super-secret-key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got []AccountView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("accounts = %#v", got)
	}
	if strings.Contains(rec.Body.String(), "super-secret-key") {
		t.Fatalf("accounts response leaked api key: %s", rec.Body.String())
	}
}

func TestAvailableModelsUsesCatalog(t *testing.T) {
	modelCatalog = []ModelInfo{
		{ID: "test-model-1", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "test-model-2", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	}

	models := availableModels()
	if len(models) != len(modelCatalog) {
		t.Fatalf("models len = %d, catalog len = %d", len(models), len(modelCatalog))
	}
	if models[0] != modelCatalog[0].ID {
		t.Fatalf("first model = %q", models[0])
	}
}

func TestStatusRecorderCapturesExplicitAndImplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	status, wrapped := newStatusRecorder(rec)

	wrapped.WriteHeader(http.StatusNoContent)
	wrapped.WriteHeader(http.StatusInternalServerError)
	if status.status != http.StatusNoContent {
		t.Fatalf("status = %d", status.status)
	}

	rec = httptest.NewRecorder()
	status, wrapped = newStatusRecorder(rec)
	if _, err := wrapped.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if status.status != http.StatusOK {
		t.Fatalf("implicit status = %d", status.status)
	}
	if status.bytes != 2 {
		t.Fatalf("bytes = %d", status.bytes)
	}
}

func TestFormatHTTPLogSanitizesControlCharacters(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	line := formatHTTPLog(http.MethodGet, "/safe\n\x1b[31mforged", http.StatusOK, 2*time.Millisecond, "127.0.0.1:12345")
	if strings.Contains(line, "\n") || strings.Contains(line, "\x1b[") {
		t.Fatalf("log contains raw control characters: %q", line)
	}
	if !strings.Contains(line, `\n`) || !strings.Contains(line, `\x1b`) {
		t.Fatalf("log did not escape control characters: %q", line)
	}
}

func TestLoggingMiddlewareLogsMethodPathStatusAndDuration(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	oldWriter := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(oldWriter)

	handler := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "nope")
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	for _, want := range []string{"[HTTP]", "GET", "/v1/models", "401", "127.0.0.1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("NO_COLOR log contains ANSI sequence: %q", line)
	}
}

func TestLoggingMiddlewareSkipsHealth(t *testing.T) {
	oldWriter := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(oldWriter)

	handler := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if buf.Len() != 0 {
		t.Fatalf("health log = %q", buf.String())
	}
}

func TestAccountsReauthPostRequiresAuth(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/reauth", strings.NewReader(`{"name":"a"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAccountsReauthPostStartsPendingSessionWithAuthURL(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/reauth", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ReauthSession
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != ReauthPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.AuthURL == "" {
		t.Fatal("auth_url is empty")
	}
}

func TestAccountsReauthGetRequiresAuth(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts/reauth?name=a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The server must come up cleanly with zero accounts configured — the
// first account is meant to be added entirely through /ui, so none of the
// core routes may 500/panic before that happens.
func TestServerServesCoreRoutesWithZeroAccountsConfigured(t *testing.T) {
	cfg := testModelEnabledConfig()
	cfg.APIKey = "secret"
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	// /ui must render for a loopback browser request with no accounts yet.
	uiReq := httptest.NewRequest(http.MethodGet, "/ui", nil)
	uiReq.RemoteAddr = "127.0.0.1:54321"
	uiReq.Host = "localhost:11434"
	uiRec := httptest.NewRecorder()
	handler.ServeHTTP(uiRec, uiReq)
	if uiRec.Code != http.StatusOK {
		t.Fatalf("/ui status = %d, want 200; body = %s", uiRec.Code, uiRec.Body.String())
	}

	// /accounts must return an empty list, not an error.
	acctReq := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	acctReq.Header.Set("Authorization", "Bearer secret")
	acctRec := httptest.NewRecorder()
	handler.ServeHTTP(acctRec, acctReq)
	if acctRec.Code != http.StatusOK {
		t.Fatalf("/accounts status = %d, want 200; body = %s", acctRec.Code, acctRec.Body.String())
	}
	var accounts []AccountView
	if err := json.NewDecoder(acctRec.Body).Decode(&accounts); err != nil {
		t.Fatalf("decode /accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %#v, want empty", accounts)
	}

	// /v1/chat/completions must fail cleanly (not panic) with a message
	// pointing the user at /ui.
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Authorization", "Bearer secret")
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/v1/chat/completions status = %d, want 503; body = %s", chatRec.Code, chatRec.Body.String())
	}
	if !strings.Contains(chatRec.Body.String(), "no accounts configured") {
		t.Fatalf("/v1/chat/completions body missing guidance: %s", chatRec.Body.String())
	}
}

// TestFaviconServesWithoutAuth guards against a regression where browsers'
// automatic, unauthenticated GET /favicon.ico requests (sent for any page on
// the origin, including non-loopback hosts, with no Authorization header)
// fell through to authMiddleware's bearer-token check and got a 401 instead
// of an icon.
func TestFaviconServesWithoutAuth(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	req.RemoteAddr = "203.0.113.5:54321" // non-loopback: no admin bypass applies either
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/favicon.ico status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/") && !strings.Contains(ct, "icon") {
		t.Fatalf("/favicon.ico Content-Type = %q, want an image type", ct)
	}
}

// TestUsageEndpointShapeUnchanged guards that the unauthenticated GET /usage
// endpoint stays byte-identical to its pre-per-account-tracking shape: it
// must never gain an "accounts" key, since that would leak account names
// without the /admin/usage loopback gate. Per-account data is only exposed
// via /admin/usage (see admin.go).
func TestUsageEndpointShapeUnchanged(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "\"accounts\"") {
		t.Fatalf("/usage body unexpectedly contains an accounts key: %s", body)
	}
}

func TestAdminModelsRejectsNonLoopbackRemoteAddr(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminModelsRejectsBadHostHeader(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "attacker.example:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminModelsAllowsLoopbackWithoutOrigin(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsRejectsCrossOriginRequest(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminModelsRejectsWrongMethod(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// cfg.ExcludeModels is a legacy field no longer consulted: without an
// explicit model_overrides entry, adminModels flags every model as excluded,
// regardless of ExcludeModels.
func TestAdminModelsAllExcludedByDefault(t *testing.T) {
	oldCatalog, oldDetail := modelCatalog, modelCatalogDetail
	t.Cleanup(func() { modelCatalog, modelCatalogDetail = oldCatalog, oldDetail })
	modelCatalogDetail = []CCProviderModel{
		{ID: "claude-3-opus", Name: "Claude 3 Opus", ContextLength: 200000},
		{ID: "llama-3-70b", Name: "Llama 3 70B", ContextLength: 8192},
	}

	cfg := &Config{APIKey: "secret", ExcludeModels: []string{"claude-"}}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got AdminModelList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 2 {
		t.Fatalf("data = %#v, want 2 entries (excluded ones are flagged, not omitted)", got.Data)
	}
	byID := map[string]AdminModel{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	if !byID["claude-3-opus"].Excluded {
		t.Fatalf("claude-3-opus.Excluded = false, want true")
	}
	if !byID["llama-3-70b"].Excluded {
		t.Fatalf("llama-3-70b.Excluded = false, want true (disabled by default, ExcludeModels no longer consulted)")
	}
	if byID["llama-3-70b"].Name != "Llama 3 70B" || byID["llama-3-70b"].ContextLength != 8192 {
		t.Fatalf("llama-3-70b = %#v, missing name/context_length", byID["llama-3-70b"])
	}
}

// An explicit model_overrides entry is the only way to enable a model in
// adminModels' output.
func TestAdminModelsEnabledViaExplicitOverride(t *testing.T) {
	oldCatalog, oldDetail := modelCatalog, modelCatalogDetail
	t.Cleanup(func() { modelCatalog, modelCatalogDetail = oldCatalog, oldDetail })
	modelCatalogDetail = []CCProviderModel{
		{ID: "claude-3-opus", Name: "Claude 3 Opus", ContextLength: 200000},
		{ID: "llama-3-70b", Name: "Llama 3 70B", ContextLength: 8192},
	}

	cfg := &Config{APIKey: "secret", ModelOverrides: map[string]bool{"llama-3-70b": true}}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got AdminModelList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]AdminModel{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	if byID["llama-3-70b"].Excluded {
		t.Fatalf("llama-3-70b.Excluded = true, want false")
	}
	if !byID["claude-3-opus"].Excluded {
		t.Fatalf("claude-3-opus.Excluded = false, want true")
	}
}

func TestAdminConnectionRejectsNonLoopbackRemoteAddr(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "localhost", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminConnectionRejectsBadHostHeader(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "localhost", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "attacker.example:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminConnectionRejectsCrossOriginRequest(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "localhost", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminConnectionRejectsWrongMethod(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "localhost", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestAdminConnectionReturnsConfiguredAPIKeyNotAccountKeys(t *testing.T) {
	cfg := &Config{
		APIKey: "gateway-secret",
		Host:   "localhost",
		Port:   11434,
		Accounts: []Account{
			{Name: "a", APIKey: "per-account-cc-key", BaseURL: "http://a.example"},
		},
	}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("per-account-cc-key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ConnectionInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.APIKey != "gateway-secret" {
		t.Fatalf("api_key = %q, want gateway-secret", got.APIKey)
	}
	if got.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("base_url = %q", got.BaseURL)
	}
	if strings.Contains(rec.Body.String(), "per-account-cc-key") {
		t.Fatalf("connection response leaked per-account api key: %s", rec.Body.String())
	}
}

// TestAdminConnectionReflectsRequestHostNotConfiguredHost guards the
// allow_lan/allow_tailscale case: once cfg.Host is a wildcard bind like
// 0.0.0.0, the Setup tab must echo back whatever host the caller actually
// used (from the request's Host header), not the unreachable bind address.
func TestAdminConnectionReflectsRequestHostNotConfiguredHost(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "0.0.0.0", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.RemoteAddr = "192.168.1.77:54321"
	req.Host = "192.168.1.50:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ConnectionInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Host != "192.168.1.50" {
		t.Fatalf("host = %q, want 192.168.1.50", got.Host)
	}
	if got.BaseURL != "http://192.168.1.50:11434/v1" {
		t.Fatalf("base_url = %q, want http://192.168.1.50:11434/v1", got.BaseURL)
	}
}

// When both allow_lan and allow_tailscale are on and both IPs were detected
// at startup, the Setup tab must be able to show both alongside whatever
// host the caller actually used — not just one or the other.
func TestAdminConnectionIncludesBothLANAndTailscaleWhenBothDetected(t *testing.T) {
	cfg := &Config{
		APIKey:              "secret",
		Port:                11434,
		AllowLAN:            true,
		AllowTailscale:      true,
		DetectedLANIP:       "192.168.1.50",
		DetectedTailscaleIP: "100.101.102.103",
	}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ConnectionInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LANBaseURL != "http://192.168.1.50:11434/v1" {
		t.Fatalf("lan_base_url = %q, want http://192.168.1.50:11434/v1", got.LANBaseURL)
	}
	if got.TailscaleBaseURL != "http://100.101.102.103:11434/v1" {
		t.Fatalf("tailscale_base_url = %q, want http://100.101.102.103:11434/v1", got.TailscaleBaseURL)
	}
}

// Neither LAN/Tailscale URL should appear when the flags are off, even if
// stray IPs are somehow present in Config — the flag is what gates whether
// that mode is actually enabled.
func TestAdminConnectionOmitsLANAndTailscaleWhenFlagsOff(t *testing.T) {
	cfg := &Config{
		APIKey:              "secret",
		Port:                11434,
		DetectedLANIP:       "192.168.1.50",
		DetectedTailscaleIP: "100.101.102.103",
	}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ConnectionInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LANBaseURL != "" || got.TailscaleBaseURL != "" {
		t.Fatalf("got = %#v, want both empty with allow_lan/allow_tailscale off", got)
	}
}

// A flag being on doesn't help if the IP was never detected (e.g. no LAN
// interface found at startup) — the field must stay empty rather than
// producing a base URL with an empty host.
func TestAdminConnectionOmitsUndetectedIPsEvenWhenFlagsOn(t *testing.T) {
	cfg := &Config{
		APIKey:         "secret",
		Port:           11434,
		AllowLAN:       true,
		AllowTailscale: true,
	}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/connection", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got ConnectionInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LANBaseURL != "" || got.TailscaleBaseURL != "" {
		t.Fatalf("got = %#v, want both empty with no detected IPs", got)
	}
}

// TestNonLoopbackCallerWithBearerTokenReachesAdminEndpoints documents (and
// guards) the existing behavior that authMiddleware's loopback bypass is a
// tokenless *convenience*, not a hard requirement: a remote caller who
// presents the correct API key can already reach the admin surface today.
// This is what allow_lan/allow_tailscale rely on instead of widening the
// tokenless bypass itself.
func TestNonLoopbackCallerWithBearerTokenReachesAdminEndpoints(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	for _, path := range []string{"/accounts", "/admin/models", "/admin/usage"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.RemoteAddr = "192.168.1.77:54321"
		req.Host = "192.168.1.50:11434"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %s", path, rec.Code, rec.Body.String())
		}
	}
}

// ============================================================================
// POST /admin/models/toggle
// ============================================================================

// adminModelsToggleTestSetup seeds a two-model catalog (both in the same
// family) and returns a handler + the ConfigStore backing it, so tests can
// both make requests and inspect what got persisted.
func adminModelsToggleTestSetup(t *testing.T) (http.Handler, *ConfigStore) {
	t.Helper()
	oldCatalog, oldDetail := modelCatalog, modelCatalogDetail
	t.Cleanup(func() { modelCatalog, modelCatalogDetail = oldCatalog, oldDetail })
	modelCatalogDetail = []CCProviderModel{
		{ID: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextLength: 128000},
		{ID: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextLength: 128000},
	}
	modelCatalog = []ModelInfo{
		{ID: "deepseek/deepseek-v4-pro"},
		{ID: "deepseek/deepseek-v4-flash"},
	}

	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	store := testStore(t)
	policy := NewModelPolicy(cfg)
	handler := newHandlerWithPolicy(pool, cfg, &UsageTracker{}, noopReauthManager(), store, NewBillingTracker(), policy)
	return handler, store
}

func adminRequest(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = "localhost:11434"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestAdminModelsToggleRejectsGet(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodGet, "/admin/models/toggle", "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsMissingContentType(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/models/toggle", strings.NewReader(`{"models":["deepseek/deepseek-v4-pro"],"enabled":false}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsNonLoopback(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"enabled":false}`)
	req.RemoteAddr = "203.0.113.5:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsBadHostHeader(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"enabled":false}`)
	req.Host = "attacker.example:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsCrossOriginRequest(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"enabled":false}`)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsEmptyBody(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsBothModelsAndFamily(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"family":"deepseek/deepseek-v4","enabled":false}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsToggleRejectsUnknownModel(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["nonexistent/model"],"enabled":false}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminModelsTogglePerModelFlipsExcludedAndOverridden(t *testing.T) {
	handler, store := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"enabled":false}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got AdminModelList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]AdminModel{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	pro := byID["deepseek/deepseek-v4-pro"]
	if !pro.Excluded || !pro.Overridden {
		t.Fatalf("pro = %#v, want excluded=true overridden=true", pro)
	}
	flash := byID["deepseek/deepseek-v4-flash"]
	if !flash.Excluded || flash.Overridden {
		t.Fatalf("flash = %#v, want excluded=true overridden=false (untouched sibling, disabled by default)", flash)
	}

	// Persisted through the store, not just held in memory.
	if v, ok := store.cfg.ModelOverrides["deepseek/deepseek-v4-pro"]; !ok || v {
		t.Fatalf("store.cfg.ModelOverrides[deepseek/deepseek-v4-pro] = %v, %v, want false, true", v, ok)
	}
}

func TestAdminModelsToggleFamilyAppliesToAllMembersIncludingUntouchedOnes(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)
	req := adminRequest(http.MethodPost, "/admin/models/toggle", `{"family":"deepseek","enabled":false}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got AdminModelList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range got.Data {
		if !m.Excluded || !m.Overridden {
			t.Fatalf("model %q = %#v, want excluded=true overridden=true (family override)", m.ID, m)
		}
	}
}

// Precedence end-to-end through the HTTP layer: an explicit per-model
// override applied after a family override wins for that one model, and the
// family override still governs its untouched sibling.
func TestAdminModelsTogglePrecedenceModelBeatsFamily(t *testing.T) {
	handler, _ := adminModelsToggleTestSetup(t)

	famReq := adminRequest(http.MethodPost, "/admin/models/toggle", `{"family":"deepseek","enabled":false}`)
	famRec := httptest.NewRecorder()
	handler.ServeHTTP(famRec, famReq)
	if famRec.Code != http.StatusOK {
		t.Fatalf("family toggle status = %d, want 200; body = %s", famRec.Code, famRec.Body.String())
	}

	modelReq := adminRequest(http.MethodPost, "/admin/models/toggle", `{"models":["deepseek/deepseek-v4-pro"],"enabled":true}`)
	modelRec := httptest.NewRecorder()
	handler.ServeHTTP(modelRec, modelReq)
	if modelRec.Code != http.StatusOK {
		t.Fatalf("model toggle status = %d, want 200; body = %s", modelRec.Code, modelRec.Body.String())
	}

	var got AdminModelList
	if err := json.NewDecoder(modelRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]AdminModel{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	if byID["deepseek/deepseek-v4-pro"].Excluded {
		t.Fatalf("pro = %#v, want excluded=false (explicit override beats family)", byID["deepseek/deepseek-v4-pro"])
	}
	if !byID["deepseek/deepseek-v4-flash"].Excluded {
		t.Fatalf("flash = %#v, want excluded=true (still governed by family override)", byID["deepseek/deepseek-v4-flash"])
	}
}

func TestAccountsReauthGetReturns404ForUnknownAccount(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts/reauth?name=nope", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
