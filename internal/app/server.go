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
)

func authMiddleware(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /health, /usage, /favicon.ico, and CORS preflight do not require
			// authentication
			if r.Method == http.MethodOptions || r.URL.Path == "/health" || r.URL.Path == "/usage" || r.URL.Path == "/favicon.ico" {
				next.ServeHTTP(w, r)
				return
			}
			// The admin surface (/ui, /accounts*, /admin/*) can also be reached
			// without a bearer token from a genuine loopback browser request —
			// see isLocalAdminRequest in ui.go for the full loopback+Host+Origin
			// gate that keeps this from being usable by a remote page via DNS
			// rebinding. cfg.UINoAuth (the --ui-no-auth flag / ui_no_auth config
			// key) opts out of that gate entirely so the UI works from a remote
			// browser on a trusted network. Neither path affects the LLM proxy
			// routes below — those always require the bearer APIKey.
			if isAdminPath(r.URL.Path) && (cfg.UINoAuth || isLocalAdminRequest(r)) {
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
		// The admin surface (/ui, /accounts*) must never get a wildcard CORS
		// header: that would let any website the user has open in a browser
		// tab read the account list or trigger a delete via fetch(). Instead
		// it only ever gets a same-origin-implying Vary header — the actual
		// access decision is made by isLocalAdminRequest in authMiddleware.
		if isAdminPath(r.URL.Path) {
			w.Header().Set("Vary", "Origin")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
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

// newHandler is a backward-compatible shim over newHandlerWithPolicy for
// callers (mainly tests) that don't care about model overrides: it builds a
// ModelPolicy from cfg once, exactly mirroring the exclude_models-only
// behavior this function had before ModelPolicy existed.
func newHandler(pool *AccountPool, cfg *Config, usage *UsageTracker, reauthMgr *ReauthManager, store *ConfigStore, billing *BillingTracker) http.Handler {
	return newHandlerWithPolicy(pool, cfg, usage, reauthMgr, store, billing, NewModelPolicy(cfg))
}

// newHandlerWithPolicy assembles the full mux + middleware chain. Split out
// from runServer so it can be exercised directly with httptest, without
// opening a real listener.
func newHandlerWithPolicy(pool *AccountPool, cfg *Config, usage *UsageTracker, reauthMgr *ReauthManager, store *ConfigStore, billing *BillingTracker, policy *ModelPolicy) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/chat/completions", handleChatCompletionsWithPolicy(pool, cfg, usage, policy))
	// /v1/responses inherits bearer auth + CORS from the global middleware
	// automatically — do not add admin-style gating here.
	mux.HandleFunc("/v1/responses", handleResponsesWithPolicy(pool, cfg, usage, policy))
	mux.HandleFunc("/v1/models", handleModelsWithPolicy(policy))
	mux.HandleFunc("/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usage.Snapshot())
	})
	// /accounts exposes account metadata (never raw API keys), so unlike
	// /health and /usage it stays behind the bearer token.
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pool.Snapshot())
	})
	// /accounts/reauth lets an account be reauthorized over HTTP instead of
	// stopping the process to run --oauth --account <name>; it is the
	// foundation for the web UI's per-account "Reauthorize" (and "Add
	// account") button.
	mux.HandleFunc("/accounts/reauth", handleAccountsReauth(reauthMgr))
	// /accounts/delete removes an account from config, the live pool, and
	// any tracked reauth session — the web UI's "Delete" button.
	mux.HandleFunc("/accounts/delete", handleAccountsDelete(store, pool, reauthMgr))
	// /accounts/probe drives an immediate synchronous health check of every
	// account — the web UI's manual "Refresh status" button.
	mux.HandleFunc("/accounts/probe", handleAccountsProbe(pool))
	// /admin/models serves the web UI's Models tab (the full catalog, each
	// flagged with its current enabled/excluded status and family grouping).
	mux.HandleFunc("/admin/models", handleAdminModels(policy))
	// /admin/models/toggle applies a per-model or per-family enabled/disabled
	// override — the web UI's Models tab toggle switches.
	mux.HandleFunc("/admin/models/toggle", handleAdminModelsToggle(store, policy))
	// /admin/connection serves the web UI's Setup tab (base URL + API key
	// for a connecting client; never per-account Command Code credentials).
	mux.HandleFunc("/admin/connection", handleAdminConnection(cfg))
	// /admin/usage serves the web UI's Usage tab (global totals plus a
	// per-account breakdown). It is intentionally separate from the
	// unauthenticated GET /usage endpoint above because it exposes account
	// names; as an /admin/ prefixed path it inherits the same
	// loopback+Host+Origin gate as /accounts and /admin/models via
	// isAdminPath in ui.go.
	mux.HandleFunc("/admin/usage", handleAdminUsage(usage))
	// /admin/billing serves the web UI's per-account billing panel (the
	// most recently fetched subscription/credits/session snapshot).
	mux.HandleFunc("/admin/billing", handleAdminBilling(billing))
	mux.HandleFunc("/admin/alerts", handleAdminDiscordAlerts(cfg, store, billing))
	// /accounts/billing-token sets (or updates) an account's billing session
	// token and immediately triggers a fetch for it, so the web UI gets
	// fresh billing data without waiting for the next background refresh.
	mux.HandleFunc("/accounts/billing-token", handleAccountsBillingToken(store, billing))
	// /ui and /ui/* serve the embedded web UI (see ui.go).
	mux.Handle("/ui", uiHandler())
	mux.Handle("/ui/", uiHandler())
	// /favicon.ico serves the same embedded icon at root, unauthenticated
	// (see faviconHandler in ui.go).
	mux.Handle("/favicon.ico", faviconHandler())

	var handler http.Handler = mux
	handler = authMiddleware(cfg)(handler)
	handler = loggingMiddleware(handler)
	handler = corsMiddleware(handler)
	return handler
}

