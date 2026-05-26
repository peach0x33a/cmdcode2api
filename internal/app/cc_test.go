package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIToCCExtractsSystemAndContent(t *testing.T) {
	req := &ChatRequest{
		Model: "deepseek/deepseek-v4-flash",
		Messages: []Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hello"},
		},
	}

	got := openAIToCC(req)
	if got.Params.Model != req.Model {
		t.Fatalf("model = %q, want %q", got.Params.Model, req.Model)
	}
	if got.Params.System != "be terse" {
		t.Fatalf("system = %q", got.Params.System)
	}
	if len(got.Params.Messages) != 1 {
		t.Fatalf("messages len = %d", len(got.Params.Messages))
	}
	if got.Params.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("text = %q", got.Params.Messages[0].Content[0].Text)
	}
}

func TestMessagesToCCMapsToolRoleToUser(t *testing.T) {
	got := messagesToCC([]Message{
		{
			Role:       "tool",
			ToolCallID: "call-1",
			Name:       "lookup",
			Content:    "tool output",
		},
	})

	if len(got) != 1 {
		t.Fatalf("messages len = %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("role = %q", got[0].Role)
	}
	if len(got[0].Content) != 1 || got[0].Content[0].Type != "tool-result" {
		t.Fatalf("content = %#v", got[0].Content)
	}
	if got[0].Content[0].Output.Value != "tool output" {
		t.Fatalf("tool output = %#v", got[0].Content[0].Output)
	}
}

func TestParseDataURL(t *testing.T) {
	mediaType, data := parseDataURL("data:image/jpeg;base64,abc123")
	if mediaType != "image/jpeg" || data != "abc123" {
		t.Fatalf("got %q %q", mediaType, data)
	}

	mediaType, data = parseDataURL("raw-base64")
	if mediaType != "image/png" || data != "raw-base64" {
		t.Fatalf("fallback got %q %q", mediaType, data)
	}
}

func TestContentToCCDoesNotDropInvalidToolArguments(t *testing.T) {
	parts := contentToCC(Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: CallFunc{
				Name:      "bad_tool",
				Arguments: "{not-json",
			},
		}},
	})

	if len(parts) != 1 {
		t.Fatalf("parts len = %d", len(parts))
	}
	if parts[0].Type != "text" || !strings.Contains(parts[0].Text, "invalid tool arguments") {
		t.Fatalf("unexpected part: %#v", parts[0])
	}
}

func TestParseStreamEventsRejectsMalformedData(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("data: not-json\n\n")),
	}

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		t.Fatal("callback should not be called")
		return nil
	})
	if err == nil {
		t.Fatal("expected parse error")
	}
}
