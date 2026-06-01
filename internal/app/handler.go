package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		switch ev.Type {
		case "text-delta":
			delta := StreamDelta{Content: streamEventText(ev)}
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

		case "reasoning-delta":
			reasoningTokens++
			delta := StreamDelta{ReasoningContent: streamEventText(ev)}
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

		case "tool-call":
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
					Delta: StreamDelta{ToolCalls: []ToolCall{{
						ID:   ev.ToolCallID,
						Type: "function",
						Function: CallFunc{
							Name:      ev.ToolName,
							Arguments: string(argsJSON),
						},
					}}},
				}},
			}
			writeSSE(w, flusher, chunk)

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
			"text-start", "text-end", "provider-metadata",
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
