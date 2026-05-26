package app

import "testing"

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
