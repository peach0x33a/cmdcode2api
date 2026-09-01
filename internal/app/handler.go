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
	"time"
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
				log.Printf("%s cc request failed: %v", colorize("[ERROR]", ansiRed), upstreamErr)
				if upstreamErr.RetryAfter != "" {
					w.Header().Set("Retry-After", upstreamErr.RetryAfter)
				}
				if upstreamErr.RequestID != "" {
					w.Header().Set("x-request-id", upstreamErr.RequestID)
				}
				writeErrorWithCode(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Code, upstreamErr.Message)
				return
			}
			log.Printf("%s cc request failed: %v", colorize("[ERROR]", ansiRed), err)
			writeError(w, http.StatusBadGateway, "server_error", "upstream error: "+err.Error())
			return
		}

		if req.Stream {
			includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
			handleStreamWithOptions(w, resp, req.Model, usage, cfg, includeUsage)
		} else {
			handleNonStream(w, resp, req.Model, usage, cfg)
		}
		if err := usage.save(); err != nil {
			log.Printf("%s save usage failed: %v", colorize("[ERROR]", ansiRed), err)
		}
	}
}

func handleStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	handleStreamWithOptions(w, resp, model, usage, cfg, false)
}

func handleStreamWithOptions(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, includeUsage bool) {
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

	// finishStream is the commit point: validated authoritative calls can now be
	// emitted exactly once. Provisional tool-input events never reach this set.
	finishStream := func(reason string, usageInfo Usage, truncated bool) error {
		if done || finishing {
			return nil
		}
		finishing = true

		hasToolCalls := len(collectedToolCalls.kept) > 0
		if reason == "tool_calls" && !hasToolCalls {
			return fmt.Errorf("finish reason tool_calls contained no valid tool calls")
		}
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
				emitContent(event.text, false)
			case normalizedReasoning:
				emitContent(event.text, true)
			case normalizedToolCall:
				if event.toolCall != nil {
					if err := collectToolCall(*event.toolCall); err != nil {
						return err
					}
				}
			case normalizedReasoningEnd, normalizedTextEnd:
				// End markers carry no content; text and reasoning deltas are
				// forwarded immediately and are never interpreted as tool syntax.
			case normalizedFinish:
				if err := finishStream(event.finishReason, event.usage, event.truncated); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s stream parse failed: %v", colorize("[ERROR]", ansiRed), err)
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
		log.Printf("%s non-stream parse failed: %v", colorize("[ERROR]", ansiRed), err)
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

	visibleText := textContent.String()
	reasoningText := reasoningContent.String()

	msg.Content = TextContent(visibleText)
	msg.ToolCalls = toolCalls.kept
	if finishReason == "tool_calls" && len(msg.ToolCalls) == 0 {
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", "finish reason tool_calls contained no valid tool calls")
		return
	}
	finishReason = resolveFinishReason(finishReason, len(msg.ToolCalls) > 0, truncated)
	if reasoningText != "" {
		msg.ReasoningContent = reasoningText
	}
	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.FinalUsage()
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)

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
// "tool_calls" tells the client the assistant produced a complete set of
// validated structured calls. A length finish always wins because an upstream
// output cap means the turn is incomplete.
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
