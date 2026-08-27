package app

import (
	"sync"
	"testing"
)

// Every model is disabled by default when it has no explicit override —
// cfg.ExcludeModels is a legacy field that is decoded but never consulted.
func TestModelPolicyDisabledByDefaultIgnoresExcludeModels(t *testing.T) {
	policy := NewModelPolicy(&Config{ExcludeModels: []string{"gpt-"}})

	if policy.Enabled("openai/gpt-4") {
		t.Fatal("openai/gpt-4 should be disabled by default")
	}
	if policy.Enabled("deepseek/deepseek-chat") {
		t.Fatal("deepseek/deepseek-chat should be disabled by default (no override, ExcludeModels ignored)")
	}
}

// An explicit true override is the only way to enable a model.
func TestModelPolicyExplicitTrueOverrideEnablesModel(t *testing.T) {
	policy := NewModelPolicy(&Config{ModelOverrides: map[string]bool{"openai/gpt-4": true}})

	if !policy.Enabled("openai/gpt-4") {
		t.Fatal("openai/gpt-4 should be enabled via explicit override")
	}
	if policy.Enabled("deepseek/deepseek-chat") {
		t.Fatal("deepseek/deepseek-chat should remain disabled (no override)")
	}
}

func TestModelPolicyFamilyToggleExpandsToExactCurrentModels(t *testing.T) {
	store := testStore(t)
	oldCatalog := modelCatalogDetail
	t.Cleanup(func() { modelCatalogDetail = oldCatalog })
	modelCatalogDetail = []CCProviderModel{
		{ID: "deepseek/deepseek-v4-pro"},
		{ID: "deepseek/deepseek-v4-flash"},
		{ID: "other/model"},
	}
	policy := NewModelPolicy(&Config{FamilyOverrides: map[string]bool{"deepseek": false}})

	if err := policy.SetFamilyOverride(store, "deepseek", false); err != nil {
		t.Fatalf("SetFamilyOverride: %v", err)
	}
	if policy.Enabled("deepseek/deepseek-v4-pro") || policy.Enabled("deepseek/deepseek-v4-flash") {
		t.Fatal("family members should be disabled")
	}
	if policy.Enabled("other/model") {
		t.Fatal("other family should remain disabled by default (no override)")
	}

	if err := policy.SetFamilyOverride(store, "deepseek", true); err != nil {
		t.Fatalf("SetFamilyOverride on: %v", err)
	}
	for _, id := range []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"} {
		if !policy.Enabled(id) {
			t.Fatalf("%s should be enabled after family toggle on", id)
		}
	}
	reloaded, err := loadConfig(store.path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(reloaded.FamilyOverrides) != 0 {
		t.Fatalf("family_overrides = %#v, want empty", reloaded.FamilyOverrides)
	}
	for _, id := range []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"} {
		if enabled, ok := reloaded.ModelOverrides[id]; !ok || !enabled {
			t.Fatalf("model_overrides[%s] = %v, %v, want true, true", id, enabled, ok)
		}
	}
}

// Precedence: explicit per-model override beats the default-disabled
// fallback, in both directions.
func TestModelPolicyPrecedenceOverrideBeatsDefault(t *testing.T) {
	store := testStore(t)
	policy := NewModelPolicy(&Config{})

	// No override at all: disabled.
	if policy.Enabled("deepseek/deepseek-v4-pro") {
		t.Fatal("expected disabled by default with no override")
	}

	if err := policy.SetModelOverrides(store, []string{"deepseek/deepseek-v4-pro"}, false); err != nil {
		t.Fatalf("SetModelOverrides: %v", err)
	}
	if policy.Enabled("deepseek/deepseek-v4-pro") {
		t.Fatal("explicit false override should keep the model disabled")
	}
	if policy.Enabled("deepseek/deepseek-v4-flash") {
		t.Fatal("sibling model with no override should still be disabled by default")
	}
}

func TestModelPolicySetModelOverridesPersistsAndReloads(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	cfg := &Config{}
	store := NewConfigStore(path, cfg)
	policy := NewModelPolicy(cfg)

	if err := policy.SetModelOverrides(store, []string{"a/model-1", "a/model-2"}, false); err != nil {
		t.Fatalf("SetModelOverrides: %v", err)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if v, ok := reloaded.ModelOverrides["a/model-1"]; !ok || v {
		t.Fatalf("reloaded model_overrides[a/model-1] = %v, %v, want false, true", v, ok)
	}
	if v, ok := reloaded.ModelOverrides["a/model-2"]; !ok || v {
		t.Fatalf("reloaded model_overrides[a/model-2] = %v, %v, want false, true", v, ok)
	}
	if len(reloaded.FamilyOverrides) != 0 {
		t.Fatalf("reloaded family_overrides = %#v, want empty", reloaded.FamilyOverrides)
	}

	// A freshly constructed policy from the reloaded config must agree.
	reloadedPolicy := NewModelPolicy(reloaded)
	if reloadedPolicy.Enabled("a/model-1") {
		t.Fatal("reloaded policy should keep a/model-1 disabled")
	}
	if reloadedPolicy.Enabled("a/family-anything") {
		t.Fatal("reloaded policy should keep an unoverridden model disabled by default")
	}
}

// A failed disk write must not leave ModelPolicy's in-memory state disagreeing
// with what was actually persisted (nothing, in this case) — the in-memory
// map is only swapped in after a successful write.
func TestModelPolicyRollsBackOnWriteFailure(t *testing.T) {
	// A directory in place of the config file makes os.WriteFile fail.
	dir := t.TempDir()
	badPath := dir // saveConfig will try to write to this directory path itself
	cfg := &Config{}
	store := NewConfigStore(badPath, cfg)
	policy := NewModelPolicy(cfg)

	if err := policy.SetModelOverrides(store, []string{"a/model-1"}, false); err == nil {
		t.Fatal("expected SetModelOverrides to fail when the store's path is unwritable")
	}
	if _, ok := policy.ModelOverride("a/model-1"); ok {
		t.Fatal("failed write must not be reflected in the policy's in-memory overrides")
	}
	if policy.Enabled("a/model-1") {
		t.Fatal("a/model-1 should still fall through to the (disabled) default after a failed write")
	}

}

// Concurrent Enabled reads racing against SetModelOverrides/SetFamilyOverride
// writes must be race-free — this is the entire point of ModelPolicy owning
// its own sync.RWMutex-guarded copy of the override state instead of reading
// mutable state off *Config directly. Run with `go test -race`.
func TestModelPolicyConcurrentAccessIsRaceFree(t *testing.T) {
	store := testStore(t)
	policy := NewModelPolicy(&Config{})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					policy.Enabled("deepseek/deepseek-v4-pro")
					policy.ModelOverride("deepseek/deepseek-v4-pro")
					policy.FamilyOverride("deepseek")
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			_ = policy.SetModelOverrides(store, []string{"deepseek/deepseek-v4-pro"}, i%4 == 0)
		} else {
			_ = policy.SetModelOverrides(store, []string{"deepseek/deepseek-v4-flash"}, i%3 == 0)
		}
	}
	close(stop)
	wg.Wait()
}
