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
	handler := handleChatCompletions(singleAccountPool(&CCClient{}), &Config{}, &UsageTracker{})
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
	cfg := &Config{ModelOverrides: map[string]bool{"deepseek/deepseek-v4-flash": true}}
	handler := handleChatCompletions(singleAccountPool(&CCClient{}), cfg, &UsageTracker{})
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

// Every model is disabled unless explicitly enabled — cfg.ExcludeModels is a
// legacy field no longer consulted, so a plain Config{} with no overrides
// must 404 any model, gpt-4 included.
func TestChatCompletionsBlocksModelWithNoOverride(t *testing.T) {
	handler := handleChatCompletions(singleAccountPool(&CCClient{Client: &http.Client{}}), &Config{}, &UsageTracker{})
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

func TestChatCompletionsAllowsModelWithExplicitOverride(t *testing.T) {
	cfg := &Config{}
	policy := NewModelPolicy(cfg)
	store := testStore(t)
	if err := policy.SetModelOverrides(store, []string{"deepseek/deepseek-chat"}, true); err != nil {
		t.Fatalf("SetModelOverrides: %v", err)
	}

	handler := handleChatCompletionsWithPolicy(singleAccountPool(&CCClient{Client: &http.Client{}}), cfg, &UsageTracker{}, policy)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// The enabled-model gate should pass. cc.Send will fail with an empty
	// client → 502, not 400/404.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = %d: enabled-model gate blocked an explicitly enabled model", rec.Code)
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d: enabled-model gate blocked an explicitly enabled model", rec.Code)
	}
}

func TestChatCompletionsBlocksProviderQualifiedWithNoOverride(t *testing.T) {
	handler := handleChatCompletions(singleAccountPool(&CCClient{Client: &http.Client{}}), &Config{}, &UsageTracker{})
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

// A model disabled purely through an explicit ModelPolicy override must 404
// — proving dispatchToCCWithPolicy consults the policy directly, not just
// the (default-disabled) fallback.
func TestChatCompletionsBlocksModelDisabledViaPolicyOverrideOnly(t *testing.T) {
	cfg := &Config{}
	policy := NewModelPolicy(cfg)
	store := testStore(t)
	if err := policy.SetModelOverrides(store, []string{"deepseek/deepseek-chat"}, false); err != nil {
		t.Fatalf("SetModelOverrides: %v", err)
	}

	handler := handleChatCompletionsWithPolicy(singleAccountPool(&CCClient{Client: &http.Client{}}), cfg, &UsageTracker{}, policy)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// An explicit ModelPolicy override enabling a model must be allowed through
// even though every model is disabled by default.
func TestChatCompletionsAllowsModelEnabledViaPolicyOverrideDespiteDefaultDisabled(t *testing.T) {
	cfg := &Config{}
	policy := NewModelPolicy(cfg)
	store := testStore(t)
	if err := policy.SetModelOverrides(store, []string{"openai/gpt-4"}, true); err != nil {
		t.Fatalf("SetModelOverrides: %v", err)
	}

	handler := handleChatCompletionsWithPolicy(singleAccountPool(&CCClient{Client: &http.Client{}}), cfg, &UsageTracker{}, policy)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// The enabled-model gate must pass — cc.Send then fails with an empty
	// client, producing a 502, not the 400/404 a gate rejection would give.
	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d: policy override did not beat the default-disabled fallback", rec.Code)
	}
}

func TestChatCompletionsReturnsNormalizedUpstreamRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("x-request-id", "req_123")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You've reached your 5-hour usage limit for your plan.","type":"server_error"}`)
	}))
	defer upstream.Close()

	handler := handleChatCompletions(singleAccountPool(NewCCClient("test-key", upstream.URL)), testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "120" || rec.Header().Get("x-request-id") != "req_123" {
		t.Fatalf("headers = %#v", rec.Header())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "You've reached your 5-hour usage limit for your plan." || body.Error.Type != "rate_limit_error" || body.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("body = %#v", body)
	}
}

