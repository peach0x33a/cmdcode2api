package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

type CCClient struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func NewCCClient(apiKey, baseURL string) *CCClient {
	return &CCClient{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 600 * time.Second},
	}
}

// ConvertOpenAIToCC 把 OpenAI 格式的 ChatRequest 转成 CC 格式并发请求。
// 返回 HTTP response body，调用者负责解析 SSE 流。
func (c *CCClient) Send(req *ChatRequest) (*http.Response, error) {
	ccReq := openAIToCC(req)

	body, err := json.Marshal(ccReq)
	if err != nil {
		return nil, fmt.Errorf("marshal cc request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.BaseURL+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("x-command-code-version", "0.24.1")
	httpReq.Header.Set("x-cli-environment", "production")

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("cc api error %d: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}

// ParseStreamEvents 从 resp.Body 读取 SSE 流，逐事件回调 onEvent。
func ParseStreamEvents(resp *http.Response, onEvent func(CCStreamEvent) error) error {
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimPrefix(line, "data:")
			line = strings.TrimSpace(line)
		}
		if line == "[DONE]" {
			break
		}
		var ev CCStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return fmt.Errorf("parse sse data: %w", err)
		}
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ====================== 格式转换 ======================

func openAIToCC(req *ChatRequest) CCRequest {
	tools := toolsToCC(req.Tools)
	msgs := messagesToCC(req.Messages)
	system := extractSystem(req.Messages)

	cc := CCRequest{
		Config: CCConfig{
			WorkingDir:    "/",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   fmt.Sprintf("%s-%s, Go proxy", runtime.GOOS, runtime.GOARCH),
			Structure:     []string{},
			RecentCommits: []any{},
		},
		Memory:         "",
		Taste:          "",
		Skills:         nil,
		PermissionMode: "standard",
		Params: CCParams{
			Model:     req.Model,
			Messages:  msgs,
			Tools:     tools,
			System:    system,
			MaxTokens: req.MaxTokens,
			Stream:    true, // CC API 只支持流式
		},
	}
	if cc.Params.MaxTokens <= 0 {
		cc.Params.MaxTokens = 4096
	}
	if cc.Params.MaxTokens > 200000 {
		cc.Params.MaxTokens = 200000
	}
	return cc
}

func extractSystem(msgs []Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			switch v := m.Content.(type) {
			case string:
				parts = append(parts, v)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func messagesToCC(msgs []Message) []CCMsg {
	var out []CCMsg
	for _, m := range msgs {
		if m.Role == "system" {
			continue // 已提取到 top-level system
		}
		cc := CCMsg{Role: roleToCC(m.Role), Content: contentToCC(m)}
		out = append(out, cc)
	}
	return out
}

func roleToCC(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	default:
		return "user"
	}
}

func contentToCC(m Message) []CCPart {
	if m.Role == "tool" {
		text := textFromContent(m.Content)
		return []CCPart{{
			Type: "text",
			Text: fmt.Sprintf("Tool result from %s (%s):\n%s", m.Name, m.ToolCallID, text),
		}}
	}

	var parts []CCPart

	switch v := m.Content.(type) {
	case string:
		if v != "" {
			parts = append(parts, CCPart{Type: "text", Text: v})
		}
	case []any:
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			switch typ {
			case "text":
				text, _ := obj["text"].(string)
				if text != "" {
					parts = append(parts, CCPart{Type: "text", Text: text})
				}
			case "image_url":
				// OpenAI → Anthropic 格式
				img, _ := obj["image_url"].(map[string]any)
				url, _ := img["url"].(string)
				if url != "" {
					mediaType, data := parseDataURL(url)
					parts = append(parts, CCPart{
						Type: "image",
						Source: map[string]any{
							"type":       "base64",
							"media_type": mediaType,
							"data":       data,
						},
					})
				}
			}
		}
	}

	// 工具调用
	for _, tc := range m.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				parts = append(parts, CCPart{
					Type: "text",
					Text: fmt.Sprintf("Assistant requested tool %s (%s) with invalid arguments: %v", tc.Function.Name, tc.ID, err),
				})
				continue
			}
		}
		argsJSON, _ := json.Marshal(input)
		parts = append(parts, CCPart{
			Type: "text",
			Text: fmt.Sprintf("Assistant requested tool %s (%s) with arguments: %s", tc.Function.Name, tc.ID, string(argsJSON)),
		})
	}

	return parts
}

func textFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			if typ == "text" {
				t, _ := obj["text"].(string)
				return t
			}
		}
	}
	return ""
}

func toolsToCC(tools []Tool) []CCTool {
	if tools == nil {
		return []CCTool{}
	}
	out := make([]CCTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, CCTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// parseDataURL 解析 data:image/png;base64,XXXX，返回 media_type 和纯 base64 数据
func parseDataURL(url string) (string, string) {
	if !strings.HasPrefix(url, "data:") {
		return "image/png", url
	}
	after := strings.TrimPrefix(url, "data:")
	parts := strings.SplitN(after, ",", 2)
	if len(parts) != 2 {
		return "image/png", url
	}
	mediaType := strings.TrimSuffix(parts[0], ";base64")
	return mediaType, parts[1]
}
