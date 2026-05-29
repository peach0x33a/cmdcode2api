package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

func authMiddleware(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /health、/usage 和 CORS preflight 不需要认证
			if r.Method == http.MethodOptions || r.URL.Path == "/health" || r.URL.Path == "/usage" {
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

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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

func runServer(cc *CCClient, cfg *Config, usage *UsageTracker) error {
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

	// 优雅关闭
	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(idleConnsClosed)
	}()

	log.Printf("cmdcode2api starting on http://%s", addr)
	loadedModels := len(availableModels())
	availableModels := 0
	for _, model := range modelCatalog {
		if !isModelExcluded(model.ID, cfg.ExcludeModels) {
			availableModels++
		}
	}
	log.Printf("models: %d loaded, %d available", loadedModels, availableModels)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	<-idleConnsClosed
	return nil
}
