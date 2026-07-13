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
				writeError(w, 400, "invalid_request_error", invalid.Error())
				return
			}
			log.Printf("%s cc send: %v", colorize("[ERROR]", ansiRed), err)
			writeError(w, 502, "server_error", "upstream error: "+err.Error())
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
	var done bool

	normalizer := newCCEventNormalizer()
	textParser := NewToolCallParser()
	reasoningParser := NewToolCallParser()
	hasToolCalls := false
	emittedToolCalls := make(map[string]bool)
	toolCallIndex := 0

	emitContent := func(content string, reasoning bool) {
		if content == "" {
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
		key := toolCallKey(tc)
		if emittedToolCalls[key] {
			return
		}
		emittedToolCalls[key] = true
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
				if done {
					continue
				}
				flushParser(reasoningParser, true)
				flushParser(textParser, false)

				finish := event.finishReason
				if hasToolCalls {
					finish = "tool_calls"
				}
				usageInfo := event.usage
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

				done = true
				fmt.Fprintf(w, "data: [DONE]\n\n")
				if debugMode {
					log.Printf("%s %s", colorize("[DEBUG]", ansiDim), colorize(">> [DONE]", ansiGreen))
				}
				flusher.Flush()
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s stream parse: %v", colorize("[ERROR]", ansiRed), err)
	}
	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.Usage()
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)
}

func handleNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	msg := Message{Role: "assistant"}
	normalizer := newCCEventNormalizer()
	var textContent strings.Builder
	var reasoningContent strings.Builder
	var finishReason string

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
					msg.ToolCalls = appendUniqueToolCall(msg.ToolCalls, *event.toolCall)
				}
			case normalizedFinish:
				finishReason = event.finishReason
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s non-stream parse: %v", colorize("[ERROR]", ansiRed), err)
		writeError(w, 502, "server_error", "upstream stream error")
		return
	}

	// Extract text content and parse embedded tool calls
	visibleText := textContent.String()
	if visibleText != "" {
		tcp := NewToolCallParser()
		strippedContent, parsedCalls := tcp.Feed(visibleText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				msg.ToolCalls = appendUniqueToolCall(msg.ToolCalls, call)
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
				msg.ToolCalls = appendUniqueToolCall(msg.ToolCalls, call)
			}
			reasoningText = strippedReasoning
		}
	}

	msg.Content = TextContent(visibleText)
	if len(msg.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
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
	if debugMode {
		log.Printf("%s %s %d %s: %s", colorize("[DEBUG]", ansiDim), colorize(">> error", ansiRed), status, typ, msg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
		},
	})
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
