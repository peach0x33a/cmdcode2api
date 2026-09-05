package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIToCCExtractsSystemAndContent(t *testing.T) {
	req := &ChatRequest{
		Model: "deepseek/deepseek-v4-flash",
		Messages: []Message{
			{Role: "system", Content: TextContent("be terse")},
			{Role: "user", Content: TextContent("hello")},
		},
	}

	got, err := openAIToCC(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
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

func TestOpenAIToCCUsesCommandCodeDefaultMaxTokens(t *testing.T) {
	req := &ChatRequest{
		Model:    "deepseek/deepseek-v4-pro",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	}

	got, err := openAIToCC(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.Params.MaxTokens != defaultCCMaxTokens {
		t.Fatalf("max_tokens = %d, want command-code default %d", got.Params.MaxTokens, defaultCCMaxTokens)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"max_tokens":64000`) {
		t.Fatalf("default max_tokens missing from CC request: %s", encoded)
	}
}

func TestOpenAIToCCPreservesAndCapsExplicitMaxTokens(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input int
		want  int
	}{
		{name: "preserved", input: 32768, want: 32768},
		{name: "capped", input: 300000, want: maximumCCMaxTokens},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := openAIToCC(&ChatRequest{
				Model:     "deepseek/deepseek-v4-pro",
				Messages:  []Message{{Role: "user", Content: TextContent("hello")}},
				MaxTokens: tt.input,
			})
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if got.Params.MaxTokens != tt.want {
				t.Fatalf("max_tokens = %d, want %d", got.Params.MaxTokens, tt.want)
			}
		})
	}
}

func TestMessagesToCCUsesStructuredToolHistory(t *testing.T) {
	got, err := messagesToCC([]Message{
		{
			Role:             "assistant",
			Content:          TextContent("checking"),
			ReasoningContent: "need lookup",
			ToolCalls: []ToolCall{{
				ID: "call-1", Type: "function",
				Function: CallFunc{Name: "lookup", Arguments: `{"query":"kimi"}`},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call-1",
			Content:    TextContent("tool output"),
		},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	if len(got) != 2 || got[0].Role != "assistant" || got[1].Role != "tool" {
		t.Fatalf("messages = %#v", got)
	}
	assistant := got[0].Content
	if len(assistant) != 3 || assistant[0].Type != "reasoning" || assistant[1].Type != "text" || assistant[2].Type != "tool-call" {
		t.Fatalf("assistant content = %#v", assistant)
	}
	if assistant[2].ToolCallID != "call-1" || assistant[2].ToolName != "lookup" || string(assistant[2].Input) != `{"query":"kimi"}` {
		t.Fatalf("tool call = %#v", assistant[2])
	}
	result := got[1].Content
	if len(result) != 1 || result[0].Type != "tool-result" || result[0].ToolName != "lookup" || result[0].Output == nil || result[0].Output.Value != "tool output" {
		t.Fatalf("tool result = %#v", result)
	}
}

func TestMarshaledCCRequestUsesExactStructuredHistory(t *testing.T) {
	converted, err := openAIToCC(&ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "system", Content: TextContent("system")},
			{
				Role:             "assistant",
				Content:          TextContent("calling"),
				ReasoningContent: "think",
				ToolCalls: []ToolCall{{
					ID: "call-1", Type: "function",
					Function: CallFunc{Name: "lookup", Arguments: `{"id":12345678901234567890}`},
				}},
			},
			{Role: "tool", ToolCallID: "call-1", Content: TextContent("ok")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	converted.Config = CCConfig{
		WorkingDir:    "/",
		Date:          "2026-01-02",
		Environment:   "test",
		Structure:     []string{},
		RecentCommits: []any{},
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"config":{"workingDir":"/","date":"2026-01-02","environment":"test","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},"memory":"","taste":"","skills":null,"permissionMode":"standard","params":{"model":"m","messages":[{"role":"assistant","content":[{"type":"reasoning","text":"think"},{"type":"text","text":"calling"},{"type":"tool-call","toolCallId":"call-1","toolName":"lookup","input":{"id":12345678901234567890}}]},{"role":"tool","content":[{"type":"tool-result","toolCallId":"call-1","toolName":"lookup","output":{"type":"text","value":"ok"}}]}],"tools":[],"system":"system","max_tokens":64000,"stream":true}}`
	if string(encoded) != want {
		t.Fatalf("CCRequest wire JSON changed:\n got: %s\nwant: %s", encoded, want)
	}
}

func TestAssistantToolCallUsesStructuredHistoryPart(t *testing.T) {
	got, err := contentToCC(Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: CallFunc{
				Name:      "lookup",
				Arguments: `{"query":"kimi"}`,
			},
		}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	if len(got) != 1 || got[0].Type != "tool-call" {
		t.Fatalf("parts = %#v", got)
	}
	if got[0].ToolName != "lookup" || string(got[0].Input) != `{"query":"kimi"}` {
		t.Fatalf("tool call = %#v", got[0])
	}
}

func TestParseDataURL(t *testing.T) {
	mediaType, data, err := parseDataURL("data:image/jpeg;base64,YWJjMTIz")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mediaType != "image/jpeg" || data != "YWJjMTIz" {
		t.Fatalf("got %q %q", mediaType, data)
	}

	if _, _, err := parseDataURL("https://example.com/image.png"); err == nil {
		t.Fatal("expected remote image URL to be rejected")
	}
}

func TestContentToCCRejectsInvalidToolArguments(t *testing.T) {
	_, err := contentToCC(Message{
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
	var invalid *invalidRequestError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("error = %v, want invalidRequestError", err)
	}
}

func TestContentToCCRejectsNonObjectToolArguments(t *testing.T) {
	for _, arguments := range []string{`[]`, `null`, `"text"`, `42`, `{"ok":true} trailing`} {
		_, err := contentToCC(Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1", Function: CallFunc{Name: "bad_tool", Arguments: arguments},
		}}})
		if err == nil {
			t.Fatalf("arguments %q unexpectedly accepted", arguments)
		}
	}
}

func TestSystemContentPartsArePreserved(t *testing.T) {
	got := extractSystem([]Message{
		{
			Role: "system",
			Content: PartsContent(
				ContentPart{Type: "text", Text: "first"},
				ContentPart{Type: "text", Text: " second"},
			),
		},
		{Role: "developer", Content: TextContent("developer rule")},
	})
	if got != "first second\ndeveloper rule" {
		t.Fatalf("system = %q", got)
	}
}

func TestToolResultPreservesAllTextParts(t *testing.T) {
	parts, err := contentToCC(Message{
		Role:       "tool",
		Name:       "lookup",
		ToolCallID: "call-1",
		Content: PartsContent(
			ContentPart{Type: "text", Text: "first"},
			ContentPart{Type: "text", Text: " second"},
		),
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-result" || parts[0].Output == nil || parts[0].Output.Value != "first second" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestContentToCCRejectsRemoteImageURL(t *testing.T) {
	_, err := contentToCC(Message{
		Role: "user",
		Content: PartsContent(ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: "https://example.com/image.png"},
		}),
	})
	var invalid *invalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want invalidRequestError", err)
	}
}

func TestCCClientSendUsesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewCCClient("test-key", "http://127.0.0.1:1")
	_, _, err := client.Send(ctx, &ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestCCClientSendParsesTopLevelRateLimitError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("x-request-id", "req_123")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You've reached your 5-hour usage limit for your plan.","type":"server_error"}`)
	}))
	defer upstream.Close()

	client := NewCCClient("test-key", upstream.URL)
	_, _, err := client.Send(context.Background(), &ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})

	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %T %v, want upstreamAPIError", err, err)
	}
	if upstreamErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", upstreamErr.Status)
	}
	if upstreamErr.Type != "rate_limit_error" || upstreamErr.Code != "rate_limit_exceeded" {
		t.Fatalf("normalized error = %#v", upstreamErr)
	}
	if upstreamErr.Message != "You've reached your 5-hour usage limit for your plan." {
		t.Fatalf("message = %q", upstreamErr.Message)
	}
	if upstreamErr.RetryAfter != "120" || upstreamErr.RequestID != "req_123" {
		t.Fatalf("headers = %#v", upstreamErr)
	}
}

