package app

import "sync"

// ModelPolicy is the single source of truth request-path code (dispatchToCC,
// handleModels, adminModels) consults to decide whether a model is enabled.
// It owns its own copy of model_overrides rather than reading it off *Config
// directly: NewConfigStore and newHandler are handed the same *Config
// pointer, and ConfigStore's Upsert/Remove/SetSessionToken methods mutate it
// without any lock request-path code shares — see the concurrency note on
// that sharing in app.go/server.go. Guarding this state with its own
// sync.RWMutex (mirroring AccountPool, UsageTracker, and BillingTracker
// elsewhere in this codebase) keeps every read/write here race-free
// independent of how *Config itself is touched.
type ModelPolicy struct {
	mu             sync.RWMutex
	modelOverrides map[string]bool
}

// NewModelPolicy deep-copies cfg's ModelOverrides into a fresh ModelPolicy.
// Legacy FamilyOverrides and ExcludeModels are intentionally ignored — every
// model is disabled unless it has an explicit true override. cfg is read
// once, here, at construction time; every later change must go through
// SetModelOverrides/SetFamilyOverride below, which persist through a
// ConfigStore before updating this policy's in-memory copy.
func NewModelPolicy(cfg *Config) *ModelPolicy {
	p := &ModelPolicy{
		modelOverrides: make(map[string]bool, len(cfg.ModelOverrides)),
	}
	for id, enabled := range cfg.ModelOverrides {
		p.modelOverrides[id] = enabled
	}
	return p
}

// Enabled reports whether modelID should be usable — the single decision
// point dispatchToCC, handleModels, and adminModels all consult. A model is
// enabled only if it has an explicit true entry in model_overrides; every
// other model — including ones with no override at all — is disabled by
// default.
func (p *ModelPolicy) Enabled(modelID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabledLocked(modelID)
}

func (p *ModelPolicy) enabledLocked(modelID string) bool {
	if enabled, ok := p.modelOverrides[modelID]; ok {
		return enabled
	}
	return false
}

// ModelOverride reports whether modelID has an explicit per-model override
// and, if so, its enabled value — used by adminModels to fill AdminModel's
// Overridden flag.
func (p *ModelPolicy) ModelOverride(modelID string) (enabled bool, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	enabled, ok = p.modelOverrides[modelID]
	return enabled, ok
}

// FamilyOverride is retained for compatibility. Family overrides are no
// longer stored or consulted.
func (p *ModelPolicy) FamilyOverride(family string) (enabled bool, ok bool) {
	return false, false
}

// Describe reports whether modelID is enabled and whether that state comes
// from an explicit per-model override — everything adminModels needs for one
// catalog entry, gathered
// under a single RLock instead of the three separate locked calls
// (ModelOverride, FamilyOverride, Enabled) it used to make per model.
func (p *ModelPolicy) Describe(modelID, family string) (enabled bool, overridden bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if _, ok := p.modelOverrides[modelID]; ok {
		overridden = true
	}
	return p.enabledLocked(modelID), overridden
}

// SetModelOverrides applies an explicit enabled/disabled override for every
// ID in ids, persisting the merged map through store before swapping it into
// this policy's in-memory copy — so a failed disk write never leaves memory
// and config.yaml disagreeing about which models are overridden.
func (p *ModelPolicy) SetModelOverrides(store *ConfigStore, ids []string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	changes := make(map[string]bool, len(ids))
	for _, id := range ids {
		changes[id] = enabled
	}
	next, err := applyOverrides(p.modelOverrides, changes, store.SetModelOverrides)
	if err != nil {
		return err
	}
	p.modelOverrides = next
	return nil
}

// SetFamilyOverride expands family to the current catalog and persists exact
// per-model overrides. No family rule is stored.
func (p *ModelPolicy) SetFamilyOverride(store *ConfigStore, family string, enabled bool) error {
	ids := make([]string, 0)
	for _, model := range modelCatalogDetail {
		if groupModel(model.ID).Family == family {
			ids = append(ids, model.ID)
		}
	}
	return p.SetModelOverrides(store, ids, enabled)
}

// applyOverrides copies current, merges changes on top of it, and persists
// the merged map via persist. It returns the merged map only on a successful
// persist, so SetModelOverrides/SetFamilyOverride can swap it into place
// under the lock they already hold without risking memory and config.yaml
// disagreeing after a failed disk write.
func applyOverrides[T comparable](current, changes map[T]bool, persist func(map[T]bool) error) (map[T]bool, error) {
	next := make(map[T]bool, len(current)+len(changes))
	for k, v := range current {
		next[k] = v
	}
	for k, v := range changes {
		next[k] = v
	}
	if err := persist(next); err != nil {
		return nil, err
	}
	return next, nil
}
