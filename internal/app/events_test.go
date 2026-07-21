package app

import (
	"encoding/json"
	"testing"
)

func TestTryRepairJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		ok       bool
	}{
		{
			name:     "truncated string in object",
			input:    `{"command": "echo hello`,
			expected: `{"command": "echo hello"}`,
			ok:       true,
		},
		{
			name:     "trailing comma in object",
			input:    `{"command": "echo hello",`,
			expected: `{"command": "echo hello"}`,
			ok:       true,
		},
		{
			name:     "truncated array",
			input:    `{"args": ["a", "b"`,
			expected: `{"args": ["a", "b"]}`,
			ok:       true,
		},
		{
			name:  "raw non-json text",
			input: `location:' "$headers_file"`,
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tryRepairJSON(tt.input)
			if ok != tt.ok {
				t.Errorf("tryRepairJSON() ok = %v, want %v", ok, tt.ok)
			}
			if tt.ok && got != tt.expected {
				t.Errorf("tryRepairJSON() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRepairOrFallbackToolInput(t *testing.T) {
	// Raw text non-JSON should wrap into valid JSON object
	raw := `location:' "$headers_file"`
	res := repairOrFallbackToolInput(raw, "bash")

	var v map[string]any
	if err := json.Unmarshal([]byte(res), &v); err != nil {
		t.Fatalf("repairOrFallbackToolInput returned invalid JSON: %v", err)
	}

	if v["command"] != raw {
		t.Errorf("expected command=%q, got %v", raw, v["command"])
	}
}

func TestMalformedToolInputInStream(t *testing.T) {
	normalizer := newCCEventNormalizer()

	// Simulate streaming deltas with raw unescaped script text (like in log)
	deltas := []string{
		"location",
		":'",
		" \"$",
		"headers",
		"_file\"",
		" | tail -1",
	}

	callID := "call_00_FOoANiXfHMLvEwQp7xSQ6876"
	for _, delta := range deltas {
		events, err := normalizer.Consume(CCStreamEvent{
			Type:     "tool-input-delta",
			ID:       callID,
			ToolName: "bash",
			Delta:    delta,
		})
		if err != nil {
			t.Fatalf("unexpected error on tool-input-delta: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("expected 0 events on delta, got %d", len(events))
		}
	}

	// Send tool-input-end
	events, err := normalizer.Consume(CCStreamEvent{
		Type: "tool-input-end",
		ID:   callID,
	})
	if err != nil {
		t.Fatalf("expected tool-input-end to succeed with repair/fallback, got error: %v", err)
	}
	if len(events) != 1 || events[0].kind != normalizedToolCall {
		t.Fatalf("expected 1 normalizedToolCall event, got %v", events)
	}

	tc := events[0].toolCall
	if tc == nil {
		t.Fatalf("toolCall is nil")
	}
	if tc.ID != callID {
		t.Errorf("expected ID %q, got %q", callID, tc.ID)
	}

	// Verify Arguments is valid JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
		t.Fatalf("expected Arguments to be valid JSON, got error: %v (Arguments: %s)", err, tc.Function.Arguments)
	}
	if parsed["command"] == "" {
		t.Errorf("expected non-empty command field in fallback JSON, got %v", parsed)
	}
}