// requireJSONContentType adds basic CSRF friction to mutating admin
// endpoints: a browser <form> cannot set an arbitrary Content-Type without
// triggering a CORS preflight, and that preflight would fail the
// loopback+Host+Origin check in authMiddleware before this handler ever
// runs. It writes the error response itself and reports whether the caller
// should continue.
func requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "Content-Type must be application/json")
		return false
	}
	return true
}

// decodeAccountName decodes a JSON body shaped {"name": "..."} from r,
// trims it, and rejects an empty result. It writes the error response
// itself (matching writeError's shape) and returns ok=false if decoding or
// validation fails, so callers can just check ok and return. Shared by
// handleAccountsDelete and handleAccountsReauth's POST branch so both admin
// endpoints that take a bare account name in the body agree on its shape.
func decodeAccountName(w http.ResponseWriter, r *http.Request) (name string, ok bool) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
		return "", false
	}
	name = strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "name is required")
		return "", false
	}
	return name, true
}

// handleAccountsDelete removes name from config (via store), the live pool,
// and any in-flight/resolved reauth session for it.
func handleAccountsDelete(store *ConfigStore, pool *AccountPool, reauthMgr *ReauthManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if !requireJSONContentType(w, r) {
			return
		}
		name, ok := decodeAccountName(w, r)
		if !ok {
			return
		}

		existed, err := store.Remove(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "remove account: "+err.Error())
			return
		}
		poolExisted := pool.RemoveAccount(name)
		reauthMgr.Forget(name)

		if !existed && !poolExisted {
			writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("account %q not found", name))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"deleted": name})
	}
}

