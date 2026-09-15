package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// adminRequestIn mirrors adminRequest but sets Accept-Language, which is what
// the WebUI sends to have the admin API answer in the selected UI language.
func adminRequestIn(t *testing.T, srv *httptest.Server, method, path, password, lang string, body any) (*http.Response, map[string]any) {
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
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
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

func errorOf(t *testing.T, payload map[string]any) string {
	t.Helper()
	msg, _ := payload["error"].(string)
	if msg == "" {
		t.Fatalf("payload has no error message: %#v", payload)
	}
	return msg
}

// TestAdminErrorsFollowAcceptLanguage covers the three ways a message reaches
// the client: messages composed in the handler, messages produced by a deeper
// layer, and errors that wrap an underlying cause.
func TestAdminErrorsFollowAcceptLanguage(t *testing.T) {
	srv, _, _, _, _, _ := newAdminTestEnv(t)

	t.Run("handler message, chinese", func(t *testing.T) {
		resp, payload := adminRequestIn(t, srv, "PATCH", "/admin/api/accounts/missing", "admin-pass-123", "zh-CN,zh;q=0.9,en;q=0.8", map[string]any{"enabled": false})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := errorOf(t, payload); got != "账号不存在" {
			t.Errorf("error = %q, want %q", got, "账号不存在")
		}
	})

	t.Run("handler message, english default", func(t *testing.T) {
		resp, payload := adminRequestIn(t, srv, "PATCH", "/admin/api/accounts/missing", "admin-pass-123", "de-DE,de;q=0.9", map[string]any{"enabled": false})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := errorOf(t, payload); got != "account not found" {
			t.Errorf("error = %q, want %q", got, "account not found")
		}
	})

	t.Run("domain layer message", func(t *testing.T) {
		resp, payload := adminRequestIn(t, srv, "POST", "/admin/api/accounts", "admin-pass-123", "zh-CN", map[string]any{"name": "empty-key"})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if got := errorOf(t, payload); got != "api_key 不能为空" {
			t.Errorf("error = %q, want %q", got, "api_key 不能为空")
		}
	})

	t.Run("wrapped cause keeps its detail", func(t *testing.T) {
		// Point persistence into a directory that does not exist, so saving
		// fails and the handler wraps the filesystem error.
		configFile = filepath.Join(t.TempDir(), "missing-dir", "config.yaml")
		resp, payload := adminRequestIn(t, srv, "PUT", "/admin/api/settings", "admin-pass-123", "zh-CN", map[string]any{"base_url": "https://api.example.test"})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		got := errorOf(t, payload)
		if !strings.HasPrefix(got, "已应用，但保存配置失败：") {
			t.Errorf("error = %q, want the localized wrapper prefix", got)
		}
		if !strings.Contains(got, "missing-dir") {
			t.Errorf("error = %q, want the underlying cause kept verbatim", got)
		}
	})
}

// TestAdminAuthErrorsFollowAcceptLanguage localizes the middleware, which
// answers before any handler runs, including the lockout message.
func TestAdminAuthErrorsFollowAcceptLanguage(t *testing.T) {
	srv, _, _, _, _, _ := newAdminTestEnv(t)

	resp, payload := adminRequestIn(t, srv, "GET", "/admin/api/overview", "wrong", "zh-CN", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := errorOf(t, payload); got != "管理密码不正确" {
		t.Errorf("error = %q, want %q", got, "管理密码不正确")
	}

	// Repeated failures arm the lockout; the lockout message is localized too.
	locked := false
	for i := 0; i < adminFailLimit+1 && !locked; i++ {
		attempt, _ := adminRequestIn(t, srv, "GET", "/admin/api/overview", "wrong", "zh-CN", nil)
		locked = attempt.StatusCode == http.StatusTooManyRequests
	}
	if !locked {
		t.Fatal("admin auth never locked out after repeated failures")
	}
	resp, payload = adminRequestIn(t, srv, "GET", "/admin/api/overview", "admin-pass-123", "zh-CN", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	got := errorOf(t, payload)
	if !strings.HasPrefix(got, "失败次数过多，请 ") || !strings.HasSuffix(got, " 秒后重试") {
		t.Errorf("error = %q, want the localized lockout message", got)
	}
}
