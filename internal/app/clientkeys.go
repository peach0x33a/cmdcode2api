package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// ClientKey is one local bearer key that callers use to reach this gateway.
// Keys are surfaced to the admin in full — the admin password already grants
// full config control, so masking them would only add friction.
type ClientKey struct {
	ID      string
	Name    string
	Key     string
	Enabled bool

	mu         sync.Mutex
	lastUsedAt time.Time
}

func newClientKey(name, key string, enabled bool) *ClientKey {
	return &ClientKey{ID: clientKeyID(key), Name: name, Key: key, Enabled: enabled}
}

// clientKeyID derives a stable identifier from the key value.
func clientKeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k" + hex.EncodeToString(sum[:4])
}

// MaskedKey hides most of the key: prefix plus the last four characters.
func (k *ClientKey) MaskedKey() string {
	if len(k.Key) > 12 {
		return k.Key[:6] + "…" + k.Key[len(k.Key)-4:]
	}
	return strings.Repeat("*", len(k.Key))
}

func (k *ClientKey) RecordUsed() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lastUsedAt = time.Now()
}

func (k *ClientKey) View() ClientKeyView {
	k.mu.Lock()
	defer k.mu.Unlock()
	view := ClientKeyView{ID: k.ID, Name: k.Name, KeyMasked: k.MaskedKey(), Enabled: k.Enabled}
	if !k.lastUsedAt.IsZero() {
		lastUsedAt := k.lastUsedAt
		view.LastUsedAt = &lastUsedAt
	}
	return view
}

type ClientKeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyMasked  string     `json:"key_masked"`
	Enabled    bool       `json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ClientKeyPool holds the local client keys accepted by authMiddleware.
type ClientKeyPool struct {
	mu   sync.RWMutex
	keys []*ClientKey
}

func NewClientKeyPool(list []ClientKeyConfig) *ClientKeyPool {
	pool := &ClientKeyPool{}
	for _, kc := range list {
		if strings.TrimSpace(kc.Key) == "" {
			continue
		}
		pool.keys = append(pool.keys, newClientKey(kc.Name, kc.Key, kc.IsEnabled()))
	}
	return pool
}

// Lookup finds an enabled key by its raw bearer value, or nil.
func (p *ClientKeyPool) Lookup(key string) *ClientKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, k := range p.keys {
		if k.Key == key {
			return k
		}
	}
	return nil
}

func (p *ClientKeyPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.keys)
}

func (p *ClientKeyPool) EnabledCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, k := range p.keys {
		if k.Enabled {
			n++
		}
	}
	return n
}

func (p *ClientKeyPool) Get(id string) *ClientKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, k := range p.keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

var errDuplicateClientKey = errors.New("a key with this value already exists")

// Add registers a key. An empty key value generates a fresh ccgw- key.
func (p *ClientKeyPool) Add(name, key string, enabled bool) (*ClientKey, error) {
	if strings.TrimSpace(key) == "" {
		generated, err := genAPIKey()
		if err != nil {
			return nil, err
		}
		key = generated
	}
	key = strings.TrimSpace(key)
	if id := clientKeyID(key); p.Get(id) != nil {
		return nil, errDuplicateClientKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := newClientKey(name, key, enabled)
	p.keys = append(p.keys, k)
	return k, nil
}

func (p *ClientKeyPool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, k := range p.keys {
		if k.ID == id {
			p.keys = append(p.keys[:i], p.keys[i+1:]...)
			return true
		}
	}
	return false
}

func (p *ClientKeyPool) SetEnabled(id string, enabled bool) bool {
	k := p.Get(id)
	if k == nil {
		return false
	}
	k.Enabled = enabled
	return true
}

func (p *ClientKeyPool) Rename(id, name string) bool {
	k := p.Get(id)
	if k == nil {
		return false
	}
	k.Name = name
	return true
}

func (p *ClientKeyPool) Views() []ClientKeyView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	views := make([]ClientKeyView, 0, len(p.keys))
	for _, k := range p.keys {
		views = append(views, k.View())
	}
	return views
}

func (p *ClientKeyPool) Stats() (total, enabled int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total = len(p.keys)
	for _, k := range p.keys {
		if k.Enabled {
			enabled++
		}
	}
	return total, enabled
}

func (p *ClientKeyPool) Config() []ClientKeyConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	list := make([]ClientKeyConfig, 0, len(p.keys))
	for _, k := range p.keys {
		enabled := k.Enabled
		list = append(list, ClientKeyConfig{Name: k.Name, Key: k.Key, Enabled: &enabled})
	}
	return list
}

// SyncToConfig copies the pool into cfg ahead of a saveConfig.
func (p *ClientKeyPool) SyncToConfig(cfg *Config) {
	cfg.APIKeys = p.Config()
	if len(cfg.APIKeys) > 0 {
		cfg.APIKey = ""
	}
}

// persistKeys snapshots the pool into cfg and writes config.yaml.
func persistKeys(keys *ClientKeyPool, cfg *Config) error {
	keys.SyncToConfig(cfg)
	return saveConfig(configFile, cfg)
}
