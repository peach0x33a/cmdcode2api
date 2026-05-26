package main

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
		// skip health check logging
		if r.URL.Path != "/health" {
			log.Printf("[%s] %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" {
			log.Printf("[%s] %s — %v", r.Method, r.URL.Path, time.Since(start))
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
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usage.Snapshot())
	})

	var handler http.Handler = mux
	handler = corsMiddleware(handler)
	handler = authMiddleware(cfg)(handler)
	handler = loggingMiddleware(handler)

	addr := fmt.Sprintf(":%d", cfg.Port)
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

	log.Printf("cmdcode2api starting on http://localhost%s", addr)
	log.Printf("models: %d available", len(availableModels()))
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	<-idleConnsClosed
	return nil
}

func availableModels() []string {
	// 与 handler.go 里硬编码的列表同步
	return []string{
		"claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.4-mini",
		"gemini-3.5-flash", "gemini-3.1-flash-lite",
		"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash",
		"moonshotai/Kimi-K2.6", "moonshotai/Kimi-K2.5",
		"zai-org/GLM-5.1", "zai-org/GLM-5",
		"MiniMaxAI/MiniMax-M2.7", "MiniMaxAI/MiniMax-M2.5",
		"Qwen/Qwen3.6-Max-Preview", "Qwen/Qwen3.6-Plus", "Qwen/Qwen3.7-Max",
		"step-3.5-flash",
	}
}
