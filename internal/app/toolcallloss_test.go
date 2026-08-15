package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// A stream that ends while a tool input is still buffered must still deliver
// the call. Upstream does not always send tool-input-end before finish.
func TestFinishDrainsBufferedToolInput(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls -la"}`})

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})

	call := onlyToolCall(t, events)
	if call.ID != "c1" || call.Function.Name != "bash" {
		t.Fatalf("recovered wrong call: %+v", call)
	}
	if call.Function.Arguments != `{"command":"ls -la"}` {
		t.Fatalf("arguments mangled: %s", call.Function.Arguments)
	}
	if last := events[len(events)-1]; last.kind != normalizedFinish {
		t.Fatalf("finish must remain the last event, got kind %v", last.kind)
	}
}

// Buffered inputs drain in the order the model produced them, not in Go map order.
func TestFinishDrainsInArrivalOrder(t *testing.T) {
	n := newCCEventNormalizer()
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: id, ToolName: "bash"})
		mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: id, Delta: `{"command":"` + id + `"}`})
	}

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})

	var got []string
	for _, ev := range events {
		if ev.kind == normalizedToolCall {
			got = append(got, ev.toolCall.ID)
		}
	}
	if strings.Join(got, "") != "abcdefgh" {
		t.Fatalf("drain order = %v, want a..h", got)
	}
}

// tool-error can arrive in place of tool-input-end; it must not discard the call.
func TestToolErrorTerminatesToolInput(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c2", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c2", Delta: `{"path":"/a","content":"x"}`})

	if events := mustConsume(t, n, CCStreamEvent{Type: "tool-error", ToolCallID: "c2", ToolName: "write"}); len(events) != 0 {
		t.Fatalf("tool call emitted before finish: %+v", events)
	}
	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	call := onlyToolCall(t, events)
	if call.ID != "c2" || call.Function.Arguments != `{"path":"/a","content":"x"}` {
		t.Fatalf("tool-error lost the call: %+v", call)
	}
}

// The observed production ordering is tool-input-end, tool-call, tool-error for
// the same id. Only one call may reach the client.
func TestToolErrorAfterEndDoesNotDuplicate(t *testing.T) {
	n := newCCEventNormalizer()
	var deduper toolCallDeduper

	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c3", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c3", Delta: `{"command":"echo hi"}`})
	for _, ev := range []CCStreamEvent{
		{Type: "tool-input-end", ID: "c3"},
		{Type: "tool-call", ToolCallID: "c3", ToolName: "bash", Input: map[string]any{"command": "echo hi"}},
		{Type: "tool-error", ToolCallID: "c3", ToolName: "bash", Input: `{"command":"echo hi"}`},
		{Type: "finish", FinishReason: "tool-calls"},
	} {
		for _, out := range mustConsume(t, n, ev) {
			if out.kind == normalizedToolCall {
				deduper.Add(*out.toolCall)
			}
		}
	}

	if len(deduper.kept) != 1 {
		t.Fatalf("expected 1 call after dedup, got %d: %+v", len(deduper.kept), deduper.kept)
	}
}

// An upstream cut off by the output-token cap leaves truncated arguments that
// repairOrFallbackToolInput patches into valid JSON. The client must be told
// "length", never "tool_calls".
func TestTruncatedToolInputReportsLength(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c4", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c4", Delta: `{"path":"/a.py","content":"line1`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: "c4"})

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "stop"})
	finish := events[len(events)-1]
	if !finish.truncated {
		t.Fatal("normalizer did not flag the repaired input as truncated")
	}
	if got := resolveFinishReason(finish.finishReason, true, finish.truncated); got != "length" {
		t.Fatalf("finish_reason = %q, want \"length\"", got)
	}
}

func TestResolveFinishReason(t *testing.T) {
	cases := []struct {
		upstream            string
		hasToolCalls, trunc bool
		want                string
	}{
		{"tool_calls", true, false, "tool_calls"},
		{"stop", true, false, "tool_calls"},
		{"stop", false, false, "stop"},
		{"length", true, false, "tool_calls"}, // authoritative call is complete
		{"stop", true, true, "length"},        // arguments needed repair
		{"length", false, false, "length"},    // plain text truncation
	}
	for _, c := range cases {
		if got := resolveFinishReason(c.upstream, c.hasToolCalls, c.trunc); got != c.want {
			t.Errorf("resolveFinishReason(%q,%v,%v) = %q, want %q",
				c.upstream, c.hasToolCalls, c.trunc, got, c.want)
		}
	}
}

// A text-form tool call larger than the old 128 KB cap must still be recovered.
func TestLargeTextToolCallSurvivesBuffering(t *testing.T) {
	body := strings.Repeat("x", 400*1024)
	p := NewToolCallParser()

	content, calls := p.Feed(`prose before. Assistant requested tool write (call_x) with arguments: {"path":"/a","content":"`+body, false)
	if len(calls) != 0 {
		t.Fatalf("emitted a call before its JSON completed: %+v", calls)
	}
	if content != "prose before. " {
		t.Fatalf("leading prose = %q, want %q", content, "prose before. ")
	}

	content, calls = p.Feed(`"}`, true)
	if len(calls) != 1 {
		t.Fatalf("large tool call was lost: %d calls, %d bytes flushed as text", len(calls), len(content))
	}
	var args struct{ Content string }
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if len(args.Content) != len(body) {
		t.Fatalf("content truncated: got %d bytes, want %d", len(args.Content), len(body))
	}
}

// Text preceding an oversized unterminated call must not be held hostage by it,
// and the buffer must not grow past the cap.
func TestOversizedUnterminatedCallDegradesToText(t *testing.T) {
	p := NewToolCallParser()
	p.maxSize = 4096

	content, calls := p.Feed("visible prose. "+`Assistant requested tool write (c) with arguments: {"content":"`+strings.Repeat("y", 8192), false)
	if len(calls) != 0 {
		t.Fatalf("unexpected calls: %+v", calls)
	}
	if !strings.HasPrefix(content, "visible prose. ") {
		t.Fatalf("preceding prose was withheld; content starts %q", content[:min(40, len(content))])
	}
	if p.buf.Len() > p.maxSize {
		t.Fatalf("buffer grew past the cap: %d > %d", p.buf.Len(), p.maxSize)
	}
}

func mustConsume(t *testing.T, n *ccEventNormalizer, ev CCStreamEvent) []normalizedCCEvent {
	t.Helper()
	events, err := n.Consume(ev)
	if err != nil {
		t.Fatalf("consume %s: %v", ev.Type, err)
	}
	return events
}

func onlyToolCall(t *testing.T, events []normalizedCCEvent) ToolCall {
	t.Helper()
	var found []ToolCall
	for _, ev := range events {
		if ev.kind == normalizedToolCall && ev.toolCall != nil {
			found = append(found, *ev.toolCall)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 tool call, got %d", len(found))
	}
	return found[0]
}
