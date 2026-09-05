package app

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// registerAdminRoutes wires the admin JSON API used by the WebUI. It must be
// mounted behind adminAuth.
func registerAdminRoutes(mux *http.ServeMux, cc *CCClient, pool *AccountPool, cfg *Config, usage *UsageTracker, ring *logRing) {
	mux.HandleFunc("GET /admin/api/overview", handleAdminOverview(cfg, pool, usage))
	mux.HandleFunc("GET /admin/api/accounts", handleAdminAccountsList(pool, usage))
	mux.HandleFunc("POST /admin/api/accounts", handleAdminAccountAdd(pool, cfg, usage))
	mux.HandleFunc("PATCH /admin/api/accounts/{id}", handleAdminAccountPatch(pool, cfg, usage))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", handleAdminAccountDelete(pool, cfg, usage))
	mux.HandleFunc("POST /admin/api/accounts/{id}/test", handleAdminAccountTest(pool, cc))
	mux.HandleFunc("GET /admin/api/settings", handleAdminSettingsGet(cfg))
	mux.HandleFunc("PUT /admin/api/settings", handleAdminSettingsPut(cfg, cc, pool))
	mux.HandleFunc("GET /admin/api/logs", handleAdminLogs(ring))
	mux.HandleFunc("POST /admin/api/oauth/start", handleAdminOAuthStart(pool, cfg))
	mux.HandleFunc("GET /admin/api/oauth/status", handleAdminOAuthStatus(pool))
	mux.HandleFunc("POST /admin/api/oauth/cancel", handleAdminOAuthCancel())
}

func writeAdminJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeAdminError(w http.ResponseWriter, status int, msg string) {
	writeAdminJSON(w, status, map[string]any{"error": msg})
}

func subtleConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// adminAccount merges the ephemeral health view with durable usage counters.
type adminAccount struct {
	AccountView
	AccountUsageSnapshot
}

func adminAccountViews(pool *AccountPool, usage *UsageTracker) []adminAccount {
	views := pool.Views()
	out := make([]adminAccount, 0, len(views))
	for _, v := range views {
		out = append(out, adminAccount{AccountView: v, AccountUsageSnapshot: usage.AccountUsage(v.ID)})
	}
	return out
}

func handleAdminOverview(cfg *Config, pool *AccountPool, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		total, enabled, rateLimited := pool.Stats()
		loaded := len(modelCatalog)
		available := 0
		for _, m := range modelCatalog {
			if !isModelExcluded(m.ID, cfg.Excludes()) {
				available++
			}
		}
		overview := adminOverview{
			Version:       Version,
			UptimeSeconds: int64(time.Since(serverStartedAt).Seconds()),
			Listen:        fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			WebUI:         cfg.WebUIEnabled(),
			Usage:         usage.Snapshot(),
		}
		overview.Accounts = adminAccountsSummary{Total: total, Enabled: enabled, RateLimited: rateLimited}
		overview.Models = adminModelsSummary{Loaded: loaded, Available: available}
		writeAdminJSON(w, 200, overview)
	}
}

type adminOverview struct {
	Version       string               `json:"version"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	Listen        string               `json:"listen"`
	WebUI         bool                 `json:"webui"`
	Usage         UsageSnapshot        `json:"usage"`
	Accounts      adminAccountsSummary `json:"accounts"`
	Models        adminModelsSummary   `json:"models"`
}

type adminAccountsSummary struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	RateLimited int `json:"rate_limited"`
}

type adminModelsSummary struct {
	Loaded    int `json:"loaded"`
	Available int `json:"available"`
}

func handleAdminAccountsList(pool *AccountPool, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeAdminJSON(w, 200, map[string]any{"accounts": adminAccountViews(pool, usage)})
	}
}

func handleAdminAccountAdd(pool *AccountPool, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string `json:"name"`
			APIKey string `json:"api_key"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			writeAdminError(w, 400, err.Error())
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" {
			body.Name = fmt.Sprintf("account-%d", pool.Len()+1)
		}
		acct, err := pool.Add(body.Name, body.APIKey, true)
		if err != nil {
			writeAdminError(w, http.StatusConflict, err.Error())
			return
		}
		if err := persistPool(pool, cfg); err != nil {
			writeAdminError(w, 500, "account added but saving config failed: "+err.Error())
			return
		}
		log.Printf("account %q added via webui", acct.Name)
		writeAdminJSON(w, 201, adminAccount{AccountView: acct.View(), AccountUsageSnapshot: usage.AccountUsage(acct.ID)})
	}
}