func TestChatCompletionsFailsOverToNextAccountOnAuthError(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid api key","type":"authentication_error"}`)
	}))
	defer stale.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))
	}))
	defer healthy.Close()

	pool := newTestAccountPool(
		testAccountEntry{Name: "stale-account", Client: NewCCClient("stale-key", stale.URL)},
		testAccountEntry{Name: "healthy-account", Client: NewCCClient("healthy-key", healthy.URL)},
	)

	handler := handleChatCompletions(pool, testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("body missing content from healthy account: %s", rec.Body.String())
	}

	var staleStatus AccountStatus
	for _, view := range pool.Snapshot() {
		if view.Name == "stale-account" {
			staleStatus = view.Status
		}
	}
	if staleStatus != StatusStale {
		t.Fatalf("stale-account status = %q, want %q", staleStatus, StatusStale)
	}
}

// TestChatCompletionsCreditsUsageToServingAccount is the regression guard for
// the failover + usage-tracking interaction: usage must be credited to the
// account that actually served the request, not the one(s) that failed over
// before it (which sendWithFailover never even returns to the caller).
func TestChatCompletionsCreditsUsageToServingAccount(t *testing.T) {
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

	usage := &UsageTracker{}
	handler := handleChatCompletions(pool, testModelEnabledConfig(), usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hi"}]}`))
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

// A rate-limited account must not make the whole request fail when another
// account in the pool is healthy — Next() deliberately keeps StatusLimited
// accounts in rotation so they can be retried later, which only helps if
// sendWithFailover actually tries the next account instead of returning the
// 429 straight to the caller.
func TestChatCompletionsFailsOverToNextAccountOnRateLimit(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You've reached your 5-hour usage limit for your plan.","type":"server_error"}`)
	}))
	defer limited.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"text-delta","text":"hi"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))
	}))
	defer healthy.Close()

	pool := newTestAccountPool(
		testAccountEntry{Name: "limited-account", Client: NewCCClient("limited-key", limited.URL)},
		testAccountEntry{Name: "healthy-account", Client: NewCCClient("healthy-key", healthy.URL)},
	)

	handler := handleChatCompletions(pool, testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("body missing content from healthy account: %s", rec.Body.String())
	}

	var limitedStatus AccountStatus
	for _, view := range pool.Snapshot() {
		if view.Name == "limited-account" {
			limitedStatus = view.Status
		}
	}
	if limitedStatus != StatusLimited {
		t.Fatalf("limited-account status = %q, want %q", limitedStatus, StatusLimited)
	}
}

func TestChatCompletionsReturns502WhenAllAccountsExhausted(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid api key","type":"authentication_error"}`)
	}))
	defer stale.Close()

	pool := newTestAccountPool(testAccountEntry{Name: "only-account", Client: NewCCClient("stale-key", stale.URL)})
	handler := handleChatCompletions(pool, testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_accounts") {
		t.Fatalf("body missing no_healthy_accounts code: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "--oauth --account") {
		t.Fatalf("body missing remediation hint: %s", rec.Body.String())
	}
}

