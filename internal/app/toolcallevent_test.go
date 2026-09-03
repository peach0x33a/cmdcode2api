package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// truncatedWriteInput mirrors the production payload captured in log.txt
// (2026-07-21): the upstream hit its output cap and emitted a tool-call event
// whose inline input string stops mid-string, with no closing quote or brace.
const truncatedWriteInput = `{"path": "ea_login.py", "content": "#!/usr/bin/env python3\nimport subprocess\nCONFIG = {\"api\": \"https://example` + "\n"

func mustJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("arguments are not a JSON object: %v (raw=%q)", err, raw)
	}
	return value
}

func TestToolCallFromEventTruncatedStringInput(t *testing.T) {
	call, ok, err := toolCallFromEvent(CCStreamEvent{
		Type:       "tool-call",
		ToolCallID: "call_trunc_1",
		ToolName:   "write",
		Input:      truncatedWriteInput,
	})
	if err != nil {
		t.Fatalf("truncated input must not error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recovered call")
	}
	if call.repairKind != toolInputRepairTruncated {
		t.Fatalf("repairKind = %d, want toolInputRepairTruncated", call.repairKind)
	}
	if !call.unsafeArguments() {
		t.Fatal("truncated arguments must count as unsafe")
	}
	mustJSON(t, call.Function.Arguments)
	if call.Function.Name != "write" || call.ID != "call_trunc_1" {
		t.Fatalf("unexpected call identity: %+v", call.Function)
	}
}

func TestToolCallFromEventNonObjectStringInput(t *testing.T) {
	call, ok, err := toolCallFromEvent(CCStreamEvent{
		Type:       "tool-call",
		ToolCallID: "call_text_1",
		ToolName:   "bash",
		Input:      "just do it",
	})
	if err != nil {
		t.Fatalf("non-object input must not error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recovered call")
	}
	if call.repairKind != toolInputRepairFallback {
		t.Fatalf("repairKind = %d, want toolInputRepairFallback", call.repairKind)
	}
	args := mustJSON(t, call.Function.Arguments)
	if args["command"] != "just do it" {
		t.Fatalf("fallback arguments missing command: %v", args)
	}
}

func TestToolCallFromEventStringWrappedArrayInput(t *testing.T) {
	call, ok, err := toolCallFromEvent(CCStreamEvent{
		Type:       "tool-call",
		ToolCallID: "call_arr_1",
		ToolName:   "bash",
		Input:      `[{"command": "ls"}]`,
	})
	if err != nil {
		t.Fatalf("string-wrapped array must not error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recovered call")
	}
	args := mustJSON(t, call.Function.Arguments)
	if args["command"] != "ls" {
		t.Fatalf("expected singleton array unwrapped to object: %v", args)
	}
}

func TestToolCallFromEventCleanInputsUnchanged(t *testing.T) {
	stringCall, ok, err := toolCallFromEvent(CCStreamEvent{
		Type: "tool-call", ToolCallID: "call_a", ToolName: "bash",
		Input: `{"command": "ls -la"}`,
	})
	if err != nil || !ok {
		t.Fatalf("clean string input errored: %v", err)
	}
	if stringCall.repairKind != toolInputRepairNone || stringCall.Function.Arguments != `{"command": "ls -la"}` {
		t.Fatalf("clean string input changed: kind=%d args=%q", stringCall.repairKind, stringCall.Function.Arguments)
	}

	objectCall, ok, err := toolCallFromEvent(CCStreamEvent{
		Type: "tool-call", ToolCallID: "call_b", ToolName: "bash",
		Input: map[string]any{"command": "wc -l f.py"},
	})
	if err != nil || !ok {
		t.Fatalf("object input errored: %v", err)
	}
	if objectCall.repairKind != toolInputRepairNone {
		t.Fatalf("object input marked repaired: %d", objectCall.repairKind)
	}
	args := mustJSON(t, objectCall.Function.Arguments)
	if args["command"] != "wc -l f.py" {
		t.Fatalf("object arguments corrupted: %v", args)
	}
}

