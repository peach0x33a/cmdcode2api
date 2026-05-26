package main

import (
	"crypto/rand"
	"encoding/hex"
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
	Port int `yaml:"port"`
}

func defaultConfig() Config {
	c := Config{
		APIKey: genAPIKey(),
		Port:   11434,
	}
	c.CommandCode.BaseURL = "https://api.commandcode.ai"
	return c
}

func genAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "ccgw-" + hex.EncodeToString(b)
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
