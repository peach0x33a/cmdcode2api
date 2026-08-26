package app

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleAdminModels serves the web UI's Models tab: the full model catalog
// (including entries exclude_models currently filters out of /v1/models),
// each flagged with its excluded status. Reachable only via the same
// loopback+Host+Origin gate as /accounts — see isAdminPath and
// isLocalAdminRequest in ui.go.
func handleAdminModels(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AdminModelList{Object: "list", Data: adminModels(cfg)})
	}
}

// handleAdminConnection serves the web UI's Setup tab: the base URL and
// bearer API key a client should use to reach this gateway. It never
// includes cfg.Accounts (the per-account Command Code credentials) — those
// are internal to the gateway, not something a connecting client needs.
// Reachable only via the same loopback+Host+Origin gate as /accounts.
// handleAdminUsage serves the web UI's Usage tab: the global usage totals
// plus a per-account breakdown. This is distinct from the unauthenticated
// GET /usage endpoint (see server.go) because it exposes account names;
// reachable only via the same loopback+Host+Origin gate as /accounts.
func handleAdminUsage(usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usage.Report())
	}
}

func handleAdminConnection(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(ConnectionInfo{
			BaseURL: fmt.Sprintf("http://%s:%d/v1", cfg.Host, cfg.Port),
			Host:    cfg.Host,
			Port:    cfg.Port,
			APIKey:  cfg.APIKey,
		})
	}
}
