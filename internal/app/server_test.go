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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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

// TestUsageEndpointShapeUnchanged guards that the unauthenticated GET /usage
// endpoint stays byte-identical to its pre-per-account-tracking shape: it
// must never gain an "accounts" key, since that would leak account names
// without the /admin/usage loopback gate. Per-account data is only exposed
// via /admin/usage (see admin.go).
func TestUsageEndpointShapeUnchanged(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

	req := httptest.NewRequest(http.MethodPost, "/admin/models", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestAdminModelsRespectsExcludeModels(t *testing.T) {
	oldCatalog, oldDetail := modelCatalog, modelCatalogDetail
	t.Cleanup(func() { modelCatalog, modelCatalogDetail = oldCatalog, oldDetail })
	modelCatalogDetail = []CCProviderModel{
		{ID: "claude-3-opus", Name: "Claude 3 Opus", ContextLength: 200000},
		{ID: "llama-3-70b", Name: "Llama 3 70B", ContextLength: 8192},
	}

	cfg := &Config{APIKey: "secret", ExcludeModels: []string{"claude-"}}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	if byID["llama-3-70b"].Excluded {
		t.Fatalf("llama-3-70b.Excluded = true, want false")
	}
	if byID["llama-3-70b"].Name != "Llama 3 70B" || byID["llama-3-70b"].ContextLength != 8192 {
		t.Fatalf("llama-3-70b = %#v, missing name/context_length", byID["llama-3-70b"])
	}
}

func TestAdminConnectionRejectsNonLoopbackRemoteAddr(t *testing.T) {
	cfg := &Config{APIKey: "secret", Host: "localhost", Port: 11434}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

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

func TestAccountsReauthGetReturns404ForUnknownAccount(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

	req := httptest.NewRequest(http.MethodGet, "/accounts/reauth?name=nope", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
