package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type discordAlertsView struct {
	Enabled           bool    `json:"enabled"`
	WebhookURL        string  `json:"webhook_url"`
	WebhookConfigured bool    `json:"webhook_configured"`
	MentionEveryone   bool    `json:"mention_everyone"`
	HourlyCap         float64 `json:"hourly_cap"`
	WeeklyCap         float64 `json:"weekly_cap"`
	MonthlyCap        float64 `json:"monthly_cap"`
}

type discordAlertsUpdate struct {
	Enabled         bool    `json:"enabled"`
	WebhookURL      string  `json:"webhook_url"`
	ClearWebhook    bool    `json:"clear_webhook"`
	MentionEveryone bool    `json:"mention_everyone"`
	HourlyCap       float64 `json:"hourly_cap"`
	WeeklyCap       float64 `json:"weekly_cap"`
	MonthlyCap      float64 `json:"monthly_cap"`
}

func discordAlertsResponse(c DiscordAlertConfig, legacyWebhook string) discordAlertsView {
	if c.WebhookURL == "" {
		c.WebhookURL = legacyWebhook
	}
	if c.HourlyCap == 0 {
		c.HourlyCap = defaultDiscordHourlyCap
	}
	if c.WeeklyCap == 0 {
		c.WeeklyCap = defaultDiscordWeeklyCap
	}
	if c.MonthlyCap == 0 {
		c.MonthlyCap = defaultDiscordMonthlyCap
	}
	return discordAlertsView{
		Enabled: c.Enabled, WebhookURL: maskWebhookURL(c.WebhookURL), WebhookConfigured: c.WebhookURL != "",
		MentionEveryone: c.MentionEveryone, HourlyCap: c.HourlyCap, WeeklyCap: c.WeeklyCap, MonthlyCap: c.MonthlyCap,
	}
}

func maskWebhookURL(webhook string) string {
	if webhook == "" {
		return ""
	}
	return "********"
}

func handleAdminDiscordAlerts(cfg *Config, store *ConfigStore, billing *BillingTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			c, legacyWebhook := store.DiscordAlerts()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(discordAlertsResponse(c, legacyWebhook))
		case http.MethodPost:
			if !requireJSONContentType(w, r) {
				return
			}
			var body discordAlertsUpdate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
				return
			}
			body.WebhookURL = strings.TrimSpace(body.WebhookURL)
			current, legacyWebhook := store.DiscordAlerts()
			if current.WebhookURL == "" {
				current.WebhookURL = legacyWebhook
			}
			webhook := current.WebhookURL
			if body.ClearWebhook {
				webhook = ""
			} else if body.WebhookURL != "" {
				webhook = body.WebhookURL
			}
			if body.Enabled && webhook == "" {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "webhook_url is required when alerts are enabled")
				return
			}
			if body.HourlyCap <= 0 || body.WeeklyCap <= 0 || body.MonthlyCap <= 0 {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "caps must be greater than zero")
				return
			}
			c := DiscordAlertConfig{Enabled: body.Enabled, WebhookURL: webhook, HourlyCap: body.HourlyCap, WeeklyCap: body.WeeklyCap, MonthlyCap: body.MonthlyCap, MentionEveryone: body.MentionEveryone}
			if err := store.SetDiscordAlerts(c); err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "save alert settings: "+err.Error())
				return
			}
			if c.Enabled {
				billing.SetDiscordAlerter(NewDiscordAlerterWithConfig(c))
			} else {
				billing.SetDiscordAlerter(nil)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(discordAlertsResponse(c, ""))
		default:
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		}
	}
}

// handleAdminModels serves the web UI's Models tab: the full model catalog
// (including entries currently filtered out of /v1/models), each flagged
// with its enabled/excluded status per policy (see ModelPolicy.Enabled) and
// its family/variant grouping. Reachable only via the same
// loopback+Host+Origin gate as /accounts — see isAdminPath and
// isLocalAdminRequest in ui.go.
func handleAdminModels(policy *ModelPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AdminModelList{Object: "list", Data: adminModels(policy)})
	}
}

