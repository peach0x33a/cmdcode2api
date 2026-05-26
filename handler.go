package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func handleChatCompletions(cc *CCClient, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid_request_error", "bad request body: "+err.Error())
			return
		}

		if req.Model == "" {
			req.Model = cfg.DefaultModel
		}

		resp, err := cc.Send(&req)
		if err != nil {
			log.Printf("[ERROR] cc send: %v", err)
			writeError(w, 502, "server_error", "upstream error: "+err.Error())
			return
		}

		if req.Stream {
			handleStream(w, resp, req.Model, usage)
			usage.save()
		} else {
			handleNonStream(w, resp, req.Model, usage)
			usage.save()
		}
	}
}

func handleStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker) {
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

		case "finish":
			reason := ev.FinishReason
			// CC 的 finishReason 可能是 "tool-calls" → 转为 OpenAI 的 "tool_calls"
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
				usageInfo.TotalTokens = promptTokens + completionTokens
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

			// 最后发 [DONE]
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		case "error":
			log.Printf("[ERROR] cc stream event: %v", ev.Error)
			return fmt.Errorf("cc stream error")
		}
		return nil
	})

	if err != nil {
		log.Printf("[ERROR] stream parse: %v", err)
	}
	usage.Record(promptTokens, completionTokens, cacheRead, cacheWrite)
}

func handleNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker) {
	var msg Message
	msg.Role = "assistant"
	var promptTokens, completionTokens, cacheRead, cacheWrite int
	var finishReason string

	err := ParseStreamEvents(resp, func(ev CCStreamEvent) error {
		switch ev.Type {
		case "text-delta":
			// 往最后一个 text content 追加
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
			}
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	models := []ModelInfo{
		// Premium
		{ID: "claude-opus-4-7", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "claude-opus-4-6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "claude-sonnet-4-6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "claude-haiku-4-5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gpt-5.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gpt-5.4", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gpt-5.3-codex", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gpt-5.4-mini", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gemini-3.5-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "gemini-3.1-flash-lite", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		// Open-source
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "zai-org/GLM-5.1", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "zai-org/GLM-5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "Qwen/Qwen3.7-Max", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
		{ID: "step-3.5-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelList{Object: "list", Data: models})
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
	b := make([]byte, 18)
	rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

func init() {
	// 确保随机数可用
	_ = time.Now().UnixNano()
}
