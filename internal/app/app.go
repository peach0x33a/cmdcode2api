package app

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

const configFile = "config.yaml"

// healthCheckInterval is how often the background probe re-checks every
// configured account once the server is running.
const healthCheckInterval = 10 * time.Minute

// billingFetchInterval is how often each account's billing/session data is
// refetched once the server is running.
const billingFetchInterval = 10 * time.Minute

func Run() {
	oauthMode := flag.Bool("oauth", false, "authorize via browser OAuth to obtain a Command Code API Key")
	oauthCallbackFlag := flag.String("oauth-callback", "", "OAuth callback URL, e.g. http://server.example.com:5959/callback")
	account := flag.String("account", "", "Account name to authorize (required with --oauth)")
	listAccounts := flag.Bool("list-accounts", false, "List configured accounts and their live status")
	removeAccount := flag.String("remove-account", "", "Remove a configured account by name")
	host := flag.String("host", "", "HTTP listen host, e.g. localhost or 0.0.0.0")
	port := flag.Int("port", 0, "HTTP listen port")
	debug := flag.Bool("debug", false, "print request body and all CC SSE events to stderr")
	allowLAN := flag.Bool("allow-lan", false, "allow devices on the local network to reach this gateway (requires the API key; see README)")
	allowTailscale := flag.Bool("allow-tailscale", false, "allow devices reachable via Tailscale to reach this gateway (requires the API key; see README)")
	flag.Parse()

	cfgPath := findConfig()

	// --oauth mode: sign in via browser to obtain an API Key and write it under the given --account name
	if *oauthMode {
		if strings.TrimSpace(*account) == "" {
			log.Fatalf("--account <name> is required when using --oauth")
		}

		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		if cfg == nil {
			// No config yet — generate one first
			cfg2, err := defaultConfig()
			if err != nil {
				log.Fatalf("create config: %v", err)
			}
			if err := writeConfigTemplate(cfgPath, &cfg2); err != nil {
				log.Fatalf("create config: %v", err)
			}
			cfg = &cfg2
		}

		cb, err := runOAuthWithCallback(OAuthOptions{CallbackURL: *oauthCallbackFlag})
		if err != nil {
			log.Fatalf("OAuth failed: %v", err)
		}

		upsertAccount(cfg, buildAccountFromCallback(*account, cb, defaultCommandCodeBaseURL))
		if err := saveConfig(cfgPath, cfg); err != nil {
			log.Fatalf("failed to save config: %v", err)
		}

		fmt.Printf("\n✅ API Key written to %s (account: %s)\n", cfgPath, *account)
		fmt.Println("   You can now run cmdcode2api directly to start the service.")
		return
	}

	// --list-accounts: print each account and its live status from a one-time probe, without starting the service
	if *listAccounts {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		if cfg == nil || len(cfg.Accounts) == 0 {
			fmt.Println("No accounts configured. Run ./cmdcode2api --oauth --account <name> to add one.")
			return
		}
		applyDefaultBaseURLs(cfg.Accounts)
		pool := NewAccountPool(cfg.Accounts)
		pool.ProbeAll(context.Background())
		for _, acct := range pool.Snapshot() {
			line := fmt.Sprintf("%-20s %s", acct.Name, acct.Status)
			if acct.LastError != "" {
				line += fmt.Sprintf(" (%s)", acct.LastError)
			}
			fmt.Println(line)
		}
		return
	}

	// --remove-account: delete the specified account from the config
	if *removeAccount != "" {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		if cfg == nil {
			log.Fatalf("no config found; nothing to remove")
		}
		store := NewConfigStore(cfgPath, cfg)
		existed, err := store.Remove(*removeAccount)
		if err != nil {
			log.Fatalf("failed to save config: %v", err)
		}
		if !existed {
			log.Fatalf("account %q not found", *removeAccount)
		}
		fmt.Printf("Removed account %q.\n", *removeAccount)
		return
	}

	// Normal mode
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// First run — generate config
	if cfg == nil {
		cfg2, err := defaultConfig()
		if err != nil {
			log.Fatalf("create config: %v", err)
		}
		if err := writeConfigTemplate(cfgPath, &cfg2); err != nil {
			log.Fatalf("create config: %v", err)
		}
		fmt.Printf(`cmdcode2api initialized.

Created config: %s
Local client key: %s

Next:
  1. Run ./cmdcode2api --oauth --account <name> to connect a Command Code account.
  2. Run ./cmdcode2api again to start the local OpenAI-compatible API.

Use the local client key above as the Bearer token for your OpenAI client.
`, cfgPath, cfg2.APIKey)
		os.Exit(0)
	}

	// Starting with no accounts configured is allowed — the first account can be
	// added entirely through the browser at /ui, so running the --oauth CLI
	// command first is no longer required.
	if len(cfg.Accounts) == 0 {
		fmt.Println("No Command Code accounts are configured yet.")
		fmt.Println("Starting anyway — add your first account from the web UI (/ui) once the server is running.")
	}
	applyDefaultBaseURLs(cfg.Accounts)

	if cfg.Port == 0 {
		cfg.Port = 11434
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *debug {
		cfg.Debug = true
		debugMode = true
	}
	if *allowLAN {
		cfg.AllowLAN = true
	}
	if *allowTailscale {
		cfg.AllowTailscale = true
	}
	if (cfg.AllowLAN || cfg.AllowTailscale) && cfg.Host == "localhost" {
		log.Printf("allow_lan/allow_tailscale enabled: switching host from localhost to 0.0.0.0 so the network can actually reach it")
		cfg.Host = "0.0.0.0"
	}

	pool := NewAccountPool(cfg.Accounts)

	// store is the single writer for config.yaml from here on — both the
	// reauth/"add account" flow below and the web UI's /accounts/delete go
	// through it, so config.yaml is never written from two goroutines at
	// once.
	store := NewConfigStore(cfgPath, cfg)

	reauthMgr := NewReauthManager(func(name string, cb oauthCallback) {
		baseURL := store.BaseURLFor(name, defaultCommandCodeBaseURL)
		acct := buildAccountFromCallback(name, cb, baseURL)
		if err := store.Upsert(acct); err != nil {
			log.Printf("[WARN] account %q re-authorized but failed to save config: %v", name, err)
			return
		}
		pool.UpdateAccount(acct)
		log.Printf("[INFO] account %q re-authorized", name)
	})

	usage := loadUsage()

	// Model catalog is shared across Command Code accounts, so one call
	// (against the first account) is enough — no need to fetch per-account.
	// With zero accounts configured there is nothing to fetch it with yet;
	// cfg.Accounts[0] would panic on an empty slice, so skip the fetch and
	// leave the catalog empty until a restart after the first account is
	// added (GET /v1/models simply returns no models in the meantime; chat
	// completions are unaffected since they don't consult the catalog).
	if len(cfg.Accounts) > 0 {
		FetchProviderModels(cfg.Accounts[0].BaseURL, cfg.Accounts[0].APIKey)
	}

	billingTracker := NewBillingTracker()
	discordConfig := cfg.DiscordAlerts
	if discordConfig.WebhookURL == "" {
		discordConfig.WebhookURL = cfg.DiscordWebhookURL
	}
	if !discordConfig.Enabled && !cfg.DiscordAlertsEnabledSet && discordConfig.WebhookURL != "" {
		discordConfig.Enabled = true
	}
	if discordConfig.StateFile == "" {
		discordConfig.StateFile = cfg.DiscordAlertStateFile
	}
	if discordConfig.Enabled && discordConfig.WebhookURL != "" {
		billingTracker.SetDiscordAlerter(NewDiscordAlerterWithConfig(discordConfig))
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool.StartHealthChecks(ctx, healthCheckInterval)
	billingTracker.StartAutoRefresh(ctx, store, billingFetchInterval)

	err = runServer(pool, cfg, usage, reauthMgr, store, billingTracker)
	cancel()
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	if err := usage.save(); err != nil {
		log.Printf("save usage: %v", err)
	}
}

func findConfig() string {
	return configFile
}

// buildAccountFromCallback builds an Account named name from a completed
// OAuth callback, using baseURL for base_url (the caller decides whether
// that's an existing account's configured URL or the default). Shared by
// the --oauth CLI path and the reauth onSuccess closure so both persist the
// same identity fields (Email/UserID/KeyName) and can't drift apart.
func buildAccountFromCallback(name string, cb oauthCallback, baseURL string) Account {
	identity := cb.UserName
	if identity == "" {
		identity = cb.KeyName
	}
	return Account{
		Name:    name,
		APIKey:  cb.APIKey,
		BaseURL: baseURL,
		Email:   identity,
		UserID:  cb.UserID,
		KeyName: cb.KeyName,
	}
}
