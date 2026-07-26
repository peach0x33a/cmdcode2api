package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A non-final scannable segment's incomplete tail must not be discarded when a
// later segment overwrites the carried-forward remainder.
func TestNonFinalSegmentTailPreserved(t *testing.T) {
	rejected := "<｜｜DSML｜｜tool_calls><invoke name=\"read\">" +
		"<parameter name=\"path\" string=\"TRUE\">x</parameter></invoke></｜｜DSML｜｜tool_calls>"
	p := NewToolCallParser()
	content, calls := p.Feed(
		`Assistant requested tool bash (id1) with arguments: {"command":"ls"} `+
			`Assistant requested tool bash (id2) with arguments: {"comm`+rejected, true)

	var ids []string
	for _, c := range calls {
		ids = append(ids, c.ID)
		if c.Function.Name == "read" {
			t.Errorf("rejected envelope became executable: %+v", c)
		}
	}
	if !containsID(ids, "id1") {
		t.Fatalf("complete call id1 was lost; calls=%+v", calls)
	}
	if !strings.Contains(content, "id2") && !strings.Contains(content, `{"comm`) {
		t.Errorf("incomplete tail of a non-final segment was dropped; content=%q", content)
	}
}

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

// The normal finish flush still delivers a call buffered right up to finish —
// the done guard must not suppress the final drain.
func TestFinishFlushStillEmits(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls\"}"}`,
		`data: {"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"m", &UsageTracker{}, &Config{})

	out := rec.Body.String()
	if !strings.Contains(out, `"name":"bash"`) {
		t.Fatalf("finish flush dropped the buffered call: %s", out)
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

// The write fallback must never fabricate content:"" — executing that would
// truncate the target file to zero bytes.
func TestWriteFallbackNeverFabricatesEmptyContent(t *testing.T) {
	got := repairOrFallbackToolInput(`totally not json`, "write")
	if strings.Contains(got, `"content"`) {
		t.Fatalf("write fallback fabricated a content field: %s", got)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