// A zero-account pool is the normal state for a fresh install before the
// first account is added via /ui — it must return a clean, informative
// error instead of panicking or falling through to the generic
// "all accounts exhausted" message.
func TestChatCompletionsReturns503WhenNoAccountsConfigured(t *testing.T) {
	pool := newTestAccountPool()
	handler := handleChatCompletions(pool, testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/test-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_accounts_configured") {
		t.Fatalf("body missing no_accounts_configured code: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/ui") {
		t.Fatalf("body missing /ui pointer: %s", rec.Body.String())
	}
}

func TestChatCompletionsRejectsRemoteImageURL(t *testing.T) {
	handler := handleChatCompletions(singleAccountPool(&CCClient{}), testModelEnabledConfig(), &UsageTracker{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"test/test-model",
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "base64 data URL") {
		t.Fatalf("body = %s", rec.Body.String())
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

func TestHandleNonStreamExposesCachedTokens(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":12345,"cacheWriteTokens":0}}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens_details":{"cached_tokens":12345}`) {
		t.Fatalf("body should expose cache reads using the OpenAI usage format: %s", rec.Body.String())
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

func TestHandleNonStreamRejectsFinishStepWithoutFinish(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"hello"}`,
			`data: {"type":"finish-step","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":7}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_stream_incomplete") {
		t.Fatalf("status = %d, want 502 incomplete-stream error. body = %s", rec.Code, rec.Body.String())
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

	handleStreamWithOptions(rec, resp, "test-model", &UsageTracker{}, &Config{}, true)

	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("body missing normalized finish reason: %s", body)
	}
	if !strings.Contains(body, `"total_tokens":15`) {
		t.Fatalf("body should use totalUsage.totalTokens without adding local reasoning count: %s", body)
	}
}

func TestHandleStreamExposesCachedTokens(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"ok"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5,"totalTokens":15,"inputTokenDetails":{"cacheReadTokens":12345,"cacheWriteTokens":0}}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStreamWithOptions(rec, resp, "test-model", &UsageTracker{}, &Config{}, true)

	body := rec.Body.String()
	if !strings.Contains(body, `"prompt_tokens_details":{"cached_tokens":12345}`) {
		t.Fatalf("body should expose cache reads using the OpenAI usage format: %s", body)
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

// finish-step is not a terminal event. If [DONE] arrives without finish, the
// proxy must surface an error rather than certify partial text as complete.
func TestHandleStreamRejectsWhenFinishNeverArrives(t *testing.T) {
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
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
	if !strings.Contains(body, `"content":"partial"`) {
		t.Errorf("buffered content was dropped. body = %s", body)
	}
	payloads := decodeStreamPayloads(t, body)
	if !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
		t.Errorf("incomplete stream was not surfaced as an error. body = %s", body)
	}
}

// An aborted tool input remains provisional and must not be released for
// execution without a validated finish event.
func TestHandleStreamRejectsToolCallWhenUpstreamAborts(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-input-start","id":"c1","toolName":"bash"}`,
			`data: {"type":"tool-input-delta","id":"c1","delta":"{\"command\":\"ls -la\"}"}`,
			``, // upstream drops the connection here: no tool-input-end, no finish
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	body := rec.Body.String()
	payloads := decodeStreamPayloads(t, body)
	if len(streamToolCalls(t, payloads)) != 0 || !hasStreamError(payloads) || hasAnyFinishReason(payloads) {
		t.Fatalf("aborted provisional call was exposed: %s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d `data: [DONE]` markers, want 1. body = %s", n, body)
	}
}

// With no overrides at all, every model in the catalog is disabled by
// default and /v1/models must return none of them — cfg.ExcludeModels is a
// legacy field no longer consulted.
func TestHandleModelsAllDisabledByDefault(t *testing.T) {
	oldCatalog := modelCatalog
	t.Cleanup(func() { modelCatalog = oldCatalog })

	modelCatalog = []ModelInfo{
		{ID: "openai/gpt-4"},
		{ID: "anthropic/claude-3"},
		{ID: "google/gemini-1.5-pro"},
		{ID: "deepseek/deepseek-chat"},
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
	if resp.Object != "list" {
		t.Fatalf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("len(data) = %d, want 0", len(resp.Data))
	}
}

// Only models with an explicit true override in model_overrides show up in
// /v1/models.
func TestHandleModelsOnlyExplicitlyEnabled(t *testing.T) {
	oldCatalog := modelCatalog
	t.Cleanup(func() { modelCatalog = oldCatalog })

	modelCatalog = []ModelInfo{
		{ID: "openai/gpt-4"},
		{ID: "anthropic/claude-3"},
		{ID: "deepseek/deepseek-chat"},
	}
	cfg := &Config{ModelOverrides: map[string]bool{"deepseek/deepseek-chat": true}}
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
	if len(resp.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].ID != "deepseek/deepseek-chat" {
		t.Fatalf("data[0].ID = %q, want deepseek/deepseek-chat", resp.Data[0].ID)
	}
}

// =============================================================================
// ToolCallParser integration tests
// =============================================================================

func TestHandleNonStreamToolCallFromText(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_abc) with arguments: {\"file\":\"test.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
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
	if len(got.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	if len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(got.Choices[0].Message.ToolCalls))
	}
	if got.Choices[0].Message.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("tool call name = %q, want read", got.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if !got.Choices[0].Message.Content.IsEmpty() {
		t.Fatalf("content = %v, want empty", got.Choices[0].Message.Content)
	}
}

func TestHandleNonStreamStructuredToolCallNormalizesFinishReason(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"call_structured","toolName":"read","input":{"file":"test.go"}}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	if len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", got.Choices[0].Message.ToolCalls)
	}
}

func TestHandleNonStreamAssemblesToolInputEvents(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-input-start","id":"call_input","toolName":"write"}`,
			`data: {"type":"tool-input-delta","id":"call_input","delta":"{\"path\":"}`,
			`data: {"type":"tool-input-delta","id":"call_input","delta":"\"out.txt\"}"}`,
			`data: {"type":"tool-input-end","id":"call_input"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleNonStream(rec, resp, "test-model", &UsageTracker{}, &Config{})

	var got ChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v body = %s", err, rec.Body.String())
	}
	if len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", got.Choices[0].Message.ToolCalls)
	}
	call := got.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_input" || call.Function.Name != "write" || call.Function.Arguments != `{"path":"out.txt"}` {
		t.Fatalf("tool call = %#v", call)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

func TestHandleStreamToolCallFromText(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_abc) with arguments: {\"file\":\"test.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"function"`) {
		t.Fatalf("expected tool_calls delta chunk, got body = %s", body)
	}
	if !strings.Contains(body, `"name":"read"`) {
		t.Fatalf("expected tool name 'read' in output, got body = %s", body)
	}
	if strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("raw tool-call text leaked into stream output: %s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d [DONE] markers, want 1. body = %s", n, body)
	}
}

func TestHandleStreamRepairsDSMLTerminatedTextToolCall(t *testing.T) {
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
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"id":"call_dsml"`) || !strings.Contains(body, `"name":"edit"`) {
		t.Fatalf("expected repaired edit tool call, got body = %s", body)
	}
	if !strings.Contains(body, `\"edits\"`) {
		t.Fatalf("expected edits arguments, got body = %s", body)
	}
	if strings.Contains(body, `\"path\"`) {
		t.Fatalf("parser invented a missing path: %s", body)
	}
	if strings.Contains(body, `</parameter>`) || strings.Contains(body, `DSML`) {
		t.Fatalf("protocol envelope leaked into stream: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason was not normalized after repaired tool call: %s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("got %d [DONE] markers, want 1. body = %s", n, body)
	}
}

func TestHandleStreamParsesRawDSMLToolCalls(t *testing.T) {
	chunks := []string{
		`先读取两个文件：<｜｜DSML｜｜tool_`,
		`calls><invoke name="read"><parameter name="offset" string="false">95</parameter>`,
		`<parameter name="path" string="true">/tmp/protocol.ts</parameter></invoke>`,
		`<invoke name="read"><parameter name="offset" string="false">185</parameter>`,
		`<parameter name="path" string="true">/tmp/curl.py</parameter></invoke></｜｜DSML｜｜tool_calls>`,
	}
	events := make([]string, 0, len(chunks)+2)
	for _, chunk := range chunks {
		raw, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: chunk})
		if err != nil {
			t.Fatalf("marshal text event: %v", err)
		}
		events = append(events, "data: "+string(raw))
	}
	events = append(events,
		`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: [DONE]`)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join(events, "\n\n")))}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if n := strings.Count(body, `"name":"read"`); n != 2 {
		t.Fatalf("got %d read tool calls, want 2. body = %s", n, body)
	}
	if strings.Contains(body, `<invoke`) || strings.Contains(body, `<parameter`) || strings.Contains(body, `DSML`) {
		t.Fatalf("raw DSML leaked into stream: %s", body)
	}
	if !strings.Contains(body, `"content":"先读取两个文件："`) {
		t.Fatalf("visible preface missing from stream: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason was not normalized: %s", body)
	}
}

