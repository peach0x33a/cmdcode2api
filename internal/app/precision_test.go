package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The repair path must not round-trip numbers through float64: a 19-digit id
// like a Discord snowflake would be silently rounded and the call would act on
// the wrong resource.
func TestRepairPreservesLargeInteger(t *testing.T) {
	// truncated JSON forces the repair path
	got := repairOrFallbackToolInput(`{"channel_id":12345678901234567890,"content":"hi"`, "discord_send")
	if !strings.Contains(got, "12345678901234567890") {
		t.Fatalf("large integer corrupted by repair: %s", got)
	}
}

// A complete object whose only defect is a bare newline in a string also goes
// through repair, and must not corrupt an untouched large id.
func TestNewlineTriggeredRepairKeepsIDs(t *testing.T) {
	n := newCCEventNormalizer()
	n.Consume(CCStreamEvent{Type: "tool-input-start", ID: "c1", ToolName: "commit"})
	n.Consume(CCStreamEvent{Type: "tool-input-delta", ID: "c1",
		Delta: "{\"repo_id\":12345678901234567890,\"body\":\"line1\nline2\"}"})
	evs, _ := n.Consume(CCStreamEvent{Type: "tool-input-end", ID: "c1"})
	fin, _ := n.Consume(CCStreamEvent{Type: "finish", FinishReason: "tool-calls"})

	var args string
	for _, e := range append(evs, fin...) {
		if e.kind == normalizedToolCall {
			args = e.toolCall.Function.Arguments
		}
	}
	if !strings.Contains(args, "12345678901234567890") {
		t.Fatalf("repo_id corrupted: %s", args)
	}
}

// Assistant tool_calls replayed into history must keep integer precision and
// must not be HTML-escaped: the model reads this text back.
func TestHistoryToolCallsPreserveNumbersAndAngles(t *testing.T) {
	msg := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "call_1", Type: "function",
		Function: CallFunc{Name: "send", Arguments: `{"channel_id":12345678901234567890,"q":"a < b && c"}`},
	}}}
	parts, err := contentToCC(msg)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, p := range parts {
		text += p.Text
	}
	if !strings.Contains(text, "12345678901234567890") {
		t.Errorf("history integer corrupted: %s", text)
	}
	// The old marshal round-trip HTML-escaped < and & into \\u003c / \\u0026 in
	// the very text the model reads back. Verbatim pass-through keeps them literal,
	// so the escaped sequences must be absent and the literal text present.
	if strings.Contains(text, "\\u003c") || strings.Contains(text, "\\u0026") {
		t.Errorf("history HTML-escaped, the model will read mangled text: %s", text)
	}
	if !strings.Contains(text, "a < b && c") {
		t.Errorf("history angle/amp text not preserved verbatim: %s", text)
	}
}

// Empty content tool_calls render as "arguments: {}" not "arguments: null".
func TestEmptyArgumentsRenderAsObject(t *testing.T) {
	msg := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "c1", Type: "function", Function: CallFunc{Name: "now", Arguments: ""},
	}}}
	parts, _ := contentToCC(msg)
	if !strings.Contains(parts[0].Text, "arguments: {}") {
		t.Fatalf("empty arguments rendered wrong: %s", parts[0].Text)
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

// A mid-stream error must not discard a tool call the model embedded in its
// reasoning; the non-stream path parses reasoning before deciding to 502.
func TestNonStreamKeepsReasoningCallOnError(t *testing.T) {
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

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (a reasoning-embedded call survived)", rec.Code)
	}
	var out ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("reasoning-embedded call lost: %+v", out.Choices[0].Message.ToolCalls)
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
