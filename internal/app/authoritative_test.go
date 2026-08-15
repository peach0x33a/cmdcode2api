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

// A provisional call is retained internally but never released without a
// validated finish event.
func TestBufferedCallNotReleasedOnAbort(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c1"})

	drained, err := n.drainToolInputs()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(drained) != 0 || len(n.toolCalls.kept) != 1 {
		t.Fatalf("provisional call escaped before finish: drained=%+v kept=%+v", drained, n.toolCalls.kept)
	}
}

// A tool-error carrying the payload as a pre-encoded JSON string must not be
// double-encoded: the client has to be able to decode arguments as an object.
func TestToolErrorStringInputNotDoubleEncoded(t *testing.T) {
	n := newCCEventNormalizer()
	if events := mustConsume(t, n, CCStreamEvent{
		Type: "tool-error", ToolCallID: "c1", ToolName: "write",
		Input: `{"path":"/a.py","content":"x"}`,
	}); len(events) != 0 {
		t.Fatalf("tool call emitted before finish: %+v", events)
	}
	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
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

// The non-streaming path must reject an aborted upstream instead of returning
// a successful partial turn.
func TestNonStreamRejectsAbort(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls -la\"}"}`,
		``, // upstream drops the connection: no tool-input-end, no finish
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_incomplete") {
		t.Fatalf("status = %d, want 502 incomplete-stream error. body = %s", rec.Code, rec.Body.String())
	}
}

// A malformed line after real content still makes the whole non-streaming
// response an error; partial tool calls must not be presented as completed.
func TestNonStreamRejectsLateParseError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {oops not json}`,
		``,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_error") {
		t.Fatalf("status = %d, want 502 stream error. body = %s", rec.Code, rec.Body.String())
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
