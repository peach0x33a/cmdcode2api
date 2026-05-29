package app

import "testing"

func TestIsModelExcludedPrefixMatch(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"gpt-4", []string{"gpt-"}, true},
		{"claude-3-sonnet", []string{"claude-"}, true},
		{"gemini-pro", []string{"gpt-"}, false},
		{"deepseek-chat", []string{"gpt-", "claude-", "gemini-"}, false},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}

func TestIsModelExcludedProviderQualified(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"openai/gpt-4", []string{"gpt-"}, true},
		{"anthropic/claude-3", []string{"claude-"}, true},
		{"google/gemini-1.5-pro", []string{"gemini-"}, true},
		{"deepseek/deepseek-chat", []string{"gpt-"}, false},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}

func TestIsModelExcludedEmptyList(t *testing.T) {
	if isModelExcluded("gpt-4", nil) {
		t.Fatal("expected false for nil excludes")
	}
	if isModelExcluded("gpt-4", []string{}) {
		t.Fatal("expected false for empty excludes")
	}
}

func TestIsModelExcludedWhitespaceEntry(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"gpt-4", []string{"  ", "gpt-"}, true},
		{"gpt-4", []string{"", "gpt-"}, true},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}
