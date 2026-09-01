package app

import "testing"

func TestFinishDropsBufferedInputWithoutEnd(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "bash"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c1", Delta: `{"command":"ls -la"}`})

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	for _, event := range events {
		if event.kind == normalizedToolCall {
			t.Fatalf("unterminated provisional call escaped: %+v", event.toolCall)
		}
	}
}

func TestCompletedProvisionalInputsAreDropped(t *testing.T) {
	n := newCCEventNormalizer()
	for _, id := range []string{"b", "a", "c"} {
		mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: id, ToolName: "bash"})
		mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: id, Delta: `{"command":"` + id + `"}`})
		mustConsume(t, n, CCStreamEvent{Type: "tool-input-end", ID: id})
	}

	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	for _, event := range events {
		if event.kind == normalizedToolCall {
			t.Fatalf("completed provisional call escaped: %+v", event.toolCall)
		}
	}
	if len(n.toolInputBuf) != 0 || len(n.toolInputOrder) != 0 {
		t.Fatalf("provisional state survived finish: buf=%v order=%v", n.toolInputBuf, n.toolInputOrder)
	}
}

func TestToolErrorDiscardsProvisionalInput(t *testing.T) {
	n := newCCEventNormalizer()
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-start", ID: "c2", ToolName: "write"})
	mustConsume(t, n, CCStreamEvent{Type: "tool-input-delta", ID: "c2", Delta: `{"path":"/a","content":"x"}`})
	mustConsume(t, n, CCStreamEvent{Type: "tool-error", ToolCallID: "c2", ToolName: "write"})
	events := mustConsume(t, n, CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})
	for _, event := range events {
		if event.kind == normalizedToolCall {
			t.Fatalf("errored provisional call escaped: %+v", event.toolCall)
		}
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
		{"length", true, true, "length"},
		{"length", false, true, "length"},
	}
	for _, c := range cases {
		if got := resolveFinishReason(c.upstream, c.hasToolCalls, c.trunc); got != c.want {
			t.Errorf("resolveFinishReason(%q,%v,%v) = %q, want %q",
				c.upstream, c.hasToolCalls, c.trunc, got, c.want)
		}
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
