package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const maxChatRequestBytes = 50 * 1024 * 1024

var debugMode bool

func handleChatCompletions(cc *CCClient, cfg *Config, usage *UsageTracker) http.HandlerFunc {
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

		if req.Model == "" {
			writeError(w, 400, "invalid_request_error", "model is required")
			return
		}
		if isModelExcluded(req.Model, cfg.ExcludeModels) {
			writeError(w, 404, "invalid_request_error", fmt.Sprintf("model %q is not available", req.Model))
			return
		}
		if len(req.Messages) == 0 {
			writeError(w, 400, "invalid_request_error", "messages is required")
			return
		}

		resp, err := cc.Send(r.Context(), &req)
		if err != nil {
			var invalid *invalidRequestError
			if errors.As(err, &invalid) {
				writeError(w, http.StatusBadRequest, "invalid_request_error", invalid.Error())
				return
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
				return
			}
			log.Printf("%s cc send: %v", colorize("[ERROR]", ansiRed), err)
			writeError(w, http.StatusBadGateway, "server_error", "upstream error: "+err.Error())
			return
		}

		if req.Stream {
			handleStream(w, resp, req.Model, usage, cfg)
		} else {
			handleNonStream(w, resp, req.Model, usage, cfg)
		}
		if err := usage.save(); err != nil {
			log.Printf("%s save usage: %v", colorize("[ERROR]", ansiRed), err)
		}
	}
}

func handleStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "server_error", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	firstText := true
	var done bool     // [DONE] has been written; nothing more may be emitted
	var finishing bool // finishStream is running its final flush

	normalizer := newCCEventNormalizer()
	textParser := NewToolCallParser()
	reasoningParser := NewToolCallParser()
	hasToolCalls := false
	var emittedToolCalls toolCallDeduper
	toolCallIndex := 0

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
			ID:     genStreamID(),
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []StreamChoice{{
				Index: 0,
				Delta: delta,
			}},
		})
	}

	emitToolCall := func(tc ToolCall) {
		if done {
			// A stray event after [DONE] must not be written past it, where no
			// OpenAI client could read it. The final flush (finishing=true,
			// done=false) is still permitted.
			log.Printf("%s tool call %q arrived after the stream was terminated; dropping it",
				colorize("[WARN]", ansiYellow), tc.ID)
			return
		}
		if !emittedToolCalls.Add(tc) {
			return
		}
		delta := StreamDelta{ToolCalls: []StreamToolCall{{
			Index:    toolCallIndex,
			ID:       tc.ID,
			Type:     tc.Type,
			Function: &tc.Function,
		}}}
		toolCallIndex++
		if firstText {
			delta.Role = "assistant"
			firstText = false
		}
		writeSSE(w, flusher, ChatStreamChunk{
			ID:     genStreamID(),
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []StreamChoice{{
				Index: 0,
				Delta: delta,
			}},
		})
		hasToolCalls = true
	}

	flushParser := func(parser *ToolCallParser, reasoning bool) {
		content, calls := parser.Feed("", true)
		emitContent(content, reasoning)
		for _, tc := range calls {
			emitToolCall(tc)
		}
	}

	// finishStream emits everything still held back, then the terminating chunk
	// and [DONE]. It runs for a normal finish event and again after the read
	// loop returns, so a stream the upstream abandons mid-tool-call still
	// delivers that call and still terminates in a shape OpenAI clients accept.
	finishStream := func(reason string, usageInfo Usage, truncated bool) {
		if done || finishing {
			return
		}
		finishing = true
		flushParser(reasoningParser, true)
		flushParser(textParser, false)

		finish := resolveFinishReason(reason, hasToolCalls, truncated)
		writeSSE(w, flusher, ChatStreamChunk{
			ID:     genStreamID(),
			Object: "chat.completion.chunk",
			Model:  model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{},
				FinishReason: &finish,
			}},
			Usage: &usageInfo,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		done = true
		if debugMode {
			log.Printf("%s %s", colorize("[DEBUG]", ansiDim), colorize(">> [DONE]", ansiGreen))
		}
		flusher.Flush()
	}

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
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
					emitToolCall(call)
				}
			case normalizedReasoning:
				content, calls := reasoningParser.Feed(event.text, false)
				emitContent(content, true)
				for _, call := range calls {
					emitToolCall(call)
				}
			case normalizedToolCall:
				if event.toolCall != nil {
					emitToolCall(*event.toolCall)
				}
			case normalizedReasoningEnd:
				flushParser(reasoningParser, true)
			case normalizedTextEnd:
				flushParser(textParser, false)
			case normalizedFinish:
				finishStream(event.finishReason, event.usage, event.truncated)
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s stream parse: %v", colorize("[ERROR]", ansiRed), err)
	}

	// The upstream can stop without ever sending finish — a dropped connection,
	// a mid-stream error, or a truncated response. Recover whatever it already
	// told us rather than leaving the client with a stream that just stops.
	if !done {
		// The turn did not complete. "length" is the OpenAI value for a response
		// that was cut off; reporting "stop" would repeat the lie that a partial
		// turn finished cleanly.
		reason := "stop"
		if err != nil {
			reason = "length"
		}
		drained, drainErr := normalizer.drainToolInputs()
		if drainErr != nil {
			log.Printf("%s drain tool inputs: %v", colorize("[ERROR]", ansiRed), drainErr)
		}
		for _, event := range drained {
			if event.kind == normalizedToolCall && event.toolCall != nil {
				emitToolCall(*event.toolCall)
			}
		}
		if err != nil || len(drained) > 0 {
			log.Printf("%s stream ended without a finish event; terminating with %d recovered tool call(s)",
				colorize("[WARN]", ansiYellow), len(drained))
		}
		finishStream(reason, normalizer.FinalUsageInfo(), normalizer.truncated)
	}

	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.Usage()
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)
}

func handleNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	msg := Message{Role: "assistant"}
	var toolCalls toolCallDeduper
	normalizer := newCCEventNormalizer()
	var textContent strings.Builder
	var reasoningContent strings.Builder
	var finishReason string
	var truncated bool

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
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
					toolCalls.Add(*event.toolCall)
				}
			case normalizedFinish:
				finishReason = event.finishReason
				truncated = event.truncated
			}
		}
		return nil
	})

	// Recover anything the upstream buffered but never terminated, exactly as
	// the streaming path does.
	if !normalizer.finished {
		drained, drainErr := normalizer.drainToolInputs()
		if drainErr != nil {
			log.Printf("%s drain tool inputs: %v", colorize("[ERROR]", ansiRed), drainErr)
		}
		for _, event := range drained {
			if event.kind == normalizedToolCall && event.toolCall != nil {
				toolCalls.Add(*event.toolCall)
			}
		}
		truncated = truncated || normalizer.truncated
	}

	// Extract text content and parse embedded tool calls. This must run before
	// the error check below: a call the model wrote as text inside its reasoning
	// only becomes visible here, and discarding the turn before parsing it would
	// lose it.
	visibleText := textContent.String()
	if visibleText != "" {
		tcp := NewToolCallParser()
		strippedContent, parsedCalls := tcp.Feed(visibleText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				toolCalls.Add(call)
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
				toolCalls.Add(call)
			}
			reasoningText = strippedReasoning
		}
	}

	if err != nil {
		log.Printf("%s non-stream parse: %v", colorize("[ERROR]", ansiRed), err)
		// Discarding a partial turn wholesale loses tool calls the upstream
		// already delivered in full — including ones embedded in reasoning.
		// Return what survived and mark it truncated; only a turn with nothing
		// at all in it is a failed request.
		if len(toolCalls.kept) == 0 && strings.TrimSpace(visibleText) == "" && strings.TrimSpace(reasoningText) == "" {
			writeError(w, 502, "server_error", "upstream stream error")
			return
		}
		truncated = true
	}

	msg.Content = TextContent(visibleText)
	msg.ToolCalls = toolCalls.kept
	finishReason = resolveFinishReason(finishReason, len(msg.ToolCalls) > 0, truncated)
	if reasoningText != "" {
		msg.ReasoningContent = reasoningText
	}
	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.FinalUsage()
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)

	res := ChatResponse{
		ID:     genStreamID(),
		Object: "chat.completion",
		Model:  model,
		Choices: []Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      normalizer.FinalUsageInfo().TotalTokens,
		},
	}

	if cfg.Debug {
		raw, _ := json.Marshal(res)
		log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> response", ansiGreen), colorize(string(raw), ansiCyan))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func handleModels(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		filtered := make([]ModelInfo, 0, len(modelCatalog))
		for _, m := range modelCatalog {
			if !isModelExcluded(m.ID, cfg.ExcludeModels) {
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
	if upstream == "length" || truncated {
		return "length"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return upstream
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool-calls":
		return "tool_calls"
	case "max_tokens", "max_output_tokens":
		return "length"
	default:
		return reason
	}
}

func genStreamID() string {
	id, err := randomHex(18)
	if err != nil {
		panic(err)
	}
	return "chatcmpl-" + id
}
