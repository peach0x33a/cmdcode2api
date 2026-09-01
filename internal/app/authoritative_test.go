package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthoritativeToolCallSupersedesProvisional(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"path":"/a.py","content":"draft"}`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-call", ToolCallID: "c1", ToolName: "write",
		Input: map[string]any{"path": "/a.py", "content": "authoritative"}})

	call := onlyToolCall(t, mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"}))
	var args struct{ Content string }
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if args.Content != "authoritative" {
		t.Fatalf("authoritative call did not supersede provisional: %+v", call)
	}
}

func TestMalformedProvisionalCanBeSupersededByAuthoritative(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"path":"/a.py","content":"cut`})
	if events := mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"}); len(events) != 0 {
		t.Fatalf("malformed provisional input emitted: %+v", events)
	}
	mustConsume(t, n, CCStreamEvent{Type: "tool-call", ToolCallID: "c1", ToolName: "write",
		Input: map[string]any{"path": "/a.py", "content": "complete"}})

	call := onlyToolCall(t, mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"}))
	if !strings.Contains(call.Function.Arguments, "complete") {
		t.Fatalf("valid authoritative call was lost: %+v", call)
	}
}

func TestBufferedCallNotReleasedOnAbort(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls"}`})
	if len(n.toolCalls.kept) != 0 {
		t.Fatalf("provisional call escaped before structured end: %+v", n.toolCalls.kept)
	}
}

func TestToolErrorNeverCreatesCall(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-error", ToolCallID: "c1", ToolName: "write",
		Input: `{"path":"/a.py","content":"x"}`})
	for _, event := range mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"}) {
		if event.kind == normalizedToolCall {
			t.Fatalf("tool-error became executable: %+v", event.toolCall)
		}
	}
}

func TestNonStreamRejectsAbort(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls -la\"}"}`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_incomplete") {
		t.Fatalf("status = %d, want 502 incomplete-stream error. body = %s", rec.Code, rec.Body.String())
	}
}

func TestNonStreamRejectsLateParseError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {oops not json}`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_error") {
		t.Fatalf("status = %d, want 502 stream error. body = %s", rec.Code, rec.Body.String())
	}
}
