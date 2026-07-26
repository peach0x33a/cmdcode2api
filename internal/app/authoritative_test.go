package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reproduces the shape captured in log.txt for call_00_QGEcKdSRrV2LHPV5FeUb8382:
// the buffered deltas were cut off, and the upstream then sent a tool-call event
// holding the complete input. The client used to receive the truncated
// reconstruction (14600 bytes) instead of the authoritative payload (42985).
func TestAuthoritativeToolCallSupersedesRepairedGuess(t *testing.T) {
	complete := strings.Repeat("A", 4096)
	n := newCCEventNormalizer()
	var deduper toolCallDeduper

	consume := func(ev CCStreamEvent) {
		t.Helper()
		events, err := n.Consume(ev)
		if err != nil {
			t.Fatalf("consume %s: %v", ev.Type, err)
		}
		for _, e := range events {
			if e.kind == normalizedToolCall {
				deduper.Add(*e.toolCall)
			}
		}
	}

	consume(CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "write"})
	consume(CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"path":"/a.py","content":"trunc`})
	consume(CCStreamEvent{Type: "tool-input-end", ID: "c1"})
	consume(CCStreamEvent{Type: "tool-call", ToolCallID: "c1", ToolName: "write",
		Input: map[string]any{"path": "/a.py", "content": complete}})
	consume(CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})

	if len(deduper.kept) != 1 {
		t.Fatalf("expected exactly 1 call, got %d: %+v", len(deduper.kept), deduper.kept)
	}
	var args struct{ Content string }
	if err := json.Unmarshal([]byte(deduper.kept[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args.Content != complete {
		t.Fatalf("client got %d bytes of content, want the authoritative %d",
			len(args.Content), len(complete))
	}
}

// With no authoritative event, the repaired guess must still be delivered —
// holding it back must not become a new way to lose a call.
func TestRepairedCallStillDeliveredWithoutAuthoritative(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls`})
	if events := mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"}); len(events) != 0 {
		t.Fatalf("repaired call should be held, got %v", events)
	}

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	call := onlyToolCall(t, events)
	if call.ID != "c1" || call.Function.Name != "bash" {
		t.Fatalf("held call not released correctly: %+v", call)
	}
	if last := events[len(events)-1]; last.kind != normalizedFinish {
		t.Fatal("finish must remain last")
	}
}

// An aborted stream must also release held calls.
func TestHeldCallReleasedOnAbort(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"})

	drained, err := n.drainToolInputs()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(drained) != 1 || drained[0].toolCall == nil || drained[0].toolCall.ID != "c1" {
		t.Fatalf("abort lost the held call: %+v", drained)
	}
}

// A tool-error carrying the payload as a pre-encoded JSON string must not be
// double-encoded: the client has to be able to decode arguments as an object.
func TestToolErrorStringInputNotDoubleEncoded(t *testing.T) {
	n := newCCEventNormalizer()
	events := mustConsume(t, n, CCStreamEvent{
		Type: "tool-error", ToolCallID: "c1", ToolName: "write",
		Input: `{"path":"/a.py","content":"x"}`,
	})
	call := onlyToolCall(t, events)
	var obj map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &obj); err != nil {
		t.Fatalf("arguments are double-encoded, client cannot use them: %v (got %s)",
			err, call.Function.Arguments)
	}
	if obj["path"] != "/a.py" {
		t.Fatalf("wrong arguments: %s", call.Function.Arguments)
	}
}

// The non-streaming path must recover buffered calls on an aborted upstream
// too, and must not discard a partial turn that already holds complete calls.
func TestNonStreamRecoversOnAbort(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls -la\"}"}`,
		``, // upstream drops the connection: no tool-input-end, no finish
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200. body = %s", rec.Code, rec.Body.String())
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("aborted non-stream lost the buffered call: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "ls -la") {
		t.Fatalf("wrong arguments: %s", calls[0].Function.Arguments)
	}
}

// A malformed line after real content must not throw the whole turn away.
func TestNonStreamKeepsCallsDespiteLateParseError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {oops not json}`,
		``,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (a complete call had already arrived)", rec.Code)
	}
	var out ChatResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("call discarded by the late parse error: %+v", out.Choices[0].Message.ToolCalls)
	}
	if got := out.Choices[0].FinishReason; got != "length" {
		t.Errorf("finish_reason = %q, want \"length\" for a truncated turn", got)
	}
}

// A repaired guess that an authoritative event superseded is not a truncation
// the client needs to hear about: the delivered arguments are complete.
func TestSupersededRepairDoesNotReportLength(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"path":"/a","content":"trunc`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-call", ToolCallID: "c1", ToolName: "write",
		Input: map[string]any{"path": "/a", "content": "complete"}})

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	finish := events[len(events)-1]
	if finish.truncated {
		t.Error("superseded repair still flagged the turn as truncated")
	}
	if got := resolveFinishReason(finish.finishReason, true, finish.truncated); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want \"tool_calls\"", got)
	}
}

// But a repair that IS delivered must still report length.
func TestDeliveredRepairReportsLength(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"path":"/a","content":"trunc`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"})

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	finish := events[len(events)-1]
	if !finish.truncated {
		t.Fatal("delivered repair did not flag truncation")
	}
	if got := resolveFinishReason(finish.finishReason, true, finish.truncated); got != "length" {
		t.Errorf("finish_reason = %q, want \"length\"", got)
	}
}
