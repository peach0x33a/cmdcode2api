package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxChatRequestBytes = 50 * 1024 * 1024

var debugMode bool

// maxFailoverAttempts caps how many accounts one request will try before
// giving up. Capped rather than trying every account so a request against a
// large pool of mostly-stale accounts still fails fast.
const maxFailoverAttempts = 3

// handleChatCompletions is a backward-compatible shim over
// handleChatCompletionsWithPolicy for callers (mainly tests) that don't care
// about model overrides: it builds a ModelPolicy from cfg once, at handler
// construction time, exactly mirroring the exclude_models-only behavior this
// function had before ModelPolicy existed.
func handleChatCompletions(pool *AccountPool, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return handleChatCompletionsWithPolicy(pool, cfg, usage, NewModelPolicy(cfg))
}

func handleChatCompletionsWithPolicy(pool *AccountPool, cfg *Config, usage *UsageTracker, policy *ModelPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxChatRequestBytes)

		var req ChatRequest
		if cfg.Debug {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, 400, "invalid_request_error", "bad request body: "+err.Error())
				return
			}
			log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> body", ansiGreen), colorize(string(bodyBytes), ansiCyan))
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid_request_error", "bad request body: "+err.Error())
			return
		}
		setMonitorModel(r.Context(), req.Model, req.Stream)

		disp, ok := dispatchToCCWithPolicy(w, r.Context(), pool, cfg, policy, &req)
		if !ok {
			return
		}
		setMonitorAccount(r.Context(), disp.account)
		wrapUpstreamBody(r.Context(), disp.resp)

		if req.Stream {
			includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
			handleStreamForAccount(w, disp.resp, req.Model, usage, cfg, includeUsage, disp.account)
		} else {
			handleNonStreamForAccount(w, disp.resp, req.Model, usage, cfg, disp.account)
		}
		if err := usage.save(); err != nil {
			log.Printf("%s save usage: %v", colorize("[ERROR]", ansiRed), err)
		}
	}
}

// ccDispatch bundles a successful upstream response together with the name
// of the account that actually served it, so callers can credit usage to the
// right account instead of the one(s) that failed over before it.
type ccDispatch struct {
	resp    *http.Response
	account string
}

// dispatchToCC is a backward-compatible shim over dispatchToCCWithPolicy for
// callers (mainly tests) that don't care about model overrides: it builds a
// ModelPolicy from cfg on every call, exactly mirroring the
// exclude_models-only behavior this function had before ModelPolicy existed.
func dispatchToCC(w http.ResponseWriter, ctx context.Context, pool *AccountPool, cfg *Config, req *ChatRequest) (ccDispatch, bool) {
	return dispatchToCCWithPolicy(w, ctx, pool, cfg, NewModelPolicy(cfg), req)
}

// dispatchToCCWithPolicy validates the request, guards against
// no-accounts/disabled models (per policy — see ModelPolicy.Enabled), and
// sends it through sendWithFailover, translating any resulting error into an
// HTTP response. It returns (disp, true) on success and writes the error
// response itself on failure, returning (ccDispatch{}, false).
func dispatchToCCWithPolicy(w http.ResponseWriter, ctx context.Context, pool *AccountPool, cfg *Config, policy *ModelPolicy, req *ChatRequest) (ccDispatch, bool) {
	if req.Model == "" {
		writeError(w, 400, "invalid_request_error", "model is required")
		return ccDispatch{}, false
	}
	if !policy.Enabled(req.Model) {
		writeError(w, 404, "invalid_request_error", fmt.Sprintf("model %q is not available — enable it in the Models tab at /ui#models", req.Model))
		return ccDispatch{}, false
	}
	if len(req.Messages) == 0 {
		writeError(w, 400, "invalid_request_error", "messages is required")
		return ccDispatch{}, false
	}

	// No accounts configured at all is a distinct, much more common case
	// for a fresh install than every account going stale — surface it as
	// its own clean error pointing at /ui instead of the generic
	// "exhausted/stale" message sendWithFailover would otherwise produce.
	if pool.Len() == 0 {
		writeErrorWithCode(w, http.StatusServiceUnavailable, "server_error", "no_accounts_configured",
			"no accounts configured — add one at /ui")
		return ccDispatch{}, false
	}

	disp, err := sendWithFailover(ctx, pool, req)
	if err != nil {
		var authExhausted *authExhaustedError
		if errors.As(err, &authExhausted) {
			log.Printf("%s %s", colorize("[ERROR]", ansiRed), authExhausted.Error())
			writeErrorWithCode(w, http.StatusBadGateway, "server_error", "no_healthy_accounts",
				"all configured Command Code accounts are stale or unavailable; check GET /accounts or run --oauth --account <name> to reauthorize")
			return ccDispatch{}, false
		}
		var invalid *invalidRequestError
		if errors.As(err, &invalid) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", invalid.Error())
			return ccDispatch{}, false
		}
		var upstreamErr *upstreamAPIError
		if errors.As(err, &upstreamErr) {
			log.Printf("%s cc send: %v", colorize("[ERROR]", ansiRed), upstreamErr)
			if upstreamErr.RetryAfter != "" {
				w.Header().Set("Retry-After", upstreamErr.RetryAfter)
			}
			if upstreamErr.RequestID != "" {
				w.Header().Set("x-request-id", upstreamErr.RequestID)
			}
			writeErrorWithCode(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Code, upstreamErr.Message)
			return ccDispatch{}, false
		}
		log.Printf("%s cc send: %v", colorize("[ERROR]", ansiRed), err)
		writeError(w, http.StatusBadGateway, "server_error", "upstream error: "+err.Error())
		return ccDispatch{}, false
	}

	return disp, true
}

