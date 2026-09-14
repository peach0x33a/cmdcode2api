package app

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cmdcode2api/internal/web"
)

func TestWebUIServedUnderWebuiPathOnly(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", handleModels(cfg))
	mux.HandleFunc("/webui", web.Handler())
	mux.HandleFunc("/webui/", web.Handler())
	handler := corsMiddleware(authMiddleware(cfg, nil)(mux))

	// /webui is public and serves the embedded HTML.
	req := httptest.NewRequest(http.MethodGet, "/webui", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/webui status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("/webui content-type = %q", ct)
	}

	// Unknown UI sub-paths keep their 404.
	req = httptest.NewRequest(http.MethodGet, "/webui/nope.js", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/webui/nope.js status = %d, want 404", rec.Code)
	}

	// The root path stays API territory and still requires the bearer token.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/ status = %d, want 401", rec.Code)
	}

	// /v1/models keeps requiring the bearer token.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddlewareRejectsMissingTokenWithCorsHeaders(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	handler := corsMiddleware(authMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := corsMiddleware(authMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})))

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
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

func TestLoggingMiddlewareLogsForwardedClientIP(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	oldWriter := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(oldWriter)

	handler := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:11434"
	req.Header.Set("CF-Connecting-IP", "203.0.113.18")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	if !strings.Contains(line, "203.0.113.18") {
		t.Fatalf("log %q missing forwarded client IP", line)
	}
	if strings.Contains(line, "127.0.0.1") {
		t.Fatalf("log %q contains proxy IP", line)
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

func TestRequestClientIPUsesCloudflareHeaderThroughLoopbackProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:11434"
	req.Header.Set("CF-Connecting-IP", "203.0.113.8")
	req.Header.Set("X-Forwarded-For", "198.51.100.4, 203.0.113.8")

	if got := requestClientIP(req); got != "203.0.113.8" {
		t.Fatalf("client IP = %q, want 203.0.113.8", got)
	}
}

func TestRequestClientIPFallsBackThroughTrustedHeaders(t *testing.T) {
	tests := []struct {
		name string
		cf   string
		xff  string
		real string
		want string
	}{
		{name: "xff", xff: "198.51.100.7, 203.0.113.9", want: "198.51.100.7"},
		{name: "real ip", real: "2001:db8::7", want: "2001:db8::7"},
		{name: "invalid headers", cf: "not-an-ip", xff: "bad, also-bad", real: "bad", want: "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "127.0.0.1:11434"
			req.Header.Set("CF-Connecting-IP", tt.cf)
			req.Header.Set("X-Forwarded-For", tt.xff)
			req.Header.Set("X-Real-IP", tt.real)
			if got := requestClientIP(req); got != tt.want {
				t.Fatalf("client IP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestClientIPIgnoresHeadersFromDirectPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.20:443"
	req.Header.Set("CF-Connecting-IP", "203.0.113.8")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.10")

	if got := requestClientIP(req); got != "198.51.100.20" {
		t.Fatalf("client IP = %q, want 198.51.100.20", got)
	}
}

func TestRequestClientIPHandlesLoopbackIPv6(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::1]:11434"
	req.Header.Set("CF-Connecting-IP", "2001:db8::8")

	if got := requestClientIP(req); got != "2001:db8::8" {
		t.Fatalf("client IP = %q, want 2001:db8::8", got)
	}
}
