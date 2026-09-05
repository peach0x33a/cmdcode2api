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
	"strings"
	"syscall"
	"time"

	"cmdcode2api/internal/web"
)

var serverStartedAt = time.Now()

func authMiddleware(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /health、/usage、WebUI 页面和 CORS preflight 不需要 Bearer 认证；
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
			if key != cfg.APIKey {
				writeError(w, 401, "authentication_error", "invalid API key")
				return
			}
			next.ServeHTTP(w, r)
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

// adminAuth guards the admin API with the separate admin_password.
func adminAuth(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				writeAdminError(w, 401, "missing Authorization header")
				return
			}
			key := strings.TrimPrefix(auth, "Bearer ")
			if subtleConstantTimeEqual(key, cfg.adminPassword()) {
				next.ServeHTTP(w, r)
				return
			}
			writeAdminError(w, 401, "invalid admin password")
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
	registerAdminRoutes(adminMux, cc, pool, cfg, usage, ring)
	if cfg.WebUIEnabled() {
		mux.Handle("/admin/", adminAuth(cfg)(adminMux))
		mux.HandleFunc("/webui", web.Handler())
		mux.HandleFunc("/webui/", web.Handler())
	}

	var handler http.Handler = mux
	handler = authMiddleware(cfg)(handler)
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
