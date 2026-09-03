package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleStreamSurfacesUpstreamError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"partial"}`,
		`data: {"type":"error","error":{"message":"boom"}}`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasStreamError(payloads) {
		t.Fatalf("stream did not surface upstream error: %s", rec.Body.String())
	}
	if hasFinishReason(payloads, "length") || hasFinishReason(payloads, "stop") {
		t.Fatalf("upstream error was disguised as a completion: %s", rec.Body.String())
	}
	if strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("error stream must terminate exactly once: %s", rec.Body.String())
	}
}

func TestHandleStreamRejectsDoneWithoutFinish(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"partial"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasStreamError(payloads) {
		t.Fatalf("[DONE] without finish was accepted: %s", rec.Body.String())
	}
	if hasAnyFinishReason(payloads) {
		t.Fatalf("incomplete stream emitted a success finish_reason: %s", rec.Body.String())
	}
}

func TestHandleStreamRejectsEOFWithoutFinish(t *testing.T) {
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(`data: {"type":"text-delta","text":"partial"}`),
		"test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasStreamError(payloads) {
		t.Fatalf("EOF without finish was accepted: %s", rec.Body.String())
	}
	if hasAnyFinishReason(payloads) {
		t.Fatalf("EOF emitted a success finish_reason: %s", rec.Body.String())
	}
}

func TestHandleNonStreamRejectsIncompleteUpstream(t *testing.T) {
	for _, body := range []string{
		strings.Join([]string{`data: {"type":"text-delta","text":"partial"}`, `data: [DONE]`}, "\n\n"),
		`data: {"type":"text-delta","text":"partial"}`,
	} {
		rec := httptest.NewRecorder()
		handleNonStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
		}
	}
}

func TestParseStreamEventsCombinesMultilineData(t *testing.T) {
	body := strings.Join([]string{
		"data: {\"type\":\"text-delta\",",
		"data: \"text\":\"hello\"}",
		"",
		`data: {"type":"finish","finishReason":"stop"}`,
		"",
	}, "\n")
	var events []CCStreamEvent

	err := ParseStreamEvents(streamResponse(body), func(ev CCStreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("parse multiline SSE: %v", err)
	}
	if len(events) != 2 || events[0].Type != "text-delta" || events[0].Text != "hello" || events[1].Type != "finish" {
		t.Fatalf("events = %#v", events)
	}
}

func TestHandleStreamPrefersAuthoritativeToolCall(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"write"}`,
		`data: {"type":"tool-input-delta","id":"c1","delta":"{\"path\":\"/a\"}"}`,
		`data: {"type":"tool-input-end","id":"c1"}`,
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"write","input":{"path":"/a","content":"complete"}}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	calls := streamToolCalls(t, decodeStreamPayloads(t, rec.Body.String()))
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	arguments := calls[0]["function"].(map[string]any)["arguments"]
	if !strings.Contains(arguments.(string), `"content":"complete"`) {
		t.Fatalf("provisional arguments won over authoritative payload: %s", arguments)
	}
}

func TestHandleStreamPreservesToolCallOrderAcrossRepairs(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"a","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"a","delta":"{\"command\":\"first"}`,
		`data: {"type":"tool-input-end","id":"a"}`,
		`data: {"type":"tool-input-start","id":"b","toolName":"bash"}`,
		`data: {"type":"tool-input-delta","id":"b","delta":"{\"command\":\"second\"}"}`,
		`data: {"type":"tool-input-end","id":"b"}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	calls := streamToolCalls(t, decodeStreamPayloads(t, rec.Body.String()))
	if len(calls) != 2 || calls[0]["id"] != "a" || calls[1]["id"] != "b" {
		t.Fatalf("tool call order = %#v", calls)
	}
}

