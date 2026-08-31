package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// A reauth rebuilds an Account purely from the OAuth callback (no billing
// session fields) and feeds it through upsertAccount. That must not wipe the
// billing session token / identity a previous /auth/get-session fetch
// persisted — otherwise every post-sleep reauth silently breaks billing calls
// until the token is pasted back in by hand.
func TestUpsertAccountPreservesBillingSessionOnReauth(t *testing.T) {
	expires := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	planExpires := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cfg := &Config{Accounts: []Account{{
		Name:             "work",
		APIKey:           "old-key",
		BaseURL:          "https://api.commandcode.ai",
		SessionToken:     "sess-abc123",
		SessionEmail:     "me@example.com",
		SessionExpiresAt: &expires,
		PlanExpiresAt:    &planExpires,
	}}}

	reauthed := buildAccountFromCallback("work", oauthCallback{
		APIKey:   "new-key",
		UserID:   "u1",
		UserName: "me@example.com",
		KeyName:  "k1",
	}, "https://api.commandcode.ai")
	upsertAccount(cfg, reauthed)

	got := cfg.Accounts[0]
	if got.APIKey != "new-key" {
		t.Fatalf("APIKey = %q, want the reauthed key", got.APIKey)
	}
	if got.SessionToken != "sess-abc123" {
		t.Fatalf("SessionToken = %q, want it preserved across reauth", got.SessionToken)
	}
	if got.SessionEmail != "me@example.com" {
		t.Fatalf("SessionEmail = %q, want it preserved across reauth", got.SessionEmail)
	}
	if got.SessionExpiresAt == nil || !got.SessionExpiresAt.Equal(expires) {
		t.Fatalf("SessionExpiresAt = %v, want it preserved across reauth", got.SessionExpiresAt)
	}
	if got.PlanExpiresAt == nil || !got.PlanExpiresAt.Equal(planExpires) {
		t.Fatalf("PlanExpiresAt = %v, want it preserved across reauth", got.PlanExpiresAt)
	}
}

// Explicit non-blank values in the incoming account always win over the
// existing ones, so SetSessionToken/SetSessionIdentity-style overwrites are not
// blocked by the reauth backfill.
func TestUpsertAccountIncomingSessionFieldsWin(t *testing.T) {
	oldExpires := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newExpires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	oldPlan := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	newPlan := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	cfg := &Config{Accounts: []Account{{
		Name:             "work",
		SessionToken:     "old-tok",
		SessionEmail:     "old@example.com",
		SessionExpiresAt: &oldExpires,
		PlanExpiresAt:    &oldPlan,
	}}}

	upsertAccount(cfg, Account{
		Name:             "work",
		SessionToken:     "new-tok",
		SessionEmail:     "new@example.com",
		SessionExpiresAt: &newExpires,
		PlanExpiresAt:    &newPlan,
	})

	got := cfg.Accounts[0]
	if got.SessionToken != "new-tok" {
		t.Fatalf("SessionToken = %q, want %q", got.SessionToken, "new-tok")
	}
	if got.SessionEmail != "new@example.com" {
		t.Fatalf("SessionEmail = %q, want %q", got.SessionEmail, "new@example.com")
	}
	if got.SessionExpiresAt == nil || !got.SessionExpiresAt.Equal(newExpires) {
		t.Fatalf("SessionExpiresAt = %v, want %v", got.SessionExpiresAt, newExpires)
	}
	if got.PlanExpiresAt == nil || !got.PlanExpiresAt.Equal(newPlan) {
		t.Fatalf("PlanExpiresAt = %v, want %v", got.PlanExpiresAt, newPlan)
	}
}

// defaultConfig no longer seeds ExcludeModels (a legacy field): every model
// starts disabled and must be explicitly enabled via ModelOverrides instead.
func TestDefaultConfigHasNoExcludeModelsOrOverrides(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig error: %v", err)
	}
	if len(cfg.ExcludeModels) != 0 {
		t.Fatalf("ExcludeModels = %#v, want empty", cfg.ExcludeModels)
	}
	if len(cfg.ModelOverrides) != 0 {
		t.Fatalf("ModelOverrides = %#v, want empty", cfg.ModelOverrides)
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

func TestWriteConfigTemplateExplainsModelsDisabledByDefault(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, &cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Every model starts disabled") {
		t.Fatalf("missing disabled-by-default comment in:\n%s", content)
	}
	if !strings.Contains(content, "/ui#models") {
		t.Fatalf("missing Models tab pointer in:\n%s", content)
	}
	if !strings.Contains(content, "https://commandcode.ai/docs/plans/go#models") {
		t.Fatalf("missing Go plan docs link in:\n%s", content)
	}
	if !strings.Contains(content, cfg.APIKey) {
		t.Fatalf("missing actual config content in:\n%s", content)
	}
}

func TestLoadConfigMigratesLegacyCommandCodeAccount(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	legacyYAML := "api_key: ccgw-test\n" +
		"commandcode:\n" +
		"  api_key: legacy-cc-key\n" +
		"  base_url: https://api.commandcode.ai\n" +
		"host: localhost\n" +
		"port: 11434\n"
	if err := os.WriteFile(path, []byte(legacyYAML), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(cfg.Accounts))
	}
	acct := cfg.Accounts[0]
	if acct.Name != "default" || acct.APIKey != "legacy-cc-key" || acct.BaseURL != "https://api.commandcode.ai" {
		t.Fatalf("migrated account = %#v", acct)
	}

	// Migration must have re-saved the config in the new shape.
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if strings.Contains(string(saved), "commandcode:") {
		t.Fatalf("saved config still contains legacy commandcode key:\n%s", saved)
	}
	if !strings.Contains(string(saved), "name: default") {
		t.Fatalf("saved config missing migrated account name:\n%s", saved)
	}
	if !strings.Contains(string(saved), "legacy-cc-key") {
		t.Fatalf("saved config missing migrated api key:\n%s", saved)
	}
}

