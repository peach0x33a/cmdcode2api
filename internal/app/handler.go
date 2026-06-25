package app

import (
	"bytes"
	"encoding/json"
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

		resp, err := cc.Send(&req)
		if err != nil {
			log.Printf("%s cc send: %v", colorize("[ERROR]", ansiRed), err)
			writeError(w, 502, "server_error", "upstream error: "+err.Error())
			return
		}

		if req.Stream {
			handleStream(w, resp, req.Model, usage, cfg)
			usage.save()
		} else {
			handleNonStream(w, resp, req.Model, usage, cfg)
			usage.save()
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
	var promptTokens, completionTokens, cacheRead, cacheWrite int
	var reasoningTokens int
	var done bool

	tcp := NewToolCallParser()
	parsedToolCallFromText := false
	structuredToolCallIDs := make(map[string]bool)
	toolCallIndex := 0
	toolInputBuf := make(map[string]string)      // accumulated JSON per tool call ID
	toolInputToolName := make(map[string]string) // tool name per ID

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		switch ev.Type {
		case "text-delta":
			text := streamEventText(ev)
			content, calls := tcp.Feed(text, false)
			if content != "" || len(calls) > 0 {
				if content != "" {
					delta := StreamDelta{Content: content}
					if firstText {
						delta.Role = "assistant"
						firstText = false
					}
					chunk := ChatStreamChunk{
						ID:     genStreamID(),
						Object: "chat.completion.chunk",
						Model:  model,
						Choices: []StreamChoice{{
							Index: 0,
							Delta: delta,
						}},
					}
					writeSSE(w, flusher, chunk)
				}
				for _, tc := range calls {
					if structuredToolCallIDs[tc.ID] {
						continue
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
					chunk := ChatStreamChunk{
						ID:     genStreamID(),
						Object: "chat.completion.chunk",
						Model:  model,
						Choices: []StreamChoice{{
							Index: 0,
							Delta: delta,
						}},
					}
					writeSSE(w, flusher, chunk)
					parsedToolCallFromText = true
				}
			}

		case "reasoning-delta":
			reasoningTokens++
			text := streamEventText(ev)
			content, calls := tcp.Feed(text, false)
			if content != "" {
				delta := StreamDelta{ReasoningContent: content}
				if firstText {
					delta.Role = "assistant"
					firstText = false
				}
				chunk := ChatStreamChunk{
					ID:     genStreamID(),
					Object: "chat.completion.chunk",
					Model:  model,
					Choices: []StreamChoice{{
						Index: 0,
						Delta: delta,
					}},
				}
				writeSSE(w, flusher, chunk)
			}
			for _, tc := range calls {
				if structuredToolCallIDs[tc.ID] {
					continue
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
				chunk := ChatStreamChunk{
					ID:     genStreamID(),
					Object: "chat.completion.chunk",
					Model:  model,
					Choices: []StreamChoice{{
						Index: 0,
						Delta: delta,
					}},
				}
				writeSSE(w, flusher, chunk)
				parsedToolCallFromText = true
			}

		case "tool-call":
			structuredToolCallIDs[ev.ToolCallID] = true
			input := ev.Input
			if input == nil {
				input = ev.Args
			}
			if input == nil {
				input = ev.Arguments
			}
			argsJSON, _ := json.Marshal(input)
			chunk := ChatStreamChunk{
				ID:     genStreamID(),
				Object: "chat.completion.chunk",
				Model:  model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: StreamDelta{ToolCalls: []StreamToolCall{{
						Index: toolCallIndex,
						ID:    ev.ToolCallID,
						Type:  "function",
						Function: &CallFunc{
							Name:      ev.ToolName,
							Arguments: string(argsJSON),
						},
					}}},
				}},
			}
			writeSSE(w, flusher, chunk)
			toolCallIndex++

		case "finish-step":
			if ev.Usage != nil {
				promptTokens = ev.Usage.InputTokens
				completionTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokenDetails != nil {
					cacheRead = ev.Usage.InputTokenDetails.CacheReadTokens
					cacheWrite = ev.Usage.InputTokenDetails.CacheWriteTokens
				}
			}

		case "finish":
			reason := normalizeFinishReason(ev.FinishReason)
			finish := reason

			// Flush remaining buffered text through the tool-call parser.
			flushContent, flushCalls := tcp.Feed("", true)
			if flushContent != "" {
				delta := StreamDelta{Content: flushContent}
				if firstText {
					delta.Role = "assistant"
					firstText = false
				}
				chunk := ChatStreamChunk{
					ID:     genStreamID(),
					Object: "chat.completion.chunk",
					Model:  model,
					Choices: []StreamChoice{{
						Index: 0,
						Delta: delta,
					}},
				}
				writeSSE(w, flusher, chunk)
			}
			for _, tc := range flushCalls {
				if structuredToolCallIDs[tc.ID] {
					continue
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
				chunk := ChatStreamChunk{
					ID:     genStreamID(),
					Object: "chat.completion.chunk",
					Model:  model,
					Choices: []StreamChoice{{
						Index: 0,
						Delta: delta,
					}},
				}
				writeSSE(w, flusher, chunk)
				parsedToolCallFromText = true
			}

			usageInfo := &Usage{}
			if ev.TotalUsage != nil {
				promptTokens = ev.TotalUsage.InputTokens
				completionTokens = ev.TotalUsage.OutputTokens
				if ev.TotalUsage.InputTokenDetails != nil {
					cacheRead = ev.TotalUsage.InputTokenDetails.CacheReadTokens
					cacheWrite = ev.TotalUsage.InputTokenDetails.CacheWriteTokens
				}
				usageInfo.PromptTokens = promptTokens
				usageInfo.CompletionTokens = completionTokens
				if ev.TotalUsage.TotalTokens > 0 {
					usageInfo.TotalTokens = ev.TotalUsage.TotalTokens
				} else {
					usageInfo.TotalTokens = promptTokens + completionTokens
				}
			}

			// If tool calls were parsed from text-delta events, override
			// the finish reason so the client knows to expect tool results.
			if parsedToolCallFromText {
				finish = "tool_calls"
			}

			chunk := ChatStreamChunk{
				ID:     genStreamID(),
				Object: "chat.completion.chunk",
				Model:  model,
				Choices: []StreamChoice{{
					Index:        0,
					Delta:        StreamDelta{},
					FinishReason: &finish,
				}},
				Usage: usageInfo,
			}
			writeSSE(w, flusher, chunk)

			// OpenAI SSE protocol: `data: [DONE]` must appear exactly once
			// per response. finish-step may fire repeatedly (one per agent
			// step); only finish terminates the stream.
			if !done {
				done = true
				fmt.Fprintf(w, "data: [DONE]\n\n")
				if debugMode {
					log.Printf("%s %s", colorize("[DEBUG]", ansiDim), colorize(">> [DONE]", ansiGreen))
				}
				flusher.Flush()
			}

		case "error":
			log.Printf("%s stream event: %v", colorize("[ERROR]", ansiRed), ev.Error)
			return fmt.Errorf("cc stream error")

		case "start", "start-step", "reasoning-start", "reasoning-end",
			"text-start", "text-end", "provider-metadata", "tool-result":
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
			}

		case "tool-input-start":
			toolInputBuf[ev.ID] = ""
			toolInputToolName[ev.ID] = ev.ToolName
			if cfg.Debug {
				log.Printf("%s %s tool-input-start id=%s tool=%s",
					colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.ID, ev.ToolName)
			}

		case "tool-input-delta":
			if cur, ok := toolInputBuf[ev.ID]; ok {
				toolInputBuf[ev.ID] = cur + ev.Delta
			}
			if cfg.Debug {
				log.Printf("%s %s tool-input-delta id=%s delta=%q",
					colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.ID, ev.Delta)
			}

		case "tool-input-end", "tool-input-available":
			args, ok := toolInputBuf[ev.ID]
			toolName, nameOk := toolInputToolName[ev.ID]
			if ok && nameOk && args != "" {
				var v any
				if err := json.Unmarshal([]byte(args), &v); err == nil {
					if !structuredToolCallIDs[ev.ID] {
						structuredToolCallIDs[ev.ID] = true
						chunk := ChatStreamChunk{
							ID:     genStreamID(),
							Object: "chat.completion.chunk",
							Model:  model,
							Choices: []StreamChoice{{
								Index: 0,
								Delta: StreamDelta{ToolCalls: []StreamToolCall{{
									Index: toolCallIndex,
									ID:    ev.ID,
									Type:  "function",
									Function: &CallFunc{
										Name:      toolName,
										Arguments: args,
									},
								}}},
							}},
						}
						toolCallIndex++
						writeSSE(w, flusher, chunk)
						parsedToolCallFromText = true
					}
				}
			}
			// clean up to avoid unbounded memory growth
			delete(toolInputBuf, ev.ID)
			delete(toolInputToolName, ev.ID)
			if cfg.Debug {
				log.Printf("%s %s tool-input-end id=%s name=%s args=%q",
					colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.ID, toolName, args)
			}

		default:
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc ?", ansiYellow), ev.Type, colorize(string(raw), ansiYellow))
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s stream parse: %v", colorize("[ERROR]", ansiRed), err)
	}
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)
}

func handleNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config) {
	var msg Message
	msg.Role = "assistant"
	var promptTokens, completionTokens, cacheRead, cacheWrite int
	var finishReason string
	var reasoningContent string

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		switch ev.Type {
		case "text-delta":
			text := streamEventText(ev)
			found := false
			switch parts := msg.Content.(type) {
			case []ContentPart:
				for i := len(parts) - 1; i >= 0; i-- {
					if parts[i].Type == "text" {
						parts[i].Text += text
						found = true
						break
					}
				}
				if !found {
					parts = append(parts, ContentPart{Type: "text", Text: text})
					msg.Content = parts
				}
			case nil:
				msg.Content = []ContentPart{{Type: "text", Text: text}}
			case string:
				msg.Content = parts + text
			default:
				msg.Content = text
			}
		case "reasoning-delta":
			reasoningContent += streamEventText(ev)
		case "tool-call":
			input := ev.Input
			if input == nil {
				input = ev.Args
			}
			if input == nil {
				input = ev.Arguments
			}
			argsJSON, _ := json.Marshal(input)
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:   ev.ToolCallID,
				Type: "function",
				Function: CallFunc{
					Name:      ev.ToolName,
					Arguments: string(argsJSON),
				},
			})
		case "finish":
			finishReason = normalizeFinishReason(ev.FinishReason)
			if ev.TotalUsage != nil {
				promptTokens = ev.TotalUsage.InputTokens
				completionTokens = ev.TotalUsage.OutputTokens
				if ev.TotalUsage.InputTokenDetails != nil {
					cacheRead = ev.TotalUsage.InputTokenDetails.CacheReadTokens
					cacheWrite = ev.TotalUsage.InputTokenDetails.CacheWriteTokens
				}
			}
		case "start", "start-step", "reasoning-start", "reasoning-end",
			"text-start", "text-end", "finish-step", "provider-metadata",
			"tool-input-start", "tool-input-delta", "tool-input-end", "tool-input-available", "tool-result":
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
			}

		default:
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc ?", ansiYellow), ev.Type, colorize(string(raw), ansiYellow))
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s non-stream parse: %v", colorize("[ERROR]", ansiRed), err)
		writeError(w, 502, "server_error", "upstream stream error")
		return
	}

	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)

	// Extract text content and parse embedded tool calls
	var textContent string
	switch c := msg.Content.(type) {
	case string:
		textContent = c
	case []ContentPart:
		var sb strings.Builder
		for _, p := range c {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		textContent = sb.String()
	}
	if textContent != "" {
		tcp := NewToolCallParser()
		strippedContent, parsedCalls := tcp.Feed(textContent, true)
		if len(parsedCalls) > 0 {
			// Deduplicate against tool calls from structured events
			existingIDs := make(map[string]bool)
			for _, tc := range msg.ToolCalls {
				existingIDs[tc.ID] = true
			}
			for _, pc := range parsedCalls {
				if !existingIDs[pc.ID] {
					msg.ToolCalls = append(msg.ToolCalls, pc)
				}
			}
			msg.Content = strippedContent
			finishReason = normalizeFinishReason("tool_calls")
		}
	}

	// Also parse reasoning text for embedded tool calls
	if reasoningContent != "" {
		tcp := NewToolCallParser()
		strippedReasoning, parsedCalls := tcp.Feed(reasoningContent, true)
		if len(parsedCalls) > 0 {
			existingIDs := make(map[string]bool)
			for _, tc := range msg.ToolCalls {
				existingIDs[tc.ID] = true
			}
			for _, pc := range parsedCalls {
				if !existingIDs[pc.ID] {
					msg.ToolCalls = append(msg.ToolCalls, pc)
				}
			}
			reasoningContent = strippedReasoning
			finishReason = normalizeFinishReason("tool_calls")
		}
	}

	// 如果只有 text，展平 content
	if parts, ok := msg.Content.([]ContentPart); ok && len(parts) == 1 && parts[0].Type == "text" {
		msg.Content = parts[0].Text
	}
	if msg.Content == nil {
		msg.Content = ""
	}
	if reasoningContent != "" {
		msg.ReasoningContent = reasoningContent
	}

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
			TotalTokens:      promptTokens + completionTokens,
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