func TestSyntaxRepairDoesNotReportLength(t *testing.T) {
	delta, err := json.Marshal(CCStreamEvent{
		Type: "tool-input-delta", ID: "c1", ToolName: "bash",
		Delta: "{\"command\":\"line1\nline2\"}",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
		"data: " + string(delta),
		`data: {"type":"tool-input-end","id":"c1"}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasFinishReason(payloads, "tool_calls") || hasFinishReason(payloads, "length") {
		t.Fatalf("syntax-only repair was marked truncated: %s", rec.Body.String())
	}
}

func TestStructuredToolCallValidation(t *testing.T) {
	// A tool-call event missing its function name cannot be repaired into
	// anything the client could execute, so it still aborts the stream.
	t.Run("missing name", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"c1","input":{"command":"ls"}}`,
			`data: {"type":"finish","finishReason":"tool-calls"}`,
			`data: [DONE]`,
		}, "\n\n")
		rec := httptest.NewRecorder()
		handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		payloads := decodeStreamPayloads(t, rec.Body.String())
		if !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
			t.Fatalf("unrecoverable call was accepted: %s", rec.Body.String())
		}
	})

	// Recoverable malformations must degrade to a repaired call instead of
	// aborting a stream the client may have already read text from.
	t.Run("scalar input recovers with fallback arguments", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":"ls"}`,
			`data: {"type":"finish","finishReason":"tool-calls"}`,
			`data: [DONE]`,
		}, "\n\n")
		rec := httptest.NewRecorder()
		handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		if hasStreamError(decodeStreamPayloads(t, rec.Body.String())) {
			t.Fatalf("recoverable call aborted the stream: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"finish_reason":"length"`) {
			t.Fatalf("fallback arguments must surface as finish_reason length: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `{\"command\":\"ls\"}`) {
			t.Fatalf("expected bash fallback arguments: %s", rec.Body.String())
		}
	})

	t.Run("array input recovers with fallback arguments", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":["ls"]}`,
			`data: {"type":"finish","finishReason":"tool-calls"}`,
			`data: [DONE]`,
		}, "\n\n")
		rec := httptest.NewRecorder()
		handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		if hasStreamError(decodeStreamPayloads(t, rec.Body.String())) {
			t.Fatalf("recoverable call aborted the stream: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"finish_reason":"length"`) {
			t.Fatalf("fallback arguments must surface as finish_reason length: %s", rec.Body.String())
		}
	})

	t.Run("missing id is synthesized", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"tool-call","toolName":"bash","input":{"command":"ls"}}`,
			`data: {"type":"finish","finishReason":"tool-calls"}`,
			`data: [DONE]`,
		}, "\n\n")
		rec := httptest.NewRecorder()
		handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		if hasStreamError(decodeStreamPayloads(t, rec.Body.String())) {
			t.Fatalf("missing id aborted the stream: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"id":"call_recovered_`) {
			t.Fatalf("expected a synthesized tool call id: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
			t.Fatalf("expected clean call to keep finish_reason tool_calls: %s", rec.Body.String())
		}
	})
}

func TestNoArgumentToolCallUsesEmptyObject(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"status"}`,
		`data: {"type":"finish","finishReason":"tool-calls"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	calls := streamToolCalls(t, decodeStreamPayloads(t, rec.Body.String()))
	if len(calls) != 1 || calls[0]["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("no-argument call = %#v", calls)
	}
}

func TestFinishReasonUsesRawFallbackAndRejectsUnknown(t *testing.T) {
	t.Run("raw end turn", func(t *testing.T) {
		body := strings.Join([]string{`data: {"type":"finish","rawFinishReason":"end_turn"}`, `data: [DONE]`}, "\n\n")
		rec := httptest.NewRecorder()
		handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
		if !hasFinishReason(decodeStreamPayloads(t, rec.Body.String()), "stop") {
			t.Fatalf("raw finish reason was not normalized: %s", rec.Body.String())
		}
	})

	for _, reason := range []string{"", "provider_specific"} {
		t.Run("reject "+reason, func(t *testing.T) {
			finish := `data: {"type":"finish"}`
			if reason != "" {
				finish = `data: {"type":"finish","finishReason":"` + reason + `"}`
			}
			rec := httptest.NewRecorder()
			handleStream(rec, streamResponse(strings.Join([]string{finish, `data: [DONE]`}, "\n\n")),
				"test-model", &UsageTracker{}, &Config{})
			payloads := decodeStreamPayloads(t, rec.Body.String())
			if !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
				t.Fatalf("unknown finish reason was accepted: %s", rec.Body.String())
			}
		})
	}
}

func TestContentFilterFinishReasonWinsOverToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {"type":"finish","finishReason":"content_filter"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasFinishReason(payloads, "content_filter") || hasFinishReason(payloads, "tool_calls") {
		t.Fatalf("content_filter was overridden by tool calls: %s", rec.Body.String())
	}
}

