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
			log.Printf("[DEBUG] >> POST /v1/chat/completions body: %s", string(bodyBytes))
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
		if len(req.Messages) == 0 {
			writeError(w, 400, "invalid_request_error", "messages is required")
			return
		}

		resp, err := cc.Send(&req)
		if err != nil {
			log.Printf("[ERROR] cc send: %v", err)
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

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		switch ev.Type {
		case "text-delta":
			delta := StreamDelta{Content: ev.Text}
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
			delta := StreamDelta{ReasoningContent: ev.Text}
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

		case "finish", "finish-step":
			reason := ev.FinishReason
			if reason == "tool-calls" {
				reason = "tool_calls"
			}
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
				usageInfo.TotalTokens = promptTokens + completionTokens + reasoningTokens
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

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		case "error":
			log.Printf("[ERROR] cc stream event: %v", ev.Error)
			return fmt.Errorf("cc stream error")

		case "start", "start-step", "reasoning-start", "reasoning-end",
			"text-start", "text-end", "provider-metadata":
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("[DEBUG] << cc event type=%q raw=%s", ev.Type, string(raw))
			}

		default:
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("[DEBUG] << cc unknown event type=%q raw=%s", ev.Type, string(raw))
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("[ERROR] stream parse: %v", err)
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
			found := false
			switch parts := msg.Content.(type) {
			case []ContentPart:
				for i := len(parts) - 1; i >= 0; i-- {
					if parts[i].Type == "text" {
						parts[i].Text += ev.Text
						found = true
						break
					}
				}
				if !found {
					parts = append(parts, ContentPart{Type: "text", Text: ev.Text})
					msg.Content = parts
				}
			case nil:
				msg.Content = []ContentPart{{Type: "text", Text: ev.Text}}
			case string:
				msg.Content = parts + ev.Text
			default:
				msg.Content = ev.Text
			}
		case "reasoning-delta":
			reasoningContent += ev.Text
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
		case "finish", "finish-step":
			if ev.FinishReason == "tool-calls" {
				finishReason = "tool_calls"
			} else {
				finishReason = ev.FinishReason
			}
			if ev.TotalUsage != nil {
				promptTokens = ev.TotalUsage.InputTokens
				completionTokens = ev.TotalUsage.OutputTokens
				if ev.TotalUsage.InputTokenDetails != nil {
					cacheRead = ev.TotalUsage.InputTokenDetails.CacheReadTokens
					cacheWrite = ev.TotalUsage.InputTokenDetails.CacheWriteTokens
				}
			}
		case "start", "start-step", "reasoning-start", "reasoning-end",
			"text-start", "text-end", "provider-metadata":
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("[DEBUG] << cc event type=%q raw=%s", ev.Type, string(raw))
			}

		default:
			if cfg.Debug {
				raw, _ := json.Marshal(ev)
				log.Printf("[DEBUG] << cc unknown event type=%q raw=%s", ev.Type, string(raw))
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("[ERROR] non-stream parse: %v", err)
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
			PromptTokens:       promptTokens,
			CompletionTokens:   completionTokens,
			TotalTokens:        promptTokens + completionTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelList{Object: "list", Data: modelCatalog})
}

// ====================== helpers ======================

func writeError(w http.ResponseWriter, status int, typ, msg string) {
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
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func genStreamID() string {
	id, err := randomHex(18)
	if err != nil {
		panic(err)
	}
	return "chatcmpl-" + id
}
