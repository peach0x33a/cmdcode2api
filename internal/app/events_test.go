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
			name:     "extra closing brace",
			input:    `{"edits":[{"oldText":"a","newText":"b"}],"path":"file.go"}}`,
			expected: `{"edits":[{"oldText":"a","newText":"b"}],"path":"file.go"}`,
			ok:       true,
		},
		{
			name:     "extra closing bracket",
			input:    `{"command":"echo ok"}]`,
			expected: `{"command":"echo ok"}`,
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

func TestRepairOrFallbackEditInputRemovesExtraCloser(t *testing.T) {
	raw := `{"edits":[{"oldText":"old","newText":"new"}],"path":"/tmp/file.go"}}`
	assertRepairedEditInput(t, raw)
}

func TestRepairOrFallbackUnwrapsJSONStringifiedEditInput(t *testing.T) {
	inner := `{"edits":[{"oldText":"old","newText":"new"}],"path":"/tmp/file.go"}`
	encoded, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	assertRepairedEditInput(t, string(encoded))
}

func TestRepairOrFallbackUnwrapsSingleObjectArray(t *testing.T) {
	raw := `[{"edits":[{"oldText":"old","newText":"new"}],"path":"/tmp/file.go"}]`
	assertRepairedEditInput(t, raw)
}

func TestRepairOrFallbackEscapesBareControlCharacters(t *testing.T) {
	raw := "{\"edits\":[{\"oldText\":\"old\",\"newText\":\"line 1\nline 2\"}],\"path\":\"/tmp/file.go\"}"
	res := repairOrFallbackToolInput(raw, "edit")
	var got map[string]any
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("repairOrFallbackToolInput returned invalid JSON: %v", err)
	}
	edits, ok := got["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("edits = %#v", got["edits"])
	}
	edit, ok := edits[0].(map[string]any)
	if !ok || edit["newText"] != "line 1\nline 2" {
		t.Fatalf("edit = %#v", edits[0])
	}
}

func assertRepairedEditInput(t *testing.T, raw string) {
	t.Helper()
	res := repairOrFallbackToolInput(raw, "edit")

	var got struct {
		Path  string `json:"path"`
		Edits []struct {
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		} `json:"edits"`
	}
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("repairOrFallbackToolInput returned invalid JSON: %v", err)
	}
	if got.Path != "/tmp/file.go" || len(got.Edits) != 1 || got.Edits[0].OldText != "old" || got.Edits[0].NewText != "new" {
		t.Fatalf("repaired edit input = %#v (raw: %q, repaired: %s)", got, raw, res)
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
