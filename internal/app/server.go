package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cmdcode2api/internal/web"
)

var serverStartedAt = time.Now()

// ctxKeyClientKeyID identifies the authenticated client key in request
// contexts.
type ctxKey int

const ctxKeyClientKeyID ctxKey = iota

// clientKeyID returns the authenticated client key ID stored by
// authMiddleware, or "".
func clientKeyIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyClientKeyID).(string)
	return id
}

func authMiddleware(cfg *Config, keys *ClientKeyPool) func(http.Handler) http.Handler {
	if keys == nil {
		// Legacy single-key setup (also keeps direct middleware use in tests
		// working without a pool).
		if cfg.APIKey != "" {
			keys = NewClientKeyPool([]ClientKeyConfig{{Name: "default", Key: cfg.APIKey}})
		} else {
			keys = NewClientKeyPool(nil)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /health、/usage、/webui 页面和 CORS preflight 不需要 Bearer 认证；
			// /admin/* 有独立的管理密码认证。
			if r.Method == http.MethodOptions || isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				writeError(w, 401, "authentication_error", "missing Authorization header")
				return
			}
			key := strings.TrimPrefix(auth, "Bearer ")
			ck := keys.Lookup(key)
			if ck == nil || !ck.Enabled {
				writeError(w, 401, "authentication_error", "invalid API key")
				return
			}
			ck.RecordUsed()
			ctx := context.WithValue(r.Context(), ctxKeyClientKeyID, ck.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isPublicPath(path string) bool {
	switch path {
	case "/health", "/usage", "/webui", "/webui/":
		return true
	}
	switch {
	case strings.HasPrefix(path, "/webui/"):
		return true
	case strings.HasPrefix(path, "/admin/"):
		return true
	}
	return false
}

// adminAuth guards the admin API with the separate admin_password. Failed
// attempts are rate limited per source IP; the limiter instance lives as
// long as the middleware does (pass nil to get a fresh one).
func adminAuth(cfg *Config, limiter *ipRateLimiter) func(http.Handler) http.Handler {
	if limiter == nil {
		limiter = newIPRateLimiter()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r.RemoteAddr)
			if ok, retryAfter := limiter.Allow(ip); !ok {
				seconds := int(retryAfter.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeAdminError(w, http.StatusTooManyRequests,
					fmt.Sprintf("失败次数过多，请 %d 秒后重试", seconds))
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				limiter.Fail(ip)
				writeAdminError(w, 401, "missing Authorization header")
				return
			}
			key := strings.TrimPrefix(auth, "Bearer ")
			if !subtleConstantTimeEqual(key, cfg.adminPassword()) {
				limiter.Fail(ip)
				writeAdminError(w, 401, "invalid admin password")
				return
			}
			limiter.Reset(ip)
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders adds hardening headers to every response. The CSP allows
// the embedded console's inline script/styles while blocking framing,
// sniffing, referrer leakage, and external content sources.
func securityHeaders() func(http.Handler) http.Handler {
	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'unsafe-inline'; " +
		"connect-src 'self'; " +
		"img-src 'self' data:; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			next.ServeHTTP(w, r)
		})
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder, wrapped := newStatusRecorder(w)
		next.ServeHTTP(wrapped, r)
		if r.URL.Path != "/health" {
			log.Print(formatHTTPLog(r.Method, r.URL.Path, recorder.status, time.Since(start), r.RemoteAddr))
		}
	})
}

func runServer(cc *CCClient, cfg *Config, usage *UsageTracker, ring *logRing) error {
	serverStartedAt = time.Now()

	pool := cc.Pool
	if pool == nil {
		pool = NewAccountPool(nil)
	}
	keys := NewClientKeyPool(cfg.APIKeys)

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(cc, cfg, usage))
	mux.HandleFunc("/v1/models", handleModels(cfg))
	mux.HandleFunc("/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usage.Snapshot())
	})

	// WebUI：管理 API 与内嵌的单文件界面，挂在 /webui 下，根路径留给 API。
	adminMux := http.NewServeMux()
	registerAdminRoutes(adminMux, cc, pool, keys, cfg, usage, ring)
	if cfg.WebUIEnabled() {
		mux.Handle("/admin/", adminAuth(cfg, nil)(adminMux))
		// Command Code 页面回传凭据的公开端点（靠 state 校验，非管理密码）。
		// 注册为更具体的 pattern，绕过 adminAuth。
		mux.HandleFunc("POST /admin/api/oauth/callback", handleWebOAuthCallback())
		mux.HandleFunc("/webui", web.Handler())
		mux.HandleFunc("/webui/", web.Handler())
	}

	var handler http.Handler = mux
	handler = authMiddleware(cfg, keys)(handler)
	handler = securityHeaders()(handler)
	handler = loggingMiddleware(handler)
	handler = corsMiddleware(handler)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 600 * time.Second, // 流式响应需要长超时
		IdleTimeout:  120 * time.Second,
	}

	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	idleConnsClosed := make(chan struct{})
	go func() {
		<-shutdownSignal.Done()
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown failed: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Printf("cmdcode2api listening on http://%s", addr)
	log.Printf("client keys: %d configured, %d enabled", keys.Len(), keys.EnabledCount())
	loadedModels := len(availableModels())
	availableCount := 0
	for _, model := range modelCatalog {
		if !isModelExcluded(model.ID, cfg.Excludes()) {
			availableCount++
		}
	}
	log.Printf("models: %d loaded, %d available", loadedModels, availableCount)
	if cfg.WebUIEnabled() {
		log.Printf("webui available at http://%s/webui", addr)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleConnsClosed
	return nil
}