func TestLoadConfigDoesNotMigrateWhenAccountsAlreadyPresent(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	yamlData := "accounts:\n" +
		"  - name: work\n" +
		"    api_key: work-key\n" +
		"    base_url: https://api.commandcode.ai\n"
	if err := os.WriteFile(path, []byte(yamlData), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Name != "work" || cfg.Accounts[0].APIKey != "work-key" {
		t.Fatalf("accounts = %#v", cfg.Accounts)
	}
}

func TestLoadConfigNoAccountsNoLegacyKeyStaysEmpty(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	yamlData := "host: localhost\nport: 11434\n"
	if err := os.WriteFile(path, []byte(yamlData), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Accounts) != 0 {
		t.Fatalf("Accounts = %#v, want empty", cfg.Accounts)
	}
}

func TestLoadConfigLegacyAlertEnabledDetectionIsStructural(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		enabled bool
	}{
		{name: "commented enabled", yaml: "# enabled: true\ndiscord_webhook_url: https://discord.example/webhook\n", enabled: true},
		{name: "unrelated enabled", yaml: "enabled: true\ndiscord_webhook_url: https://discord.example/webhook\n", enabled: true},
		{name: "nested explicit false", yaml: "discord_webhook_url: https://discord.example/webhook\ndiscord_alerts:\n  enabled: false\n", enabled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir() + "/config.yaml"
			if err := os.WriteFile(path, []byte(tt.yaml), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := loadConfig(path)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.DiscordAlerts.Enabled != tt.enabled {
				t.Fatalf("enabled = %v, want %v", cfg.DiscordAlerts.Enabled, tt.enabled)
			}
		})
	}
}

func TestConfigModelAndFamilyOverridesRoundTrip(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{
		APIKey: "ccgw-test",
		Host:   "localhost",
		Port:   11434,
		ModelOverrides: map[string]bool{
			"deepseek/deepseek-v4-pro": false,
			"openai/gpt-4":             true,
		},
		FamilyOverrides: map[string]bool{
			"deepseek/deepseek-v4": true,
		},
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(loaded.ModelOverrides) != 2 {
		t.Fatalf("ModelOverrides = %#v, want 2 entries", loaded.ModelOverrides)
	}
	if v, ok := loaded.ModelOverrides["deepseek/deepseek-v4-pro"]; !ok || v {
		t.Fatalf("ModelOverrides[deepseek/deepseek-v4-pro] = %v, %v, want false, true", v, ok)
	}
	if v, ok := loaded.ModelOverrides["openai/gpt-4"]; !ok || !v {
		t.Fatalf("ModelOverrides[openai/gpt-4] = %v, %v, want true, true", v, ok)
	}
	if len(loaded.FamilyOverrides) != 1 {
		t.Fatalf("FamilyOverrides = %#v, want 1 entry", loaded.FamilyOverrides)
	}
	if v, ok := loaded.FamilyOverrides["deepseek/deepseek-v4"]; !ok || !v {
		t.Fatalf("FamilyOverrides[deepseek/deepseek-v4] = %v, %v, want true, true", v, ok)
	}
}

func TestConfigModelAndFamilyOverridesAbsentAreNil(t *testing.T) {
	yamlData := "host: localhost\nport: 11434\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if cfg.ModelOverrides != nil {
		t.Fatalf("ModelOverrides = %v, want nil", cfg.ModelOverrides)
	}
	if cfg.FamilyOverrides != nil {
		t.Fatalf("FamilyOverrides = %v, want nil", cfg.FamilyOverrides)
	}
}

// A legacy config.yaml written before model_overrides/family_overrides
// existed (only exclude_models present) must still load cleanly, with both
// new maps nil rather than erroring or defaulting to some non-nil zero
// value.
func TestConfigLegacyExcludeModelsOnlyStillLoads(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	legacyYAML := "api_key: ccgw-test\n" +
		"host: localhost\n" +
		"port: 11434\n" +
		"exclude_models:\n" +
		"  - gpt-\n" +
		"  - claude-\n" +
		"  - gemini-\n"
	if err := os.WriteFile(path, []byte(legacyYAML), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.ExcludeModels) != 3 {
		t.Fatalf("ExcludeModels = %#v, want 3 entries", cfg.ExcludeModels)
	}
	if cfg.ModelOverrides != nil {
		t.Fatalf("ModelOverrides = %v, want nil for a legacy config", cfg.ModelOverrides)
	}
	if cfg.FamilyOverrides != nil {
		t.Fatalf("FamilyOverrides = %v, want nil for a legacy config", cfg.FamilyOverrides)
	}
}

// The template written for a fresh install must round-trip with no
// ExcludeModels/ModelOverrides seeded — every model stays disabled until a
// human explicitly enables it from the Models tab.
func TestWriteConfigTemplateLoadsBackWithNoModelState(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, &cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	loaded, err := loadConfig(tmp)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(loaded.ExcludeModels) != 0 {
		t.Fatalf("ExcludeModels = %#v, want empty", loaded.ExcludeModels)
	}
	if len(loaded.ModelOverrides) != 0 {
		t.Fatalf("ModelOverrides = %#v, want empty", loaded.ModelOverrides)
	}
}
