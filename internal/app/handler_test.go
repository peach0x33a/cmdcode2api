package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatCompletionsRequiresModel(t *testing.T) {
	handler := handleChatCompletions(&CCClient{}, &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChatCompletionsRequiresMessages(t *testing.T) {
	handler := handleChatCompletions(&CCClient{}, &Config{}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "messages is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChatCompletionsBlocksExcludedModel(t *testing.T) {
	handler := handleChatCompletions(&CCClient{Client: &http.Client{}}, &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("body missing 'not available': %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("body missing error type: %s", rec.Body.String())
	}
}

func TestChatCompletionsAllowsNonExcludedModel(t *testing.T) {
	handler := handleChatCompletions(&CCClient{Client: &http.Client{}}, &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Exclusion gate should pass. cc.Send will fail with empty client → 502, not 400.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = %d: exclusion gate blocked non-excluded model", rec.Code)
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d: exclusion gate blocked non-excluded model", rec.Code)
	}
}

func TestChatCompletionsBlocksProviderQualified(t *testing.T) {
	handler := handleChatCompletions(&CCClient{Client: &http.Client{}}, &Config{ExcludeModels: []string{"gpt-"}}, &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openai/gpt-4") {
		t.Fatalf("body missing model name: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("body missing error type: %s", rec.Body.String())
	}
}

func TestHandleNonStreamAppendsTextDeltas(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"text-delta","text":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello world"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleNonStreamAppendsDeltaFieldFallback(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","delta":"hello"}`,
			`data: {"type":"text-delta","delta":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello world"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleNonStreamNormalizesFinishReason(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish","finishReason":"max_output_tokens","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", got.Choices[0].FinishReason)
	}
}

func TestHandleNonStreamIgnoresFinishStep(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":20}}`,
			`data: {"type":"finish-step","finishReason":"tool_calls","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v body = %s", err, rec.Body.String())
	}
	if len(got.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want \"stop\" (finish-step after finish must not overwrite)", got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 10 {
		t.Fatalf("prompt_tokens = %d, want 10 (finish-step after finish must not overwrite)", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 20 {
		t.Fatalf("completion_tokens = %d, want 20 (finish-step after finish must not overwrite)", got.Usage.CompletionTokens)
	}
}

func TestHandleNonStreamFinishStepWithoutFinishYieldsEmptyState(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":7}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Choices[0].FinishReason != "" {
		t.Fatalf("finish_reason = %q, want \"\" (no finish event)", got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 0 || got.Usage.CompletionTokens != 0 {
		t.Fatalf("usage = %+v, want zero (no finish event)", got.Usage)
	}
}

func TestHandleStreamEmitsDoneOnceWithFinishStep(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
	if !strings.Contains(body, `"content":"hello"`) {
		t.Fatalf("body missing text-delta chunk: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("body missing finish chunk: %s", body)
	}
}

func TestHandleStreamUsesTotalUsageTotalTokens(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"reasoning-delta","text":"think"}`,
			`data: {"type":"text-delta","text":"ok"}`,
			`data: {"type":"finish","finishReason":"max_tokens","totalUsage":{"inputTokens":10,"outputTokens":5,"reasoningTokens":4,"totalTokens":15}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("body missing normalized finish reason: %s", body)
	}
	if !strings.Contains(body, `"total_tokens":15`) {
		t.Fatalf("body should use totalUsage.totalTokens without adding local reasoning count: %s", body)
	}
}

func TestHandleStreamEmitsDoneOnFinishOnly(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
}

func TestHandleStreamNoDoneOnFinishStepOnly(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"partial"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 0 {
		t.Fatalf("got %d `data: [DONE]` markers, want 0 (no finish event). body = %s", n, body)
	}
}

func TestHandleModelsExcludesPrefixes(t *testing.T) {
	oldCatalog := modelCatalog
	t.Cleanup(func() { modelCatalog = oldCatalog })

	modelCatalog = []ModelInfo{
		{ID: "openai/gpt-4"},
		{ID: "anthropic/claude-3"},
		{ID: "google/gemini-1.5-pro"},
		{ID: "deepseek/deepseek-chat"},
	}
	cfg := &Config{ExcludeModels: []string{"gpt-", "claude-", "gemini-"}}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Fatalf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].ID != "deepseek/deepseek-chat" {
		t.Fatalf("data[0].ID = %q, want deepseek/deepseek-chat", resp.Data[0].ID)
	}
}

func TestHandleModelsNoExclusions(t *testing.T) {
	oldCatalog := modelCatalog
	t.Cleanup(func() { modelCatalog = oldCatalog })

	modelCatalog = []ModelInfo{
		{ID: "openai/gpt-4"},
		{ID: "anthropic/claude-3"},
	}
	cfg := &Config{}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(resp.Data))
	}
}

func TestHandleModelsAllExcluded(t *testing.T) {
	oldCatalog := modelCatalog
	t.Cleanup(func() { modelCatalog = oldCatalog })

	modelCatalog = []ModelInfo{
		{ID: "openai/gpt-4"},
		{ID: "anthropic/claude-3"},
	}
	cfg := &Config{ExcludeModels: []string{"gpt-", "claude-"}}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("len(data) = %d, want 0", len(resp.Data))
	}
}