func TestToolCallFromEventMissingIDSynthesized(t *testing.T) {
	call, ok, err := toolCallFromEvent(CCStreamEvent{
		Type:     "tool-call",
		ToolName: "read",
		Input:    map[string]any{"path": "f.txt"},
	})
	if err != nil {
		t.Fatalf("missing id must not error: %v", err)
	}
	if !ok {
		t.Fatal("expected a recovered call")
	}
	if !strings.HasPrefix(call.ID, "call_recovered_") {
		t.Fatalf("expected synthesized id, got %q", call.ID)
	}
}

func TestToolCallFromEventMissingNameErrors(t *testing.T) {
	_, _, err := toolCallFromEvent(CCStreamEvent{
		Type:       "tool-call",
		ToolCallID: "call_noname",
		Input:      map[string]any{"path": "f.txt"},
	})
	if err == nil {
		t.Fatal("a call without a function name is unrecoverable and must error")
	}
}

func TestNormalizerToolCallEventTruncatedInputNoStreamError(t *testing.T) {
	n := newCCEventNormalizer()
	if _, err := n.Consume(CCStreamEvent{
		Type: "tool-call", ToolCallID: "call_x", ToolName: "write", Input: truncatedWriteInput,
	}); err != nil {
		t.Fatalf("normalizer aborted on truncated tool-call input: %v", err)
	}
	events, err := n.Consume(CCStreamEvent{
		Type:         "finish",
		FinishReason: "tool-calls",
		TotalUsage:   &CCUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	if err != nil {
		t.Fatalf("finish after truncated tool-call errored: %v", err)
	}
	var sawCall, sawFinish bool
	for _, ev := range events {
		switch ev.kind {
		case normalizedToolCall:
			sawCall = true
		case normalizedFinish:
			sawFinish = true
			if !ev.truncated {
				t.Fatal("finish event must report truncation for repaired truncated input")
			}
		}
	}
	if !sawCall || !sawFinish {
		t.Fatalf("expected tool call + finish events, got %+v", events)
	}
}

func TestFinishToolInputInlineTruncatedInput(t *testing.T) {
	n := newCCEventNormalizer()
	// tool-error with only an inline input, nothing buffered: the same path
	// production exercised when the upstream executed the truncated call.
	call, ok, err := n.finishToolInput(CCStreamEvent{
		Type: "tool-error", ToolCallID: "call_err", ToolName: "write", Input: truncatedWriteInput,
	})
	if err != nil {
		t.Fatalf("inline truncated input must not error: %v", err)
	}
	if !ok || call.repairKind != toolInputRepairTruncated {
		t.Fatalf("ok=%v repairKind=%d, want repaired truncated", ok, call.repairKind)
	}
	mustJSON(t, call.Function.Arguments)
}

func TestHandleStreamSurvivesTruncatedToolCallInput(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"writing file"}`,
			`data: {"type":"tool-call","toolCallId":"call_t1","toolName":"write","input":` + jsonQuote(truncatedWriteInput) + `}`,
			`data: {"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if strings.Contains(body, `"error"`) {
		t.Fatalf("stream aborted on truncated tool-call input: %s", body)
	}
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("body missing tool_calls delta: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("truncated arguments must surface as finish_reason length: %s", body)
	}
}

func TestHandleNonStreamSurvivesTruncatedToolCallInput(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"call_t2","toolName":"write","input":` + jsonQuote(truncatedWriteInput) + `}`,
			`data: {"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choice := got.Choices[0]
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d: %s", len(choice.Message.ToolCalls), rec.Body.String())
	}
	if choice.FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", choice.FinishReason)
	}
	mustJSON(t, choice.Message.ToolCalls[0].Function.Arguments)
}

// jsonQuote encodes s as a JSON string literal for embedding in SSE fixture lines.
func jsonQuote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}
