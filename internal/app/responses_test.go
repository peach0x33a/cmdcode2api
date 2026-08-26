package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustUnmarshalResponsesRequest(t *testing.T, body string) *ResponsesRequest {
	t.Helper()
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal ResponsesRequest: %v", err)
	}
	return &req
}

func TestResponsesStringInputBecomesUserMessage(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{"model":"gpt-5","input":"hello there"}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	text, ok := msg.Content.TextValue()
	if !ok || text != "hello there" {
		t.Errorf("content = %q (ok=%v), want %q", text, ok, "hello there")
	}
}

func TestResponsesInstructionsBecomeSystem(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{"model":"gpt-5","instructions":"be nice","input":"hi"}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(chat.Messages))
	}
	sys := chat.Messages[0]
	if sys.Role != "system" {
		t.Errorf("first message role = %q, want system", sys.Role)
	}
	text, ok := sys.Content.TextValue()
	if !ok || text != "be nice" {
		t.Errorf("system content = %q (ok=%v), want %q", text, ok, "be nice")
	}
	if chat.Messages[1].Role != "user" {
		t.Errorf("second message role = %q, want user", chat.Messages[1].Role)
	}
}

func TestResponsesFunctionCallOutputBecomesToolMessage(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":[{"type":"function_call_output","call_id":"call_abc123","output":"42"}]
	}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if msg.Role != "tool" {
		t.Errorf("role = %q, want tool", msg.Role)
	}
	if msg.ToolCallID != "call_abc123" {
		t.Errorf("ToolCallID = %q, want call_abc123", msg.ToolCallID)
	}
	text, ok := msg.Content.TextValue()
	if !ok || text != "42" {
		t.Errorf("content = %q (ok=%v), want 42", text, ok)
	}
}

func TestResponsesFunctionCallItemUsesCallID(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":[{
			"type":"function_call",
			"id":"fc_should_not_be_used",
			"call_id":"call_the_real_id",
			"name":"get_weather",
			"arguments":"{\"city\":\"nyc\"}"
		}]
	}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if msg.Role != "assistant" {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_the_real_id" {
		t.Errorf("ToolCall.ID = %q, want call_the_real_id (must come from call_id, not id)", tc.ID)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("Function.Name = %q, want get_weather", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"city":"nyc"}` {
		t.Errorf("Function.Arguments = %q", tc.Function.Arguments)
	}
}

func TestResponsesInputImageMapsToImageURLPart(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"what is this"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]
		}]
	}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	parts := chat.Messages[0].Content.PartsValue()
	if len(parts) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this" {
		t.Errorf("part[0] = %+v, want text part", parts[0])
	}
	if parts[1].Type != "image_url" {
		t.Fatalf("part[1].Type = %q, want image_url", parts[1].Type)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("part[1].ImageURL = %+v", parts[1].ImageURL)
	}
}

func TestResponsesRejectsFileIDImage(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[{"type":"input_image","file_id":"file-123"}]
		}]
	}`)

	_, err := responsesToChatRequest(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var invalid *invalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected *invalidRequestError, got %T: %v", err, err)
	}
}

func TestResponsesFlatToolConvertsToNestedChatTool(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{
			"type":"function",
			"name":"get_weather",
			"description":"gets the weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}
		}]
	}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}
	tool := chat.Tools[0]
	if tool.Type != "function" {
		t.Errorf("Tool.Type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "get_weather" {
		t.Errorf("Function.Name = %q, want get_weather", tool.Function.Name)
	}
	if tool.Function.Description != "gets the weather" {
		t.Errorf("Function.Description = %q", tool.Function.Description)
	}
	if tool.Function.Parameters == nil {
		t.Error("Function.Parameters is nil")
	}
}

func TestResponsesRejectsNonFunctionToolType(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{"type":"local_shell","name":"shell"}]
	}`)

	_, err := responsesToChatRequest(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var invalid *invalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected *invalidRequestError, got %T: %v", err, err)
	}
}

func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":"hi",
		"previous_response_id":"resp_123"
	}`)

	_, err := responsesToChatRequest(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var invalid *invalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected *invalidRequestError, got %T: %v", err, err)
	}
	if want := "stateless"; !strings.Contains(invalid.Error(), want) {
		t.Errorf("error message %q does not mention %q", invalid.Error(), want)
	}
}

