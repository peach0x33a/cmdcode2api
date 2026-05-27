package app

import (
	"bytes"
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
