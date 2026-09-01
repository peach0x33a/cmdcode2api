package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// No event may be written after [DONE]: an OpenAI client stops reading there.
func TestNoEmissionAfterDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: {"type":"tool-call","toolCallId":"late","toolName":"bash","input":{"command":"ls"}}`,
		`data: {"type":"text-delta","text":"trailing"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"m", &UsageTracker{}, &Config{})

	out := rec.Body.String()
	done := strings.Index(out, "data: [DONE]")
	if done < 0 {
		t.Fatal("stream did not terminate with [DONE]")
	}
	if after := out[done+len("data: [DONE]"):]; strings.Contains(after, `"late"`) ||
		strings.Contains(after, "trailing") {
		t.Fatalf("content written after [DONE]: %q", after)
	}
}

// A complete provisional sequence still has no execution authority without a
// final tool-call event.
func TestFinishRejectsCompletedProvisionalInput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls\"}"}`,
		`data: {"type":"tool-input-end","id":"c1"}`,
		`data: {"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"m", &UsageTracker{}, &Config{})

	out := rec.Body.String()
	payloads := decodeStreamPayloads(t, out)
	if calls := streamToolCalls(t, payloads); len(calls) != 0 || !hasStreamError(payloads) {
		t.Fatalf("completed provisional input became executable: %s", out)
	}
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d [DONE] markers, want 1", n)
	}
}

// Non-data SSE field lines must be skipped, not decoded — decoding one would
// abort the whole stream and lose every later event.
func TestSSEFieldLinesSkipped(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"a"}`,
		`id: 42`,
		`retry: 1000`,
		`event: message`,
		`: keepalive comment`,
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
	}, "\n\n")
	var kinds []string
	err := ParseStreamEvents(&http.Response{Body: io.NopCloser(strings.NewReader(body))},
		func(ev CCStreamEvent) error { kinds = append(kinds, ev.Type); return nil })
	if err != nil {
		t.Fatalf("a non-data field line aborted the stream: %v (events=%v)", err, kinds)
	}
	if strings.Join(kinds, ",") != "text-delta,tool-call,finish" {
		t.Fatalf("events lost around field lines: %v", kinds)
	}
}