func TestHandleStreamToolCallFromTextFragmented(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool "}`,
			`data: {"type":"text-delta","text":"read (call_frag) with arguments: "}`,
			`data: {"type":"text-delta","text":"{\"file\":\"frag.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("expected tool_calls chunk from fragmented text, got body = %s", body)
	}
	if !strings.Contains(body, `"name":"read"`) {
		t.Fatalf("expected tool name 'read', got body = %s", body)
	}
	if strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("raw tool-call text leaked into stream output: %s", body)
	}
}

func TestHandleStreamToolCallFromTextWithLongIDAndFragmentedArguments(t *testing.T) {
	toolID := "call_00_a4J0yCJ48n7O5Yul0Q4u9242"
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (` + toolID + `) with arguments:"}`,
			`data: {"type":"text-delta","text":" {\"filePath\":\"internal/app/handler.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"id":"`+toolID+`"`) {
		t.Fatalf("expected OpenAI tool_calls delta, got body = %s", body)
	}
	if strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("raw tool-call text leaked into stream output: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason was not normalized after parsed tool call: %s", body)
	}
}

func TestHandleStreamTextBeforeAndAfterToolCall(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"prefix "}`,
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_both) with arguments: {\"z\":9}"}`,
			`data: {"type":"text-delta","text":" suffix"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"content":"prefix "`) {
		t.Fatalf("expected prefix content chunk, got body = %s", body)
	}
	if !strings.Contains(body, `"content":" suffix"`) {
		t.Fatalf("expected suffix content chunk, got body = %s", body)
	}
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("expected tool_calls delta chunk, got body = %s", body)
	}
	if strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("raw tool-call text leaked into stream output: %s", body)
	}
}

