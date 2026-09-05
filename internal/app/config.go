package app

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// AccountConfig is one upstream Command Code credential in config.yaml.
type AccountConfig struct {
	Name    string `yaml:"name"`
	APIKey  string `yaml:"api_key"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil means enabled
}

func (a AccountConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

type Config struct {
	APIKey string `yaml:"api_key"`
	// AdminPassword guards the WebUI admin API. Generated on first start when
	// empty; changeable at runtime via the admin API.
	AdminPassword string `yaml:"admin_password,omitempty"`
	// WebUI controls whether the binary serves the embedded admin interface.
	// nil means enabled.
	WebUI *bool  `yaml:"webui,omitempty"`
	Host  string `yaml:"host"`
	Port  int    `yaml:"port"`

	CommandCode struct {
		// APIKey is the legacy single-account field. It is migrated into
		// Accounts on load and cleared on save once accounts exist.
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	} `yaml:"commandcode"`

	ExcludeModels []string `yaml:"exclude_models"`
	Debug         bool     `yaml:"-"` // runtime flag, not persisted

	// mu guards the fields that the admin API mutates while request handlers
	// read them (ExcludeModels, CommandCode.BaseURL, AdminPassword).
	mu sync.RWMutex
}

func (c *Config) WebUIEnabled() bool {
	return c.WebUI == nil || *c.WebUI
}

func (c *Config) Excludes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ExcludeModels
}

func (c *Config) SetExcludes(list []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ExcludeModels = list
}

func (c *Config) UpstreamBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CommandCode.BaseURL
}

func (c *Config) SetUpstreamBaseURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CommandCode.BaseURL = url
}

func (c *Config) adminPassword() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AdminPassword
}

func (c *Config) setAdminPassword(password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AdminPassword = password
}

func defaultConfig() (*Config, error) {
	apiKey, err := genAPIKey()
	if err != nil {
		return nil, err
	}
	adminPassword, err := genAdminPassword()
	if err != nil {
		return nil, err
	}
	c := &Config{
		APIKey:        apiKey,
		AdminPassword: adminPassword,
		Host:          "localhost",
		Port:          11434,
		ExcludeModels: []string{"gpt-", "claude-", "gemini-"},
	}
	c.CommandCode.BaseURL = "https://api.commandcode.ai"
	return c, nil
}

func genAPIKey() (string, error) {
	key, err := randomHex(24)
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "ccgw-" + key, nil
}

func genAdminPassword() (string, error) {
	key, err := randomHex(18)
	if err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	return "ccgw-admin-" + key, nil
}

// loadConfig reads config.yaml and migrates the legacy single-key field into
// the accounts list.
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
	if len(cfg.CommandCode.Accounts) == 0 && cfg.CommandCode.APIKey != "" {
		cfg.CommandCode.Accounts = []AccountConfig{{Name: "default", APIKey: cfg.CommandCode.APIKey}}
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	cfg.mu.Lock()
	// A non-empty accounts list is the source of truth; keeping the legacy
	// field would resurrect a deleted key on the next load.
	if len(cfg.CommandCode.Accounts) > 0 {
		cfg.CommandCode.APIKey = ""
	}
	data, err := yaml.Marshal(cfg)
	cfg.mu.Unlock()
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
		"# exclude_models is enabled by default for premium/non-open-source models\n" +
		"# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
		"# Remove entries below or set exclude_models: [] to make all models available.\n" +
		"\n" +
		"# commandcode.accounts holds one or more upstream API keys; requests are\n" +
		"# rotated across them. admin_password guards the WebUI admin API.\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
