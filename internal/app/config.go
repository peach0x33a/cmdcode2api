package app

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultCommandCodeBaseURL is used for any account that does not specify
// its own base_url (all accounts share the same Command Code API today).
const defaultCommandCodeBaseURL = "https://api.commandcode.ai"

// Account is one Command Code login: a personal API key obtained via
// --oauth --account <name>, plus the base URL it talks to.
type Account struct {
	Name    string `yaml:"name"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`

	// Email is not a validated email address — it is whatever identity
	// string the OAuth callback returned (userName, falling back to
	// keyName), kept only so a human can tell which login an account
	// belongs to. UserID/KeyName are the raw callback fields it was
	// derived from, kept around for completeness/debugging.
	Email   string `yaml:"email,omitempty"`
	UserID  string `yaml:"user_id,omitempty"`
	KeyName string `yaml:"key_name,omitempty"`

	// SessionToken is the __Secure-commandcode_prod_.session_token cookie
	// value used to authenticate billing API calls (see billing.go). It is a
	// secret, exactly like APIKey: never included in AccountView or any other
	// JSON-exposed struct.
	//
	// This and the two SessionEmail/SessionExpiresAt fields below are the
	// "billing session" group that preserveSessionFields carries across an
	// OAuth reauth (which otherwise rebuilds an Account with these blank). Add
	// any future billing-session field to that helper's list too.
	SessionToken string `yaml:"session_token,omitempty"`

	// SessionEmail/SessionExpiresAt identify which commandcode.ai login the
	// session token belongs to and when that session expires, as reported by
	// /auth/get-session. They are not credentials, just metadata kept
	// durable across restarts for a future alerting mechanism that watches
	// for tokens about to expire.
	SessionEmail     string     `yaml:"session_email,omitempty"`
	SessionExpiresAt *time.Time `yaml:"session_expires_at,omitempty"`
}

type Config struct {
	APIKey                string             `yaml:"api_key"`
	Accounts              []Account          `yaml:"accounts"`
	Host                  string             `yaml:"host"`
	Port                  int                `yaml:"port"`
	DiscordWebhookURL     string             `yaml:"discord_webhook_url,omitempty"`
	DiscordAlertStateFile string             `yaml:"discord_alert_state_file,omitempty"`
	DiscordAlerts         DiscordAlertConfig `yaml:"discord_alerts,omitempty"`

	// ExcludeModels is a legacy compatibility field from when models were
	// enabled by default and this prefix-blocklist was the only way to turn
	// any off. It is still decoded from old config.yaml files so they load
	// without error, but it is never consulted — every model now starts
	// disabled and must be explicitly enabled via ModelOverrides (see
	// ModelPolicy.Enabled).
	ExcludeModels []string `yaml:"exclude_models"`

	// AllowLAN and AllowTailscale opt into binding beyond loopback so other
	// devices on the local network / a Tailscale tailnet can reach this
	// gateway. Neither weakens admin auth: the loopback bypass in
	// isLocalAdminRequest still only applies to loopback callers, so a LAN
	// or Tailscale caller needs the bearer APIKey exactly like any other
	// remote caller — see runServer and app.go's Run for how these flags
	// drive the actual bind address and startup detection.
	AllowLAN       bool `yaml:"allow_lan"`
	AllowTailscale bool `yaml:"allow_tailscale"`

	// DetectedLANIP and DetectedTailscaleIP are populated once at startup
	// (see runServer) purely for the startup log message — not persisted.
	DetectedLANIP       string `yaml:"-"`
	DetectedTailscaleIP string `yaml:"-"`

	// ModelOverrides is an explicit per-model enabled/disabled override, keyed
	// by exact catalog model ID. Every model is disabled unless it has a true
	// entry here — see ModelPolicy.Enabled for the full decision rule.
	ModelOverrides map[string]bool `yaml:"model_overrides,omitempty"`

	// FamilyOverrides is a legacy compatibility field. Family state is never
	// consulted, and family toggles expand to exact ModelOverrides entries.
	FamilyOverrides map[string]bool `yaml:"family_overrides,omitempty"`

	Debug bool `yaml:"-"` // runtime flag, not persisted

	// DiscordAlertsEnabledSet distinguishes an explicit false from legacy
	// configurations where the webhook URL implied enabled=true.
	DiscordAlertsEnabledSet bool `yaml:"-"`
}

