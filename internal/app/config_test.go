package app

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfigHasExcludeModels(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig error: %v", err)
	}
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(cfg.ExcludeModels) != len(want) {
		t.Fatalf("len(ExcludeModels) = %d, want %d", len(cfg.ExcludeModels), len(want))
	}
	for i, v := range want {
		if cfg.ExcludeModels[i] != v {
			t.Fatalf("ExcludeModels[%d] = %q, want %q", i, cfg.ExcludeModels[i], v)
		}
	}
}

func TestDefaultConfigUsesLocalhost(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig error: %v", err)
	}
	if cfg.Host != "localhost" {
		t.Fatalf("host = %q", cfg.Host)
	}
	if cfg.Port != 11434 {
		t.Fatalf("port = %d", cfg.Port)
	}
}

func TestLoadConfigExcludeModels(t *testing.T) {
	yamlData := "exclude_models:\n  - gpt-\n  - claude-\n  - gemini-\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(cfg.ExcludeModels) != len(want) {
		t.Fatalf("len(ExcludeModels) = %d, want %d", len(cfg.ExcludeModels), len(want))
	}
	for i := range want {
		if cfg.ExcludeModels[i] != want[i] {
			t.Fatalf("ExcludeModels[%d] = %q, want %q", i, cfg.ExcludeModels[i], want[i])
		}
	}
}

func TestLoadConfigNoExcludeModels(t *testing.T) {
	yamlData := "host: localhost\nport: 11434\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if cfg.ExcludeModels != nil {
		t.Fatalf("ExcludeModels = %v, want nil", cfg.ExcludeModels)
	}
}

func TestLoadConfigEmptyExcludeModels(t *testing.T) {
	yamlData := "exclude_models: []\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(cfg.ExcludeModels) != 0 {
		t.Fatalf("len(ExcludeModels) = %d, want 0", len(cfg.ExcludeModels))
	}
}

func TestWriteConfigTemplateIncludesComment(t *testing.T) {
	cfg := &Config{APIKey: "test-key"}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# exclude_models:") {
		t.Fatalf("missing commented exclude_models in:\n%s", content)
	}
	if !strings.Contains(content, "#     - gpt-") {
		t.Fatalf("missing commented gpt- entry in:\n%s", content)
	}
	if !strings.Contains(content, "test-key") {
		t.Fatalf("missing actual config content in:\n%s", content)
	}
}

func TestWriteConfigTemplateExcludeCommented(t *testing.T) {
	cfg := &Config{APIKey: "test-key"}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	loaded, err := loadConfig(tmp)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(loaded.ExcludeModels) != 0 {
		t.Fatalf("ExcludeModels = %v, want empty (template is commented out)", loaded.ExcludeModels)
	}
}