func TestResponsesMaxOutputTokensReachesCCParams(t *testing.T) {
	req := mustUnmarshalResponsesRequest(t, `{
		"model":"gpt-5",
		"input":"hi",
		"max_output_tokens":4096
	}`)

	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chat.MaxCompletionTokens != 4096 {
		t.Errorf("MaxCompletionTokens = %d, want 4096", chat.MaxCompletionTokens)
	}
}

// ============================================================================
// responsesEmitter (streaming SSE emitter) golden-transcript tests.
//
// These drive the emitter directly against a bytes.Buffer (flusher=nil, per
// writeNamedSSE's contract), then parse the buffer's "event:"/"data:" frames
// back out for assertions. No HTTP involved.
// ============================================================================

type sseEvent struct {
	Event string
	Data  map[string]any
}

// parseSSEEvents parses every "event: X\ndata: {...}\n\n" frame out of buf,
// in order, decoding each data line as JSON.
func parseSSEEvents(t *testing.T, buf *bytes.Buffer) []sseEvent {
	t.Helper()
	blocks := strings.Split(buf.String(), "\n\n")
	var events []sseEvent
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatalf("unmarshal event %q data: %v", ev.Event, err)
				}
				ev.Data = payload
			}
		}
		if ev.Event == "" {
			t.Fatalf("SSE block missing event: line: %q", block)
		}
		events = append(events, ev)
	}
	return events
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.Event
	}
	return names
}

func indexOfEvent(names []string, target string) int {
	for i, name := range names {
		if name == target {
			return i
		}
	}
	return -1
}

func assertEventOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q\nfull got: %v", i, got[i], want[i], got)
		}
	}
}