// decodeAccountBillingToken decodes a JSON body shaped
// {"name": "...", "session_token": "..."} from r, trims both fields, and
// rejects an empty result for either. It writes the error response itself
// and returns ok=false if decoding or validation fails, so callers can just
// check ok and return.
func decodeAccountBillingToken(w http.ResponseWriter, r *http.Request) (name, token string, ok bool) {
	var body struct {
		Name         string `json:"name"`
		SessionToken string `json:"session_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
		return "", "", false
	}
	name = strings.TrimSpace(body.Name)
	token = strings.TrimSpace(body.SessionToken)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "name is required")
		return "", "", false
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "session_token is required")
		return "", "", false
	}
	return name, token, true
}

// handleAccountsBillingToken sets an account's billing session token and
// immediately fetches fresh billing/session data for it, returning that
// account's AccountBilling so the web UI gets instant feedback instead of
// waiting for the next background refresh.
func handleAccountsBillingToken(store *ConfigStore, billing *BillingTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if !requireJSONContentType(w, r) {
			return
		}
		name, token, ok := decodeAccountBillingToken(w, r)
		if !ok {
			return
		}

		existed, err := store.SetSessionToken(name, token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "save session token: "+err.Error())
			return
		}
		if !existed {
			writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("account %q not found", name))
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		billing.refreshOne(ctx, store, Account{Name: name, SessionToken: token})

		w.Header().Set("Content-Type", "application/json")
		for _, row := range billing.Report() {
			if row.Account == name {
				json.NewEncoder(w).Encode(row)
				return
			}
		}
		json.NewEncoder(w).Encode(AccountBilling{Account: name})
	}
}

// handleAccountsProbe synchronously re-checks every account's health, then
// returns the fresh snapshot — the web UI's manual "Refresh status" button.
func handleAccountsProbe(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if !requireJSONContentType(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		pool.ProbeAll(ctx)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pool.Snapshot())
	}
}

// handleAccountsReauth serves both halves of the reauth flow on one path:
// POST starts (or returns the already-pending) session, GET polls it.
func handleAccountsReauth(reauthMgr *ReauthManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if !requireJSONContentType(w, r) {
				return
			}
			name, ok := decodeAccountName(w, r)
			if !ok {
				return
			}
			session, err := reauthMgr.Start(name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "start reauth: "+err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(session.Snapshot())
		case http.MethodGet:
			name := strings.TrimSpace(r.URL.Query().Get("name"))
			if name == "" {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "name is required")
				return
			}
			session, ok := reauthMgr.Get(name)
			if !ok {
				writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("no reauth session for account %q", name))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(session.Snapshot())
		default:
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		}
	}
}

func runServer(pool *AccountPool, cfg *Config, usage *UsageTracker, reauthMgr *ReauthManager, store *ConfigStore, billing *BillingTracker) error {
	policy := NewModelPolicy(cfg)
	handler := newHandlerWithPolicy(pool, cfg, usage, reauthMgr, store, billing, policy)

	if cfg.AllowLAN {
		if ip, ok := detectLANIPv4(); ok {
			cfg.DetectedLANIP = ip.String()
		}
	}
	if cfg.AllowTailscale {
		if ip, ok := detectTailscaleIPv4(); ok {
			cfg.DetectedTailscaleIP = ip.String()
		}
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 600 * time.Second, // streaming responses need a long timeout
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
			log.Printf("shutdown: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Printf("cmdcode2api starting, listening on %s", addr)
	log.Printf("reachable at http://localhost:%d (loopback, no API key needed for /ui)", cfg.Port)
	if cfg.UINoAuth {
		log.Printf("WARNING: ui_no_auth is set — /ui and the account-management API are served WITHOUT authentication")
	}
	if cfg.AllowLAN {
		if cfg.DetectedLANIP != "" {
			log.Printf("reachable at http://%s:%d (LAN — /ui requires the API key)", cfg.DetectedLANIP, cfg.Port)
		} else {
			log.Printf("allow_lan is enabled but no LAN IP address was detected")
		}
	}
	if cfg.AllowTailscale {
		if cfg.DetectedTailscaleIP != "" {
			log.Printf("reachable at http://%s:%d (Tailscale — /ui requires the API key)", cfg.DetectedTailscaleIP, cfg.Port)
		} else {
			log.Printf("allow_tailscale is enabled but no Tailscale IP was detected")
		}
	}
	loadedModels := len(availableModels())
	enabledModels := 0
	for _, model := range modelCatalog {
		if policy.Enabled(model.ID) {
			enabledModels++
		}
	}
	log.Printf("models: %d loaded, %d available", loadedModels, enabledModels)
	if enabledModels == 0 && loadedModels > 0 {
		log.Printf("models: none are enabled — enable the ones your plan serves at http://localhost:%d/ui#models", cfg.Port)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleConnsClosed
	return nil
}