// modelToggleRequest is POST /admin/models/toggle's body: exactly one of
// Models (a per-model override) or Family (expanded to current child models) must
// be populated, alongside Enabled.
type modelToggleRequest struct {
	Models  []string `json:"models"`
	Family  string   `json:"family"`
	Enabled *bool    `json:"enabled"`
}

// handleAdminModelsToggle applies either an explicit per-model override
// ({"models":["id1","id2"],"enabled":true}) or a family request expanded
// to current catalog children — exactly one of
// "models"/"family" may be populated — persists it through store via policy,
// and responds with the fresh full model list (same shape as
// GET /admin/models) so the web UI updates in one round trip. Reachable only
// via the same loopback+Host+Origin gate as /accounts.
func handleAdminModelsToggle(store *ConfigStore, policy *ModelPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if !requireJSONContentType(w, r) {
			return
		}

		var body modelToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
			return
		}
		if body.Enabled == nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "enabled is required")
			return
		}

		family := strings.TrimSpace(body.Family)
		models := make([]string, 0, len(body.Models))
		for _, id := range body.Models {
			id = strings.TrimSpace(id)
			if id != "" {
				models = append(models, id)
			}
		}

		hasModels := len(models) > 0
		hasFamily := family != ""
		if hasModels == hasFamily {
			writeError(w, http.StatusBadRequest, "invalid_request_error", `exactly one of "models" or "family" is required`)
			return
		}

		if hasModels {
			if catalog := knownModelIDs(); len(catalog) > 0 {
				for _, id := range models {
					if !catalog[id] {
						writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("unknown model %q", id))
						return
					}
				}
			}
			if err := policy.SetModelOverrides(store, models, *body.Enabled); err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "save model override: "+err.Error())
				return
			}
		} else {
			if err := policy.SetFamilyOverride(store, family, *body.Enabled); err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "save model overrides: "+err.Error())
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AdminModelList{Object: "list", Data: adminModels(policy)})
	}
}

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

// handleAdminBilling serves the web UI's per-account billing panel (inside
// the Accounts tab): each account's most recently fetched subscription,
// credits, and session identity. Reachable only via the same
// loopback+Host+Origin gate as /accounts.
func handleAdminBilling(tracker *BillingTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tracker.Report())
	}
}

// handleAdminConnection serves the web UI's Setup tab: the base URL and
// bearer API key a client should use to reach this gateway, plus the LAN
// and/or Tailscale base URLs when allow_lan/allow_tailscale detected one at
// startup (see ConnectionInfo). It never includes cfg.Accounts (the
// per-account Command Code credentials) — those are internal to the
// gateway, not something a connecting client needs. Reachable only via the
// same loopback+Host+Origin gate as /accounts.
//
// The displayed host is taken from the incoming request's Host header
// rather than cfg.Host: once cfg.Host is a wildcard bind like 0.0.0.0
// (allow_lan/allow_tailscale), echoing it back would hand the caller a
// base_url no client can actually dial. r.Host is, by construction,
// whatever address the caller just used to reach this handler.
func handleAdminConnection(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		info := ConnectionInfo{
			BaseURL: fmt.Sprintf("http://%s:%d/v1", host, cfg.Port),
			Host:    host,
			Port:    cfg.Port,
			APIKey:  cfg.APIKey,
		}
		// Surface the LAN/Tailscale addresses too, whenever both are
		// available — a caller viewing the Setup tab over loopback still
		// needs these to connect a client from another device, and if
		// allow_lan and allow_tailscale are both on, both should show up
		// side by side rather than only whichever host happened to load
		// this page.
		if cfg.AllowLAN && cfg.DetectedLANIP != "" {
			info.LANBaseURL = fmt.Sprintf("http://%s:%d/v1", cfg.DetectedLANIP, cfg.Port)
		}
		if cfg.AllowTailscale && cfg.DetectedTailscaleIP != "" {
			info.TailscaleBaseURL = fmt.Sprintf("http://%s:%d/v1", cfg.DetectedTailscaleIP, cfg.Port)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(info)
	}
}