func TestResponsesStreamEventOrderForTextOnly(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.Err(); err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := e.AddTextDelta("Hello, "); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.AddTextDelta("world!"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.Complete(buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got := eventNames(parseSSEEvents(t, &buf))
	assertEventOrder(t, got, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
}

func TestResponsesStreamSequenceNumbersMonotonic(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddReasoningDelta("thinking..."); err != nil {
		t.Fatalf("AddReasoningDelta: %v", err)
	}
	if err := e.AddTextDelta("answer"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.AddToolCall("call_1", "get_weather", `{"city":"nyc"}`); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	if err := e.Complete(buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := parseSSEEvents(t, &buf)
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}
	prev := 0
	for i, ev := range events {
		raw, ok := ev.Data["sequence_number"]
		if !ok {
			t.Fatalf("event %d (%s) missing sequence_number", i, ev.Event)
		}
		seq, ok := raw.(float64)
		if !ok {
			t.Fatalf("event %d (%s) sequence_number not numeric: %v", i, ev.Event, raw)
		}
		if int(seq) != prev+1 {
			t.Fatalf("event %d (%s) sequence_number = %v, want %d", i, ev.Event, seq, prev+1)
		}
		prev = int(seq)
	}
}

func TestResponsesStreamEmitsFunctionCallItemLifecycle(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddToolCall("call_1", "get_weather", `{"city":"nyc"}`); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	if err := e.Complete(buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := parseSSEEvents(t, &buf)
	assertEventOrder(t, eventNames(events), []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})

	deltaEvent := events[indexOfEvent(eventNames(events), "response.function_call_arguments.delta")]
	if delta, _ := deltaEvent.Data["delta"].(string); delta != `{"city":"nyc"}` {
		t.Errorf("function_call_arguments.delta delta = %q, want the full arguments JSON", delta)
	}
}

func TestResponsesStreamReasoningSummaryDeltas(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddReasoningDelta("step one. "); err != nil {
		t.Fatalf("AddReasoningDelta: %v", err)
	}
	if err := e.AddReasoningDelta("step two."); err != nil {
		t.Fatalf("AddReasoningDelta: %v", err)
	}
	if err := e.AddTextDelta("final answer"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.Complete(buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := parseSSEEvents(t, &buf)
	got := eventNames(events)
	assertEventOrder(t, got, []string{
		"response.created",
		"response.in_progress",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})

	reasoningCloseIdx := indexOfEvent(got, "response.reasoning_summary_part.done")
	messageOpenIdx := indexOfEvent(got, "response.output_item.added")
	if reasoningCloseIdx == -1 || messageOpenIdx == -1 || reasoningCloseIdx >= messageOpenIdx {
		t.Fatalf("reasoning must close entirely before the message item opens: part.done idx=%d, output_item.added idx=%d", reasoningCloseIdx, messageOpenIdx)
	}

	doneEvent := events[indexOfEvent(got, "response.reasoning_summary_text.done")]
	if text, _ := doneEvent.Data["text"].(string); text != "step one. step two." {
		t.Errorf("reasoning_summary_text.done text = %q, want full accumulated text %q", text, "step one. step two.")
	}
}

func TestResponsesStreamIncompleteOnTruncation(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddTextDelta("partial output that got cut"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.Incomplete("max_output_tokens", buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Incomplete: %v", err)
	}

	events := parseSSEEvents(t, &buf)
	last := events[len(events)-1]
	if last.Event != "response.incomplete" {
		t.Fatalf("terminal event = %q, want response.incomplete", last.Event)
	}
	for _, ev := range events {
		if ev.Event == "response.completed" {
			t.Fatal("response.completed must never be emitted for a truncated stream")
		}
	}

	respObj, ok := last.Data["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.incomplete missing response object: %v", last.Data)
	}
	if respObj["status"] != "incomplete" {
		t.Errorf("response.status = %v, want incomplete", respObj["status"])
	}
	details, ok := respObj["incomplete_details"].(map[string]any)
	if !ok {
		t.Fatalf("response.incomplete_details missing: %v", respObj)
	}
	if details["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details.reason = %v, want max_output_tokens", details["reason"])
	}
}

func TestResponsesStreamEmitsFailedOnUpstreamError(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddTextDelta("partial"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.Fail("upstream connection dropped"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	events := parseSSEEvents(t, &buf)
	last := events[len(events)-1]
	if last.Event != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed", last.Event)
	}

	respObj, ok := last.Data["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.failed missing response object: %v", last.Data)
	}
	if respObj["status"] != "failed" {
		t.Errorf("response.status = %v, want failed", respObj["status"])
	}
	errObj, ok := respObj["error"].(map[string]any)
	if !ok {
		t.Fatalf("response.failed missing error object: %v", respObj)
	}
	if errObj["message"] != "upstream connection dropped" {
		t.Errorf("error.message = %v, want %q", errObj["message"], "upstream connection dropped")
	}
}

func TestResponsesStreamNoEventsAfterCompleted(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddTextDelta("hello"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	usage := buildResponsesUsage(10, 5, 0, 0)
	if err := e.Complete(usage); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	before := buf.Len()

	if err := e.AddTextDelta("more"); err == nil {
		t.Error("AddTextDelta after Complete: expected error, got nil")
	}
	if err := e.AddReasoningDelta("more"); err == nil {
		t.Error("AddReasoningDelta after Complete: expected error, got nil")
	}
	if err := e.AddToolCall("call_2", "x", "{}"); err == nil {
		t.Error("AddToolCall after Complete: expected error, got nil")
	}
	if err := e.Complete(usage); err == nil {
		t.Error("Complete after Complete: expected error, got nil")
	}
	if err := e.Incomplete("max_output_tokens", usage); err == nil {
		t.Error("Incomplete after Complete: expected error, got nil")
	}
	if err := e.Fail("boom"); err == nil {
		t.Error("Fail after Complete: expected error, got nil")
	}

	if buf.Len() != before {
		t.Errorf("buffer grew after the terminal event: before=%d, after=%d", before, buf.Len())
	}
}

func TestResponsesStreamOutputIndexNeverReused(t *testing.T) {
	var buf bytes.Buffer
	e := newResponsesEmitter(&buf, nil, "gpt-5")
	if err := e.AddReasoningDelta("thinking"); err != nil {
		t.Fatalf("AddReasoningDelta: %v", err)
	}
	if err := e.AddTextDelta("answer"); err != nil {
		t.Fatalf("AddTextDelta: %v", err)
	}
	if err := e.AddToolCall("call_1", "tool_one", "{}"); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	if err := e.AddToolCall("call_2", "tool_two", "{}"); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	if err := e.Complete(buildResponsesUsage(10, 5, 0, 0)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	itemOpenEvents := map[string]bool{
		"response.reasoning_summary_part.added": true,
		"response.output_item.added":            true,
	}
	var indexes []int
	for _, ev := range parseSSEEvents(t, &buf) {
		if !itemOpenEvents[ev.Event] {
			continue
		}
		raw, ok := ev.Data["output_index"]
		if !ok {
			t.Fatalf("event %q missing output_index", ev.Event)
		}
		idx, ok := raw.(float64)
		if !ok {
			t.Fatalf("event %q output_index not numeric: %v", ev.Event, raw)
		}
		indexes = append(indexes, int(idx))
	}

	want := []int{0, 1, 2, 3}
	if len(indexes) != len(want) {
		t.Fatalf("collected %d output_index values, want %d: %v", len(indexes), len(want), indexes)
	}
	seen := make(map[int]bool)
	for i, idx := range indexes {
		if idx != want[i] {
			t.Errorf("output_index[%d] = %d, want %d (allocation order)", i, idx, want[i])
		}
		if seen[idx] {
			t.Errorf("output_index %d reused", idx)
		}
		seen[idx] = true
	}
}

// ============================================================================
// handleResponsesNonStream / handleResponsesStream / handleResponses (HTTP
// handlers). These drive the handlers the same way handler_test.go drives
// handleNonStream/handleStream/handleChatCompletions: a fake *http.Response
// whose Body is a hand-written CC SSE transcript, run through the handler
// directly against an httptest.ResponseRecorder.
// ============================================================================

func TestResponsesNonStreamBuildsOutputArray(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"reasoning-delta","text":"thinking..."}`,
			`data: {"type":"text-delta","text":"hello there"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleResponsesNonStream(rec, resp, "gpt-5", &UsageTracker{}, &Config{}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got ResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if len(got.Output) != 2 {
		t.Fatalf("output = %#v, want 2 items (reasoning, message)", got.Output)
	}
	if got.Output[0].Type != "reasoning" || len(got.Output[0].Summary) != 1 || got.Output[0].Summary[0].Text != "thinking..." {
		t.Fatalf("output[0] = %#v, want reasoning summary %q", got.Output[0], "thinking...")
	}
	if got.Output[1].Type != "message" || got.Output[1].Role != "assistant" {
		t.Fatalf("output[1] = %#v, want assistant message", got.Output[1])
	}
	if len(got.Output[1].Content) != 1 || got.Output[1].Content[0].Text != "hello there" {
		t.Fatalf("output[1].Content = %#v, want %q", got.Output[1].Content, "hello there")
	}
}

func TestResponsesNonStreamUsageFieldNames(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":4,"reasoningTokens":2,"inputTokenDetails":{"cacheReadTokens":3,"cacheWriteTokens":0}}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleResponsesNonStream(rec, resp, "gpt-5", &UsageTracker{}, &Config{}, "")

	var got ResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Usage == nil {
		t.Fatal("usage is nil")
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %#v, want input=10 output=4", got.Usage)
	}
	if got.Usage.InputTokensDetails.CachedTokens != 3 {
		t.Errorf("input_tokens_details.cached_tokens = %d, want 3", got.Usage.InputTokensDetails.CachedTokens)
	}
	// The normalizer now plumbs CCUsage.ReasoningTokens through
	// (ccEventNormalizer.ReasoningTokens, events.go), so this is a real
	// value, not a hardcoded 0.
	if got.Usage.OutputTokensDetails.ReasoningTokens != 2 {
		t.Errorf("output_tokens_details.reasoning_tokens = %d, want 2", got.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

func TestResponsesNonStreamToolCallsBecomeFunctionCallItems(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"call_abc","toolName":"get_weather","input":{"city":"nyc"}}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleResponsesNonStream(rec, resp, "gpt-5", &UsageTracker{}, &Config{}, "")

	var got ResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed (a tool call is not a truncation)", got.Status)
	}
	if len(got.Output) != 1 {
		t.Fatalf("output = %#v, want 1 function_call item", got.Output)
	}
	item := got.Output[0]
	if item.Type != "function_call" || item.CallID != "call_abc" || item.Name != "get_weather" {
		t.Fatalf("item = %#v", item)
	}
	if item.Arguments != `{"city":"nyc"}` {
		t.Errorf("arguments = %q, want %q", item.Arguments, `{"city":"nyc"}`)
	}
}

// TestResponsesStreamRecoversDSMLToolCallFromText ports
// TestHandleStreamRepairsDSMLTerminatedTextToolCall (handler_test.go) to the
// Responses stream path: a model response with a raw DSML tool-call
// envelope embedded as plain text must still produce a proper
// function_call_arguments event sequence, never raw markup leaking into
// output_text.delta.
func TestResponsesStreamRecoversDSMLToolCallFromText(t *testing.T) {
	toolText := `Assistant requested tool edit (call_dsml) with arguments: {"edits":[{"oldText":"old","newText":"new"}]</parameter>
</invoke>
</｜｜DSML｜｜tool_calls>`
	textEvent, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: toolText})
	if err != nil {
		t.Fatalf("marshal text event: %v", err)
	}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"data: " + string(textEvent),
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleResponsesStream(rec, resp, "gpt-5", &UsageTracker{}, &Config{}, true, "")
	body := rec.Body.String()

	if strings.Contains(body, "DSML") || strings.Contains(body, "</parameter>") {
		t.Fatalf("protocol envelope leaked into stream: %s", body)
	}

	events := parseSSEEvents(t, rec.Body)
	names := eventNames(events)
	if idx := indexOfEvent(names, "response.output_text.delta"); idx != -1 {
		t.Fatalf("raw DSML markup leaked into output_text.delta: %s", body)
	}
	if indexOfEvent(names, "response.function_call_arguments.delta") == -1 || indexOfEvent(names, "response.function_call_arguments.done") == -1 {
		t.Fatalf("expected a function_call_arguments event sequence, got events: %v\nbody: %s", names, body)
	}
	if !strings.Contains(body, `"call_dsml"`) || !strings.Contains(body, `"edit"`) {
		t.Fatalf("expected repaired edit tool call for call_dsml, got body = %s", body)
	}
	if !strings.Contains(body, `edits`) {
		t.Fatalf("expected edits arguments, got body = %s", body)
	}
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("terminal event = %q, want response.completed", names[len(names)-1])
	}
}

// TestResponsesStreamRejectsWhenFinishNeverArrives mirrors
// TestHandleStreamRejectsWhenFinishNeverArrives (handler_test.go): an
// upstream that disconnects (or only ever sends finish-step, never finish)
// must make handleResponsesStream emit response.failed as the terminal
// event, not hang, panic, or emit response.completed.
func TestResponsesStreamRejectsWhenFinishNeverArrives(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"partial"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleResponsesStream(rec, resp, "gpt-5", &UsageTracker{}, &Config{}, true, "")

	events := parseSSEEvents(t, rec.Body)
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	last := events[len(events)-1]
	if last.Event != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed. body = %s", last.Event, rec.Body.String())
	}
	for _, ev := range events {
		if ev.Event == "response.completed" {
			t.Fatalf("response.completed must never be emitted when finish never arrived: %s", rec.Body.String())
		}
	}
}

func TestResponsesRouteRejectsGET(t *testing.T) {
	handler := handleResponses(singleAccountPool(&CCClient{}), &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestResponsesRouteRequiresBearerToken(t *testing.T) {
	cfg := &Config{APIKey: "secret"}
	pool := singleAccountPool(NewCCClient("key", "http://example.invalid"))
	handler := newHandler(pool, cfg, &UsageTracker{}, noopReauthManager(), testStore(t))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestResponsesCreditsUsageToServingAccount mirrors
// TestChatCompletionsCreditsUsageToServingAccount (handler_test.go) through
// the full /v1/responses route: a stale account fails auth, a healthy
// account serves the request, and usage must be credited only to the
// account that actually served it.
func TestResponsesCreditsUsageToServingAccount(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid api key","type":"authentication_error"}`)
	}))
	defer stale.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":7,"outputTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))
	}))
	defer healthy.Close()

	pool := newTestAccountPool(
		testAccountEntry{Name: "stale-account", Client: NewCCClient("stale-key", stale.URL)},
		testAccountEntry{Name: "healthy-account", Client: NewCCClient("healthy-key", healthy.URL)},
	)

	cfg := &Config{APIKey: "secret"}
	usage := &UsageTracker{}
	handler := newHandler(pool, cfg, usage, noopReauthManager(), testStore(t))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test/test-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	report := usage.Report()
	if len(report.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want exactly one entry", report.Accounts)
	}
	got := report.Accounts[0]
	if got.Account != "healthy-account" {
		t.Fatalf("credited account = %q, want %q", got.Account, "healthy-account")
	}
	if got.PromptTokens != 7 || got.CompletionTokens != 3 {
		t.Fatalf("account usage = %#v, want prompt=7 completion=3", got)
	}
}