func TestHandleStreamStructuredAndTextToolCall(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"call_abc","toolName":"read","input":{"file":"test.go"}}`,
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_abc) with arguments: {\"file\":\"test.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	// Tool call with ID "call_abc" must appear exactly ONCE — from the
	// structured event.  The text-parsed duplicate is suppressed.
	if n := strings.Count(body, `"id":"call_abc"`); n != 1 {
		t.Fatalf("got %d tool-call chunks with id=call_abc, want 1. body = %s", n, body)
	}
	if strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("raw tool-call text leaked into stream output: %s", body)
	}
}

func TestHandleStreamStructuredAndRawDSMLToolCallDoesNotDuplicate(t *testing.T) {
	rawDSML := `<｜｜DSML｜｜tool_calls><invoke name="bash"><parameter name="command" string="true">echo once</parameter></invoke></｜｜DSML｜｜tool_calls>`
	textEvent, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: rawDSML})
	if err != nil {
		t.Fatalf("marshal text event: %v", err)
	}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-call","toolCallId":"call_structured_once","toolName":"bash","input":{"command":"echo once"}}`,
			"data: " + string(textEvent),
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if n := strings.Count(body, `"name":"bash"`); n != 1 {
		t.Fatalf("got %d equivalent bash calls, want 1. body = %s", n, body)
	}
	if strings.Contains(body, `DSML`) || strings.Contains(body, `<invoke`) {
		t.Fatalf("raw DSML leaked into stream output: %s", body)
	}
}