func TestCompleteToolCallOverridesLengthFinish(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"c1","toolName":"bash","input":{"command":"ls"}}`,
		`data: {"type":"finish","finishReason":"length"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	if !hasFinishReason(payloads, "tool_calls") || hasFinishReason(payloads, "length") {
		t.Fatalf("complete authoritative call was treated as truncated: %s", rec.Body.String())
	}
}

func TestHandleStreamUsesStableCompletionEnvelope(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"hello"}`,
		`data: {"type":"text-delta","text":" world"}`,
		`data: {"type":"finish","finishReason":"stop"}`,
		`data: [DONE]`,
	}, "\n\n")
	rec := httptest.NewRecorder()

	handleStream(rec, streamResponse(body), "test-model", &UsageTracker{}, &Config{})

	payloads := decodeStreamPayloads(t, rec.Body.String())
	var id string
	var created float64
	for _, payload := range payloads {
		if payload["object"] != "chat.completion.chunk" {
			continue
		}
		gotID, _ := payload["id"].(string)
		gotCreated, _ := payload["created"].(float64)
		if gotID == "" || gotCreated <= 0 {
			t.Fatalf("invalid completion identity: %#v", payload)
		}
		if id == "" {
			id, created = gotID, gotCreated
		} else if gotID != id || gotCreated != created {
			t.Fatalf("completion identity changed: first=(%s,%.0f) next=(%s,%.0f)", id, created, gotID, gotCreated)
		}
	}
}

func TestHandleStreamUsageFollowsStreamOptions(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"hello"}`,
		`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":3,"outputTokens":4,"totalTokens":7}}`,
		`data: [DONE]`,
	}, "\n\n")

	withoutUsage := httptest.NewRecorder()
	handleStream(withoutUsage, streamResponse(body), "test-model", &UsageTracker{}, &Config{})
	for _, payload := range decodeStreamPayloads(t, withoutUsage.Body.String()) {
		if _, exists := payload["usage"]; exists {
			t.Fatalf("usage was sent without include_usage: %s", withoutUsage.Body.String())
		}
	}

	withUsage := httptest.NewRecorder()
	handleStreamWithOptions(withUsage, streamResponse(body), "test-model", &UsageTracker{}, &Config{}, true)
	payloads := decodeStreamPayloads(t, withUsage.Body.String())
	usageChunks := 0
	for _, payload := range payloads {
		usage, hasUsage := payload["usage"].(map[string]any)
		if !hasUsage {
			continue
		}
		usageChunks++
		choices, ok := payload["choices"].([]any)
		if !ok || len(choices) != 0 || usage["total_tokens"] != float64(7) {
			t.Fatalf("invalid usage-only chunk: %#v", payload)
		}
	}
	if usageChunks != 1 {
		t.Fatalf("usage chunks = %d, want 1; body = %s", usageChunks, withUsage.Body.String())
	}

	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options not decoded: %#v", req.StreamOptions)
	}
}

func streamResponse(body string) *http.Response {
	return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
}

func decodeStreamPayloads(t *testing.T, body string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			t.Fatalf("decode SSE payload %q: %v", payload, err)
		}
		payloads = append(payloads, value)
	}
	return payloads
}

func hasStreamError(payloads []map[string]any) bool {
	for _, payload := range payloads {
		if _, ok := payload["error"]; ok {
			return true
		}
	}
	return false
}

func streamToolCalls(t *testing.T, payloads []map[string]any) []map[string]any {
	t.Helper()
	var calls []map[string]any
	for _, payload := range payloads {
		choices, _ := payload["choices"].([]any)
		for _, choice := range choices {
			item, _ := choice.(map[string]any)
			delta, _ := item["delta"].(map[string]any)
			rawCalls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range rawCalls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					t.Fatalf("tool call = %#v", rawCall)
				}
				calls = append(calls, call)
			}
		}
	}
	return calls
}

func hasAnyFinishReason(payloads []map[string]any) bool {
	for _, payload := range payloads {
		choices, _ := payload["choices"].([]any)
		for _, choice := range choices {
			item, _ := choice.(map[string]any)
			if reason, ok := item["finish_reason"]; ok && reason != nil {
				return true
			}
		}
	}
	return false
}

func hasFinishReason(payloads []map[string]any, want string) bool {
	for _, payload := range payloads {
		choices, _ := payload["choices"].([]any)
		for _, choice := range choices {
			item, _ := choice.(map[string]any)
			if item["finish_reason"] == want {
				return true
			}
		}
	}
	return false
}