func handleAdminAccountPatch(pool *AccountPool, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Enabled *bool   `json:"enabled"`
			Name    *string `json:"name"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			writeAdminError(w, 400, err.Error())
			return
		}
		if body.Enabled != nil && !pool.SetEnabled(id, *body.Enabled) {
			writeAdminError(w, 404, "account not found")
			return
		}
		if body.Name != nil {
			if !pool.Rename(id, strings.TrimSpace(*body.Name)) {
				writeAdminError(w, 404, "account not found")
				return
			}
		}
		if err := persistPool(pool, cfg); err != nil {
			writeAdminError(w, 500, "saving config failed: "+err.Error())
			return
		}
		acct := pool.Get(id)
		writeAdminJSON(w, 200, adminAccount{AccountView: acct.View(), AccountUsageSnapshot: usage.AccountUsage(id)})
	}
}

func handleAdminAccountDelete(pool *AccountPool, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !pool.Remove(id) {
			writeAdminError(w, 404, "account not found")
			return
		}
		usage.DropAccount(id)
		if err := persistPool(pool, cfg); err != nil {
			writeAdminError(w, 500, "saving config failed: "+err.Error())
			return
		}
		writeAdminJSON(w, 200, map[string]any{"deleted": true})
	}
}

// handleAdminAccountTest probes the upstream with one account's key so the
// user can validate a credential without sending a chat request.
func handleAdminAccountTest(pool *AccountPool, cc *CCClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct := pool.Get(r.PathValue("id"))
		if acct == nil {
			writeAdminError(w, 404, "account not found")
			return
		}
		result := testAccountKey(cc.BaseURLValue(), acct.APIKey)
		writeAdminJSON(w, 200, result)
	}
}

type accountTestResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Models    int    `json:"models,omitempty"`
	Error     string `json:"error,omitempty"`
}

func testAccountKey(baseURL, apiKey string) accountTestResult {
	result := accountTestResult{}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", strings.TrimRight(baseURL, "/")+"/provider/v1/models", nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	start := time.Now()
	resp, err := client.Do(req)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		result.Error = strings.TrimSpace(string(body))
		return result
	}
	var list CCProviderModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		result.Error = "decode response: " + err.Error()
		return result
	}
	result.OK = true
	result.Models = len(list.Data)
	return result
}

func handleAdminSettingsGet(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeAdminJSON(w, 200, adminSettingsFrom(cfg))
	}
}

func adminSettingsFrom(cfg *Config) adminSettings {
	return adminSettings{
		BaseURL:          cfg.UpstreamBaseURL(),
		ExcludeModels:    cfg.Excludes(),
		Host:             cfg.Host,
		Port:             cfg.Port,
		WebUI:            cfg.WebUIEnabled(),
		AdminPasswordSet: cfg.adminPassword() != "",
	}
}

type adminSettings struct {
	BaseURL          string   `json:"base_url"`
	ExcludeModels    []string `json:"exclude_models"`
	Host             string   `json:"host"`
	Port             int      `json:"port"`
	WebUI            bool     `json:"webui"`
	AdminPasswordSet bool     `json:"admin_password_set"`
}

type adminSettingsUpdate struct {
	BaseURL       *string   `json:"base_url"`
	ExcludeModels *[]string `json:"exclude_models"`
	Host          *string   `json:"host"`
	Port          *int      `json:"port"`
	WebUI         *bool     `json:"webui"`
	AdminPassword *string   `json:"admin_password"`
}

// handleAdminSettingsPut applies settings. exclude_models, base_url, and the
// admin password take effect immediately; host, port, and webui need a
// restart, which the response reports back.
func handleAdminSettingsPut(cfg *Config, cc *CCClient, pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body adminSettingsUpdate
		if err := decodeJSONBody(w, r, &body); err != nil {
			writeAdminError(w, 400, err.Error())
			return
		}

		restartRequired := []string{}

		if body.ExcludeModels != nil {
			cfg.SetExcludes(normalizeExcludeModels(*body.ExcludeModels))
		}
		if body.BaseURL != nil {
			url := strings.TrimSpace(*body.BaseURL)
			if url == "" {
				writeAdminError(w, 400, "base_url cannot be empty")
				return
			}
			cfg.SetUpstreamBaseURL(url)
			cc.SetBaseURL(url)
		}
		if body.AdminPassword != nil {
			password := strings.TrimSpace(*body.AdminPassword)
			if len(password) < 8 {
				writeAdminError(w, 400, "admin_password must be at least 8 characters")
				return
			}
			cfg.setAdminPassword(password)
		}
		if body.Host != nil && strings.TrimSpace(*body.Host) != "" {
			cfg.Host = strings.TrimSpace(*body.Host)
			restartRequired = append(restartRequired, "host")
		}
		if body.Port != nil && *body.Port > 0 {
			cfg.Port = *body.Port
			restartRequired = append(restartRequired, "port")
		}
		if body.WebUI != nil {
			cfg.WebUI = body.WebUI
			restartRequired = append(restartRequired, "webui")
		}

		// Keep the persisted config consistent with the live pool.
		pool.SyncToConfig(cfg)
		if err := saveConfig(configFile, cfg); err != nil {
			writeAdminError(w, 500, "applied but saving config failed: "+err.Error())
			return
		}
		log.Printf("settings updated via webui (restart required: %v)", restartRequired)
		writeAdminJSON(w, 200, map[string]any{
			"settings":         adminSettingsFrom(cfg),
			"restart_required": restartRequired,
		})
	}
}

func normalizeExcludeModels(list []string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func handleAdminLogs(ring *logRing) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after := int64(0)
		if raw := r.URL.Query().Get("after"); raw != "" {
			fmt.Sscanf(raw, "%d", &after)
		}
		entries, last := ring.After(after)
		writeAdminJSON(w, 200, map[string]any{"entries": entries, "last_seq": last})
	}
}

// ====== WebUI OAuth flow management ======

var (
	webOAuthMu   sync.Mutex
	webOAuthFlow *OAuthFlow
)

func handleAdminOAuthStart(pool *AccountPool, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		webOAuthMu.Lock()
		if webOAuthFlow != nil && webOAuthFlow.StateName() == "pending" {
			authURL := webOAuthFlow.AuthURL
			webOAuthMu.Unlock()
			writeAdminJSON(w, 200, map[string]any{"state": "pending", "auth_url": authURL, "already_running": true})
			return
		}
		flow, err := StartOAuthFlow(OAuthOptions{})
		if err != nil {
			webOAuthMu.Unlock()
			writeAdminError(w, 500, err.Error())
			return
		}
		webOAuthFlow = flow
		webOAuthMu.Unlock()

		go func() {
			cb, err := flow.Wait(oauthTimeout)
			if err != nil {
				log.Printf("[WARN] webui oauth flow ended: %v", err)
				return
			}
			name := strings.TrimSpace(cb.KeyName)
			if name == "" {
				name = strings.TrimSpace(cb.UserName)
			}
			if name == "" {
				name = "oauth"
			}
			acct, err := pool.Add(name, cb.APIKey, true)
			if err != nil {
				log.Printf("[WARN] webui oauth: add account failed: %v", err)
				return
			}
			if err := persistPool(pool, cfg); err != nil {
				log.Printf("[WARN] webui oauth: save config failed: %v", err)
				return
			}
			log.Printf("✓ OAuth account %q added via webui (user %s)", acct.Name, cb.UserName)
		}()

		writeAdminJSON(w, 200, map[string]any{"state": "pending", "auth_url": flow.AuthURL})
	}
}

func handleAdminOAuthStatus(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		webOAuthMu.Lock()
		flow := webOAuthFlow
		webOAuthMu.Unlock()

		if flow == nil {
			writeAdminJSON(w, 200, map[string]any{"state": "idle"})
			return
		}
		resp := map[string]any{
			"state":    flow.StateName(),
			"auth_url": flow.AuthURL,
		}
		switch flow.StateName() {
		case "success":
			if cb, ok := flow.Result(); ok {
				if acct := pool.Get(accountID(cb.APIKey)); acct != nil {
					resp["account_id"] = acct.ID
					resp["account_name"] = acct.Name
				}
			}
		case "failed":
			if err := flow.Err(); err != nil {
				resp["error"] = err.Error()
			}
		}
		writeAdminJSON(w, 200, resp)
	}
}

func handleAdminOAuthCancel() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		webOAuthMu.Lock()
		flow := webOAuthFlow
		webOAuthMu.Unlock()

		if flow != nil && flow.StateName() == "pending" {
			flow.Cancel()
		}
		writeAdminJSON(w, 200, map[string]any{"canceled": true})
	}
}

// persistPool snapshots the pool into cfg and writes config.yaml.
func persistPool(pool *AccountPool, cfg *Config) error {
	pool.SyncToConfig(cfg)
	return saveConfig(configFile, cfg)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("request body is required")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