func TestHandleStreamDeduplicatesSemanticJSONAcrossStructuredAndRawDSML(t *testing.T) {
	tests := []struct {
		name       string
		structured string
		rawValue   string
	}{
		{
			name:       "large_integer",
			structured: `{"offset":9007199254740993}`,
			rawValue:   `<parameter name="offset" string="false">9007199254740993</parameter>`,
		},
		{
			name:       "nested_key_order",
			structured: `{"options":{"a":1,"b":2}}`,
			rawValue:   `<parameter name="options" string="false">{"b":2,"a":1}</parameter>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rawDSML := `<｜｜DSML｜｜tool_calls><invoke name="read">` + test.rawValue + `</invoke></｜｜DSML｜｜tool_calls>`
			textEvent, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: rawDSML})
			if err != nil {
				t.Fatalf("marshal text event: %v", err)
			}
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"tool-call","toolCallId":"call_semantic","toolName":"read","input":` + test.structured + `}`,
				"data: " + string(textEvent),
				`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
				`data: [DONE]`,
			}, "\n\n")))}
			rec := httptest.NewRecorder()
			handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
			if n := strings.Count(rec.Body.String(), `"name":"read"`); n != 1 {
				t.Fatalf("got %d semantically equivalent calls, want 1. body = %s", n, rec.Body.String())
			}
		})
	}
}

func TestHandleStreamDeduplicatesStructuredAndRawOneToOne(t *testing.T) {
	rawDSML := `<｜｜DSML｜｜tool_calls><invoke name="read"><parameter name="path" string="true">same.txt</parameter></invoke><invoke name="read"><parameter name="path" string="true">same.txt</parameter></invoke></｜｜DSML｜｜tool_calls>`
	textEvent, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: rawDSML})
	if err != nil {
		t.Fatalf("marshal text event: %v", err)
	}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"tool-call","toolCallId":"call_structured_same","toolName":"read","input":{"path":"same.txt"}}`,
		"data: " + string(textEvent),
		`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")))}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()
	if n := strings.Count(body, `"name":"read"`); n != 2 {
		t.Fatalf("got %d calls, want one duplicate suppressed and one repeated invoke kept. body = %s", n, body)
	}
}

func TestHandleStreamKeepsTwoIntentionalEquivalentRawDSMLCalls(t *testing.T) {
	rawDSML := `<｜｜DSML｜｜tool_calls><invoke name="read"><parameter name="path" string="true">same.txt</parameter></invoke><invoke name="read"><parameter name="path" string="true">same.txt</parameter></invoke></｜｜DSML｜｜tool_calls>`
	textEvent, err := json.Marshal(CCStreamEvent{Type: "text-delta", Text: rawDSML})
	if err != nil {
		t.Fatalf("marshal text event: %v", err)
	}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		"data: " + string(textEvent),
		`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")))}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()
	if n := strings.Count(body, `"name":"read"`); n != 2 {
		t.Fatalf("got %d intentional equivalent raw calls, want 2. body = %s", n, body)
	}
}

func TestHandleStreamAssemblesToolInputEvents(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"tool-input-start","id":"call_stream_input","toolName":"write"}`,
			`data: {"type":"tool-input-delta","id":"call_stream_input","delta":"{\"path\":\"out.txt\"}"}`,
			`data: {"type":"tool-input-end","id":"call_stream_input"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"id":"call_stream_input"`) || !strings.Contains(body, `"name":"write"`) {
		t.Fatalf("tool input events were not emitted: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason was not normalized: %s", body)
	}
}

func TestHandleStreamTextAndReasoningUseSeparateParserBuffers(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"reasoning-delta","text":"think first"}`,
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_separate) with arguments: {\"file\":\"test.go\"}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"reasoning_content":"think first"`) {
		t.Fatalf("reasoning text was not emitted as reasoning_content: %s", body)
	}
	if strings.Contains(body, `"content":"think first"`) {
		t.Fatalf("reasoning text leaked into normal content: %s", body)
	}
	if !strings.Contains(body, `"id":"call_separate"`) {
		t.Fatalf("text tool call was not converted: %s", body)
	}
}

func TestHandleStreamTextThenStructuredToolCallIsDeduplicated(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_late) with arguments: {\"file\":\"test.go\"}"}`,
			`data: {"type":"tool-call","toolCallId":"call_late","toolName":"read","input":{"file":"test.go"}}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()

	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if count := strings.Count(body, `"id":"call_late"`); count != 1 {
		t.Fatalf("tool call emitted %d times, want once: %s", count, body)
	}
}

func TestHandleNonStreamMultipleToolCallsFromText(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_1) with arguments: {\"a\":1}\nAssistant requested tool write (call_2) with arguments: {\"b\":2}"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
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
	if len(got.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(got.Choices[0].Message.ToolCalls))
	}
	if got.Choices[0].Message.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("tool call[0] name = %q, want read", got.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if got.Choices[0].Message.ToolCalls[1].Function.Name != "write" {
		t.Fatalf("tool call[1] name = %q, want write", got.Choices[0].Message.ToolCalls[1].Function.Name)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

func TestHandleStreamPureTextUnchanged(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Hello"}`,
			`data: {"type":"text-delta","text":" world"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" world"`) {
		t.Fatalf("expected text deltas to be emitted separately: %s", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("unexpected tool_calls in pure text output: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason stop: %s", body)
	}
}

func TestHandleStreamInvalidArgumentsVariant(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"text-delta","text":"Assistant requested tool read (call_bad) with invalid arguments: some parse error"}`,
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2}}`,
			`data: [DONE]`,
		}, "\n\n"))),
	}
	rec := httptest.NewRecorder()
	handleStream(rec, resp, "test-model", &UsageTracker{}, &Config{})
	body := rec.Body.String()

	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("unexpected tool_calls for invalid arguments variant: %s", body)
	}
	if !strings.Contains(body, `invalid arguments`) {
		t.Fatalf("expected invalid arguments text in output content: %s", body)
	}
	if !strings.Contains(body, `Assistant requested tool`) {
		t.Fatalf("expected tool-call text to pass through as content: %s", body)
	}
}