// DiscordAlertConfig controls optional billing alerts. A zero cap is replaced
// by the documented default when the alerter is constructed. MonthlyCredits
// is a remaining balance, not usage, so monthly alerts remain disabled until
// the API exposes a reliable monthly-used value.
type DiscordAlertConfig struct {
	Enabled         bool    `yaml:"enabled"`
	WebhookURL      string  `yaml:"webhook_url,omitempty"`
	StateFile       string  `yaml:"state_file,omitempty"`
	HourlyCap       float64 `yaml:"hourly_cap,omitempty"`
	WeeklyCap       float64 `yaml:"weekly_cap,omitempty"`
	MonthlyCap      float64 `yaml:"monthly_cap,omitempty"`
	MentionEveryone bool    `yaml:"mention_everyone,omitempty"`
}

func defaultConfig() (Config, error) {
	apiKey, err := genAPIKey()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		APIKey: apiKey,
		Host:   "localhost",
		Port:   11434,
	}
	return c, nil
}

// upsertAccount replaces the account with a matching name, or appends it if
// no account with that name exists yet. When it replaces, the incoming
// account's blank billing-session fields are backfilled from the existing one
// via preserveSessionFields first.
func upsertAccount(cfg *Config, acct Account) {
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == acct.Name {
			preserveSessionFields(&acct, cfg.Accounts[i])
			cfg.Accounts[i] = acct
			return
		}
	}
	cfg.Accounts = append(cfg.Accounts, acct)
}

// preserveSessionFields backfills incoming's billing-session fields
// (SessionToken/SessionEmail/SessionExpiresAt) from existing wherever incoming
// left them at their zero value. The OAuth reauth flows (app.go, both the CLI
// --oauth path and the HTTP /accounts/reauth path) rebuild an Account from
// just the OAuth callback, which carries none of these; without this backfill
// every reauth would silently wipe the billing session token from config.yaml.
// A non-blank incoming value always wins, so SetSessionToken-style updates are
// unaffected.
func preserveSessionFields(incoming *Account, existing Account) {
	if incoming.SessionToken == "" {
		incoming.SessionToken = existing.SessionToken
	}
	if incoming.SessionEmail == "" {
		incoming.SessionEmail = existing.SessionEmail
	}
	if incoming.SessionExpiresAt == nil {
		incoming.SessionExpiresAt = existing.SessionExpiresAt
	}
}

// applyDefaultBaseURLs fills in defaultCommandCodeBaseURL for any account
// that omitted base_url.
func applyDefaultBaseURLs(accounts []Account) {
	for i := range accounts {
		if accounts[i].BaseURL == "" {
			accounts[i].BaseURL = defaultCommandCodeBaseURL
		}
	}
}

func genAPIKey() (string, error) {
	key, err := randomHex(24)
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "ccgw-" + key, nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var raw struct {
		DiscordAlerts *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"discord_alerts"`
	}
	if err := yaml.Unmarshal(data, &raw); err == nil && raw.DiscordAlerts != nil && raw.DiscordAlerts.Enabled != nil {
		cfg.DiscordAlertsEnabledSet = true
	}

	// Pre-multi-account config.yaml files carried a single commandcode.api_key
	// instead of the accounts list. Detect that legacy shape (yaml.Unmarshal
	// above silently drops the unknown "commandcode" key into the new Config)
	// and migrate it into a single "default" account, then persist immediately
	// so this only runs once.
	if len(cfg.Accounts) == 0 {
		var legacy struct {
			CommandCode struct {
				APIKey  string `yaml:"api_key"`
				BaseURL string `yaml:"base_url"`
			} `yaml:"commandcode"`
		}
		if err := yaml.Unmarshal(data, &legacy); err == nil && legacy.CommandCode.APIKey != "" {
			cfg.Accounts = []Account{{
				Name:    "default",
				APIKey:  legacy.CommandCode.APIKey,
				BaseURL: legacy.CommandCode.BaseURL,
			}}
			if err := saveConfig(path, &cfg); err != nil {
				return nil, fmt.Errorf("migrate legacy commandcode config: %w", err)
			}
		}
	}
	// Configurations written before the explicit enabled switch used a
	// non-empty webhook URL as the enabled signal. Once the field is present,
	// preserve an explicit false so the UI's disable action survives restart.
	if !cfg.DiscordAlertsEnabledSet && (cfg.DiscordAlerts.WebhookURL != "" || cfg.DiscordWebhookURL != "") {
		cfg.DiscordAlerts.Enabled = true
	}

	return &cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func writeConfigTemplate(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	template := "# cmdcode2api configuration\n" +
		"# See README.md for all options.\n" +
		"\n" +
		"# Every model starts disabled. Enable only the ones your plan actually\n" +
		"# serves from the Models tab at http://localhost:11434/ui#models — see\n" +
		"# https://commandcode.ai/docs/plans/go#models for what's included on the\n" +
		"# Go plan.\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