func TestRetryAfterFromRateLimitMessage(t *testing.T) {
	now := time.Date(2026, time.July, 21, 23, 55, 32, 351_000_000, time.UTC)
	got := retryAfterFromRateLimit(0, "Your limit resets at 2026-07-21T23:57:32.351Z. Please wait.", now)
	if got != "120" {
		t.Fatalf("Retry-After = %q, want 120", got)
	}
}

func TestCCClientSendPreservesNestedUpstreamErrorCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"RATE_LIMITED","message":"window exhausted","type":"server_error"}}`)
	}))
	defer upstream.Close()

	client := NewCCClient("test-key", upstream.URL)
	_, _, err := client.Send(context.Background(), &ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	})

	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %T %v, want upstreamAPIError", err, err)
	}
	if upstreamErr.Code != "RATE_LIMITED" || upstreamErr.Type != "rate_limit_error" || upstreamErr.Message != "window exhausted" {
		t.Fatalf("normalized error = %#v", upstreamErr)
	}
}

func TestParseStreamEventsRejectsMalformedData(t *testing.T) {
	tests := []string{
		"data: not-json\n\n",
		`data: {"type":"tool-call","toolCallId":"call_bad","toolName":"bash","input":{"command":"echo unsafe"}} garbage` + "\n\n",
		`data: {"type":"tool-call","toolCallId":"call_bad","toolName":"bash","input":{"command":"echo unsafe"}} {"type":"finish"}` + "\n\n",
	}
	for _, input := range tests {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(input))}
		called := false
		err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("input %q: err = %v, callback called = %v; want rejection before callback", input, err, called)
		}
	}
}