// authExhaustedError means every account this request tried came back with an
// auth failure (or the pool had no eligible account to try in the first
// place). It is reported distinctly from a plain upstreamAPIError because the
// fix is different: reauthorize an account, not retry the request.
type authExhaustedError struct {
	cause error
}

func (e *authExhaustedError) Error() string {
	return fmt.Sprintf("all command code accounts exhausted or stale: %v", e.cause)
}

func (e *authExhaustedError) Unwrap() error {
	return e.cause
}

// sendWithFailover tries up to maxFailoverAttempts accounts from pool. A
// 401/403 (stale key) or a rate-limit/quota-exhaustion 429 is treated as
// worth failing over from — the pool likely has another account that is not
// currently limited, and Next() specifically keeps StatusLimited accounts
// eligible so they can be retried once the limit resets, which only helps if
// this loop actually moves on instead of handing the caller an immediate
// 429. A rate limit on the last available attempt is returned to the caller
// as-is (rather than folded into authExhaustedError below) since there is no
// further account to try and authExhaustedError's "every account is a dead
// key, go reauthorize" message would be actively wrong for "every account
// happens to be rate-limited right now." A bad request, 5xx, or network
// error is returned immediately without touching the account's status
// (beyond RecordError's lastError bookkeeping), since none of those indicate
// another account would fare any better.
//
// Every non-nil error from client.Send, not just ones that assert to
// *upstreamAPIError, is passed to pool.RecordError: a network-level failure
// (timeout, connection reset, DNS error) never produces an *upstreamAPIError
// but is just as real a reason for /accounts to show something other than a
// blank lastError next to a "healthy" status. RecordError itself does the
// errors.As check to decide whether the failure is Command Code's
// rate-limit/quota-exhaustion signal, which is the only case that changes
// status — health-check probes only exercise the unmetered
// /provider/v1/models endpoint and cannot see quota exhaustion on their own.
//
// A successful response calls pool.MarkHealthy: real traffic succeeding is
// the only thing that proves a StatusLimited account's quota has actually
// recovered, as opposed to the periodic probe's blind model-listing check.
func sendWithFailover(ctx context.Context, pool *AccountPool, req *ChatRequest) (ccDispatch, error) {
	attempts := pool.Len()
	if attempts > maxFailoverAttempts {
		attempts = maxFailoverAttempts
	}
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		acct, err := pool.Next()
		if err != nil {
			return ccDispatch{}, &authExhaustedError{cause: err}
		}

		// acct.client can be rebuilt concurrently by UpdateAccount, so copy
		// the pointer under the lock and release before the network call —
		// the lock must never be held across I/O.
		acct.mu.Lock()
		client := acct.client
		acct.mu.Unlock()

		sendStart := time.Now()
		resp, err := client.Send(ctx, req)
		monitorFromContext(ctx).addUpstream(time.Since(sendStart))
		if err == nil {
			pool.MarkHealthy(acct.Name)
			return ccDispatch{resp: resp, account: acct.Name}, nil
		}

		var upstreamErr *upstreamAPIError
		if errors.As(err, &upstreamErr) && (upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden) {
			pool.MarkStale(acct.Name, upstreamErr.Error())
			lastErr = err
			continue
		}
		pool.RecordError(acct.Name, err)
		// A rate limit is worth trying the next account for — but only if
		// there is another attempt left to make. On the last attempt there is
		// nothing left to fail over to, so the caller should see the real 429
		// (and its Retry-After/message) rather than the generic
		// authExhaustedError below, which is written for "every account is a
		// dead key" and would be a misleading message for "every account
		// happens to be rate-limited right now."
		if errors.As(err, &upstreamErr) && upstreamErr.Type == "rate_limit_error" && i < attempts-1 {
			lastErr = err
			continue
		}
		return ccDispatch{}, err
	}
	return ccDispatch{}, &authExhaustedError{cause: lastErr}
}

func handleStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	handleStreamWithOptions(w, resp, model, usage, cfg, false)
}

func handleStreamWithOptions(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, includeUsage bool) {
	handleStreamForAccount(w, resp, model, usage, cfg, includeUsage, "")
}

func handleStreamForAccount(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, includeUsage bool, account string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "server_error", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	streamID := genStreamID()
	created := time.Now().Unix()
	firstText := true
	var done bool      // [DONE] has been written; nothing more may be emitted
	var finishing bool // finishStream is running its final flush

	normalizer := newCCEventNormalizer()
	textParser := NewToolCallParser()
	reasoningParser := NewToolCallParser()
	var collectedToolCalls toolCallDeduper

	emitContent := func(content string, reasoning bool) {
		if content == "" || done {
			// Once [DONE] is written the stream is closed; anything more would
			// land after it where no OpenAI client will read it. The final flush
			// runs with finishing=true but done=false, so it is still allowed.
			return
		}
		delta := StreamDelta{}
		if reasoning {
			delta.ReasoningContent = content
		} else {
			delta.Content = content
		}
		if firstText {
			delta.Role = "assistant"
			firstText = false
		}
		writeSSE(w, flusher, ChatStreamChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{{
				Index: 0,
				Delta: delta,
			}},
		})
	}

	collectToolCall := func(tc ToolCall) error {
		if err := validateToolCall(tc); err != nil {
			return err
		}
		collectedToolCalls.Add(tc)
		return nil
	}

	flushParser := func(parser *ToolCallParser, reasoning bool) error {
		content, calls := parser.Feed("", true)
		emitContent(content, reasoning)
		for _, tc := range calls {
			if err := collectToolCall(tc); err != nil {
				return err
			}
		}
		return nil
	}

	emitCollectedToolCalls := func() {
		for index := range collectedToolCalls.kept {
			tc := &collectedToolCalls.kept[index]
			delta := StreamDelta{ToolCalls: []StreamToolCall{{
				Index:    index,
				ID:       tc.ID,
				Type:     tc.Type,
				Function: &tc.Function,
			}}}
			if firstText {
				delta.Role = "assistant"
				firstText = false
			}
			writeSSE(w, flusher, ChatStreamChunk{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{{Index: 0, Delta: delta}},
			})
		}
	}

	// finishStream is the commit point: provisional calls have now had a chance
	// to be replaced by authoritative events and can be emitted exactly once.
	finishStream := func(reason string, usageInfo Usage, truncated bool) error {
		if done || finishing {
			return nil
		}
		finishing = true
		if err := flushParser(reasoningParser, true); err != nil {
			return err
		}
		if err := flushParser(textParser, false); err != nil {
			return err
		}

		hasToolCalls := len(collectedToolCalls.kept) > 0
		if reason == "tool_calls" && !hasToolCalls {
			return fmt.Errorf("finish reason tool_calls contained no valid tool calls")
		}
		truncated = truncated || collectedToolCalls.hasUnsafeArguments()
		emitCollectedToolCalls()
		finish := resolveFinishReason(reason, hasToolCalls, truncated)
		writeSSE(w, flusher, ChatStreamChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{},
				FinishReason: &finish,
			}},
		})
		if includeUsage {
			writeSSE(w, flusher, ChatStreamChunk{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{},
				Usage:   &usageInfo,
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		done = true
		if debugMode {
			log.Printf("%s %s", colorize("[DEBUG]", ansiDim), colorize(">> [DONE]", ansiGreen))
		}
		flusher.Flush()
		return nil
	}

	failStream := func(code, message string) {
		if done {
			return
		}
		writeSSEError(w, flusher, code, message)
		fmt.Fprint(w, "data: [DONE]\n\n")
		done = true
		if debugMode {
			log.Printf("%s %s", colorize("[DEBUG]", ansiDim), colorize(">> [DONE]", ansiGreen))
		}
		flusher.Flush()
	}

	endKind, err := parseStreamEvents(resp, func(ev CCStreamEvent) error {
		if cfg.Debug {
			raw, _ := json.Marshal(ev)
			log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
		}
		events, err := normalizer.Consume(ev)
		if err != nil {
			return err
		}
		for _, event := range events {
			switch event.kind {
			case normalizedText:
				content, calls := textParser.Feed(event.text, false)
				emitContent(content, false)
				for _, call := range calls {
					if err := collectToolCall(call); err != nil {
						return err
					}
				}
			case normalizedReasoning:
				content, calls := reasoningParser.Feed(event.text, false)
				emitContent(content, true)
				for _, call := range calls {
					if err := collectToolCall(call); err != nil {
						return err
					}
				}
			case normalizedToolCall:
				if event.toolCall != nil {
					if err := collectToolCall(*event.toolCall); err != nil {
						return err
					}
				}
			case normalizedReasoningEnd:
				if err := flushParser(reasoningParser, true); err != nil {
					return err
				}
			case normalizedTextEnd:
				if err := flushParser(textParser, false); err != nil {
					return err
				}
			case normalizedFinish:
				if err := finishStream(event.finishReason, event.usage, event.truncated); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s stream parse: %v", colorize("[ERROR]", ansiRed), err)
		failStream("upstream_stream_error", "upstream stream error: "+err.Error())
	} else if !done {
		message := "upstream connection closed before a finish event"
		if endKind == streamEndDone {
			message = "upstream sent [DONE] before a finish event"
		}
		log.Printf("%s %s", colorize("[ERROR]", ansiRed), message)
		failStream("upstream_stream_incomplete", message)
	}

	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.Usage()
	usage.RecordFor(account, promptTokens, completionTokens, cacheRead, cacheWrite)
}

func handleNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	handleNonStreamForAccount(w, resp, model, usage, cfg, "")
}

func handleNonStreamForAccount(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, account string) {
	msg := Message{Role: "assistant"}
	var toolCalls toolCallDeduper
	normalizer := newCCEventNormalizer()
	var textContent strings.Builder
	var reasoningContent strings.Builder
	var finishReason string
	var truncated bool
	addToolCall := func(call ToolCall) error {
		if err := validateToolCall(call); err != nil {
			return err
		}
		toolCalls.Add(call)
		return nil
	}

	endKind, err := parseStreamEvents(resp, func(ev CCStreamEvent) error {
		if cfg.Debug {
			raw, _ := json.Marshal(ev)
			log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
		}
		events, err := normalizer.Consume(ev)
		if err != nil {
			return err
		}
		for _, event := range events {
			switch event.kind {
			case normalizedText:
				textContent.WriteString(event.text)
			case normalizedReasoning:
				reasoningContent.WriteString(event.text)
			case normalizedToolCall:
				if event.toolCall != nil {
					if err := addToolCall(*event.toolCall); err != nil {
						return err
					}
				}
			case normalizedFinish:
				finishReason = event.finishReason
				truncated = event.truncated
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s non-stream parse: %v", colorize("[ERROR]", ansiRed), err)
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "upstream_stream_error", "upstream stream error: "+err.Error())
		return
	}
	if !normalizer.finished {
		message := "upstream connection closed before a finish event"
		if endKind == streamEndDone {
			message = "upstream sent [DONE] before a finish event"
		}
		log.Printf("%s %s", colorize("[ERROR]", ansiRed), message)
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "upstream_stream_incomplete", message)
		return
	}

	// Extract embedded tool calls only after the upstream turn completed.
	visibleText := textContent.String()
	if visibleText != "" {
		tcp := NewToolCallParser()
		strippedContent, parsedCalls := tcp.Feed(visibleText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				if err := addToolCall(call); err != nil {
					writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", err.Error())
					return
				}
			}
			visibleText = strippedContent
		}
	}

	// Also parse reasoning text for embedded tool calls
	reasoningText := reasoningContent.String()
	if reasoningText != "" {
		tcp := NewToolCallParser()
		strippedReasoning, parsedCalls := tcp.Feed(reasoningText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				if err := addToolCall(call); err != nil {
					writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", err.Error())
					return
				}
			}
			reasoningText = strippedReasoning
		}
	}

	msg.Content = TextContent(visibleText)
	msg.ToolCalls = toolCalls.kept
	if finishReason == "tool_calls" && len(msg.ToolCalls) == 0 {
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", "finish reason tool_calls contained no valid tool calls")
		return
	}
	truncated = truncated || toolCalls.hasUnsafeArguments()
	finishReason = resolveFinishReason(finishReason, len(msg.ToolCalls) > 0, truncated)
	if reasoningText != "" {
		msg.ReasoningContent = reasoningText
	}
	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.FinalUsage()
	usage.RecordFor(account, promptTokens, completionTokens, cacheRead, cacheWrite)

	res := ChatResponse{
		ID:      genStreamID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: normalizer.FinalUsageInfo(),
	}

	if cfg.Debug {
		raw, _ := json.Marshal(res)
		log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> response", ansiGreen), colorize(string(raw), ansiCyan))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// handleModels is a backward-compatible shim over handleModelsWithPolicy for
// callers (mainly tests) that don't care about model overrides: it builds a
// ModelPolicy from cfg once, at handler construction time, exactly
// mirroring the exclude_models-only behavior this function had before
// ModelPolicy existed.
func handleModels(cfg *Config) http.HandlerFunc {
	return handleModelsWithPolicy(NewModelPolicy(cfg))
}

func handleModelsWithPolicy(policy *ModelPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		filtered := make([]ModelInfo, 0, len(modelCatalog))
		for _, m := range modelCatalog {
			if policy.Enabled(m.ID) {
				filtered = append(filtered, m)
			}
		}
		json.NewEncoder(w).Encode(ModelList{Object: "list", Data: filtered})
	}
}

// ====================== helpers ======================

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeErrorWithCode(w, status, typ, "", msg)
}

func writeErrorWithCode(w http.ResponseWriter, status int, typ, code, msg string) {
	if debugMode {
		log.Printf("%s %s %d %s: %s", colorize("[DEBUG]", ansiDim), colorize(">> error", ansiRed), status, typ, msg)
	}
	errorBody := map[string]any{
		"message": msg,
		"type":    typ,
		"param":   nil,
	}
	if code != "" {
		errorBody["code"] = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": errorBody})
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, chunk ChatStreamChunk) {
	data, _ := json.Marshal(chunk)
	if debugMode {
		log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> sse", ansiGreen), colorize(string(data), ansiCyan))
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string) {
	payload := map[string]any{"error": map[string]any{
		"message": message,
		"type":    "server_error",
		"code":    code,
		"param":   nil,
	}}
	data, _ := json.Marshal(payload)
	if debugMode {
		log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> sse error", ansiRed), colorize(string(data), ansiCyan))
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func streamEventText(ev CCStreamEvent) string {
	if ev.Text != "" {
		return ev.Text
	}
	return ev.Delta
}

// resolveFinishReason picks the OpenAI finish_reason for a completed turn.
//
// "tool_calls" tells the client the assistant produced a complete set of calls
// it may now execute. That claim is false when the upstream hit its output cap:
// the last tool's arguments were cut off mid-JSON and only survive because
// repairOrFallbackToolInput patched them into valid syntax. Reporting "length"
// keeps the truncation visible so the client can retry or refuse instead of
// executing a silently truncated write.
func resolveFinishReason(upstream string, hasToolCalls, truncated bool) string {
	if truncated {
		return "length"
	}
	if upstream == "content_filter" {
		return "content_filter"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return upstream
}

func normalizeFinishReason(reason string) (string, error) {
	switch strings.TrimSpace(reason) {
	case "stop", "end", "end_turn":
		return "stop", nil
	case "tool-calls", "tool_calls", "tool_use", "function_call":
		return "tool_calls", nil
	case "max_tokens", "max_output_tokens", "length":
		return "length", nil
	case "content_filter":
		return "content_filter", nil
	case "":
		return "", fmt.Errorf("finish event missing finish reason")
	default:
		return "", fmt.Errorf("unsupported finish reason %q", reason)
	}
}

func genStreamID() string {
	id, err := randomHex(18)
	if err != nil {
		panic(err)
	}
	return "chatcmpl-" + id
}
