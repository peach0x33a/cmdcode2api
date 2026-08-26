package app

import (
	"fmt"
	"os"

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
}

type Config struct {
	APIKey        string    `yaml:"api_key"`
	Accounts      []Account `yaml:"accounts"`
	Host          string    `yaml:"host"`
	Port          int       `yaml:"port"`
	ExcludeModels []string  `yaml:"exclude_models"`
	Debug         bool      `yaml:"-"` // runtime flag, not persisted
}

func defaultConfig() (Config, error) {
	apiKey, err := genAPIKey()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		APIKey:        apiKey,
		Host:          "localhost",
		Port:          11434,
		ExcludeModels: []string{"gpt-", "claude-", "gemini-"},
	}
	return c, nil
}

// upsertAccount replaces the account with a matching name, or appends it if
// no account with that name exists yet.
func upsertAccount(cfg *Config, acct Account) {
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == acct.Name {
			cfg.Accounts[i] = acct
			return
		}
	}
	cfg.Accounts = append(cfg.Accounts, acct)
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
		"# exclude_models is enabled by default for premium/non-open-source models\n" +
		"# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
		"# Remove entries below or set exclude_models: [] to make all models available.\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
