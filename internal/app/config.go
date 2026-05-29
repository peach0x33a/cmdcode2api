package app

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	APIKey      string `yaml:"api_key"`
	CommandCode struct {
		APIKey  string `yaml:"api_key"`
		BaseURL string `yaml:"base_url"`
	} `yaml:"commandcode"`
	Host          string   `yaml:"host"`
	Port          int      `yaml:"port"`
	ExcludeModels []string `yaml:"exclude_models"`
	Debug         bool     `yaml:"-"` // runtime flag, not persisted
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
		"# Optional: exclude models from /v1/models and /v1/chat/completions.\n" +
		"# Uncomment the lines below to hide premium/non-open-source models\n" +
		"# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
		"# exclude_models:\n" +
		"#     - gpt-\n" +
		"#     - claude-\n" +
		"#     - gemini-\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
