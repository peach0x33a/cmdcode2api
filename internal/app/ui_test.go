package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsLocalAdminRequestAllowsLoopbackWithMatchingHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:11434"

	if !isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = false, want true for loopback + matching Host")
	}
}

func TestIsLocalAdminRequestAllowsLocalhostHostname(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "[::1]:54321"
	req.Host = "localhost:11434"

	if !isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = false, want true for loopback + localhost Host")
	}
}

func TestIsLocalAdminRequestRejectsNonLoopbackRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Host = "localhost:11434"

	if isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = true, want false for non-loopback RemoteAddr")
	}
}

// TestIsLocalAdminRequestRejectsDNSRebindingHost is the core DNS-rebinding
// defense: an attacker who gets a victim's browser to resolve an
// attacker-controlled hostname to 127.0.0.1 can make the TCP connection
// loopback, but cannot make the browser's Host header say anything other
// than that attacker hostname.
func TestIsLocalAdminRequestRejectsDNSRebindingHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "attacker.example:11434"

	if isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = true, want false for a non-localhost Host header")
	}
}

func TestIsLocalAdminRequestRejectsMismatchedOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/accounts/delete", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	req.Header.Set("Origin", "https://evil.example")

	if isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = true, want false when Origin does not match scheme://Host")
	}
}

func TestIsLocalAdminRequestAllowsMatchingOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/accounts/delete", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	req.Header.Set("Origin", "http://localhost:11434")

	if !isLocalAdminRequest(req) {
		t.Fatal("isLocalAdminRequest() = false, want true when Origin matches scheme://Host")
	}
}

func TestAccountsAllowsLoopbackWithoutBearerToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (loopback should bypass bearer auth); body = %s", rec.Code, rec.Body.String())
	}
}

func TestAccountsRejectsNonLoopbackWithoutBearerToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a non-loopback caller with no bearer token", rec.Code)
	}
}

func TestCorsMiddlewareOmitsWildcardOnAdminRoutes(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty on admin routes", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want %q on admin routes", got, "Origin")
	}
}

func TestCorsMiddlewareKeepsWildcardOnPublicRoutes(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestAccountsDeleteRemovesFromStoreAndPool(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	store := testStore(t)
	if err := store.Upsert(Account{Name: "a", APIKey: "key", BaseURL: "http://a.example"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	reauthMgr := noopReauthManager()
	handler := newHandler(pool, cfg, &UsageTracker{}, reauthMgr, store, NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/delete", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if pool.find("a") != nil {
		t.Fatal("account still present in pool after delete")
	}
	existed, err := store.Remove("a")
	if err != nil {
		t.Fatalf("store.Remove: %v", err)
	}
	if existed {
		t.Fatal("account still present in store after delete")
	}
}

func TestAccountsDeleteReturns404ForUnknownAccount(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/delete", strings.NewReader(`{"name":"nope"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAccountsDeleteRequiresJSONContentType(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/delete", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 without a JSON Content-Type", rec.Code)
	}
	if pool.find("a") == nil {
		t.Fatal("account was removed despite missing Content-Type check")
	}
}

func TestAccountsProbeReturnsFreshSnapshot(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool(testAccountEntry{Name: "a", Client: NewCCClient("key", "http://a.example")})
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/accounts/probe", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUsageAllowsLoopbackWithoutBearerToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (loopback should bypass bearer auth); body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUsageRejectsNonLoopbackWithoutBearerToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a non-loopback caller with no bearer token", rec.Code)
	}
}

func TestAdminUsageRejectsWrongMethod(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodPost, "/admin/usage", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestUIServesIndexHTML(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := newTestAccountPool()
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t), NewBillingTracker())

	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:11434"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Fatalf("body does not look like the UI shell: %s", body)
	}
}
