package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPRateLimiterLockoutWindow(t *testing.T) {
	l := newIPRateLimiter()
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("fresh IP must be allowed")
	}
	for i := 0; i < adminFailLimit; i++ {
		l.Fail("1.2.3.4")
	}
	if ok, retry := l.Allow("1.2.3.4"); ok || retry <= 0 {
		t.Fatalf("locked IP: ok=%v retry=%v", ok, retry)
	}
	// Other IPs are unaffected.
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("unrelated IP locked")
	}
	// Success clears the record.
	l.Reset("1.2.3.4")
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("Reset did not clear")
	}
}

func TestAdminAuthRateLimitsBruteForce(t *testing.T) {
	cfg := &Config{APIKey: "client-key"}
	cfg.setAdminPassword("correct-horse")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/overview", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	handler := adminAuth(cfg, nil)(mux)

	attempt := func(password string) int {
		req := httptest.NewRequest("GET", "/admin/api/overview", nil)
		if password != "" {
			req.Header.Set("Authorization", "Bearer "+password)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 0; i < adminFailLimit; i++ {
		if code := attempt("wrong-" + string(rune('a'+i))); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, code)
		}
	}
	// Locked out: even the correct password is refused, with Retry-After set.
	if code := attempt("correct-horse"); code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", code)
	}
	// A new limiter (fresh server) accepts the correct password again.
	handler2 := adminAuth(cfg, nil)(mux)
	req := httptest.NewRequest("GET", "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer correct-horse")
	rec := httptest.NewRecorder()
	handler2.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("fresh limiter status = %d, want 200", rec.Code)
	}
}
