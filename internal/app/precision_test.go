package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Assistant tool_calls replayed into structured history must keep the original
// JSON number spelling rather than round-tripping through float64.
func TestHistoryToolCallsPreserveNumbersAndAngles(t *testing.T) {
	arguments := `{"channel_id":12345678901234567890,"q":"a < b && c"}`
	msg := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "call_1", Type: "function",
		Function: CallFunc{Name: "send", Arguments: arguments},
	}}}
	parts, err := contentToCC(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-call" || string(parts[0].Input) != arguments {
		t.Fatalf("structured history input changed: %#v", parts)
	}
}

func TestEmptyArgumentsBecomeObject(t *testing.T) {
	msg := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "c1", Type: "function", Function: CallFunc{Name: "now", Arguments: ""},
	}}}
	parts, err := contentToCC(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(parts[0].Input) != "{}" {
		t.Fatalf("empty arguments rendered wrong: %s", parts[0].Input)
	}
}

// A no-argument tool must carry an empty-object schema, not omit it.
func TestNoArgToolGetsEmptySchema(t *testing.T) {
	out := toolsToCC([]Tool{{Type: "function", Function: ToolFunction{Name: "get_time"}}})
	if out[0].InputSchema == nil {
		t.Fatalf("no-arg tool lost its input_schema entirely")
	}
	if out[0].InputSchema["type"] != "object" {
		t.Fatalf("no-arg tool schema = %v, want an empty object schema", out[0].InputSchema)
	}
}

// A mid-stream error invalidates the whole non-streaming response, including
// any provisional tool call embedded in reasoning.
func TestNonStreamRejectsReasoningCallOnError(t *testing.T) {
	dsml := "<｜｜DSML｜｜tool_calls><invoke name=\"bash\">" +
		"<parameter name=\"command\" string=\"true\">rm -rf /tmp/x</parameter></invoke></｜｜DSML｜｜tool_calls>"
	reasoning, _ := json.Marshal(dsml)
	body := strings.Join([]string{
		`data: {"type":"reasoning-delta","delta":` + string(reasoning) + `}`,
		`data: {oops bad frame}`,
	}, "\n\n")
	rec := httptest.NewRecorder()
	handleNonStream(rec, &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		"m", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_error") {
		t.Fatalf("status = %d, want 502 stream error. body = %s", rec.Code, rec.Body.String())
	}
}

// An empty data: keep-alive line must not abort the stream.
func TestEmptyDataLineIsKeepalive(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		`data:`,
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
	}, "\n\n")
	var kinds []string
	err := ParseStreamEvents(&http.Response{Body: io.NopCloser(strings.NewReader(body))},
		func(ev CCStreamEvent) error { kinds = append(kinds, ev.Type); return nil })
	if err != nil {
		t.Fatalf("empty data line aborted the stream: %v (events=%v)", err, kinds)
	}
	if strings.Join(kinds, ",") != "tool-input-start,tool-call,finish" {
		t.Fatalf("events lost around empty data line: %v", kinds)
	}
}
