package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type CCClient struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

type invalidRequestError struct {
	message string
}

func (e *invalidRequestError) Error() string {
	return e.message
}

type upstreamAPIError struct {
	Status     int
	Message    string
	Type       string
	Code       string
	RetryAfter string
	RequestID  string
}

func (e *upstreamAPIError) Error() string {
	return fmt.Sprintf("cc api error %d: %s", e.Status, e.Message)
}

func normalizeUpstreamError(status int, body []byte, header http.Header) *upstreamAPIError {
	type rateLimitInfo struct {
		Reset float64 `json:"reset"`
	}
	var payload struct {
		Message   string        `json:"message"`
		Type      string        `json:"type"`
		Code      string        `json:"code"`
		RateLimit rateLimitInfo `json:"rateLimit"`
		Error     struct {
			Message   string        `json:"message"`
			Type      string        `json:"type"`
			Code      string        `json:"code"`
			RateLimit rateLimitInfo `json:"rateLimit"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)

	message, code := payload.Message, payload.Code
	reset := payload.RateLimit.Reset
	if payload.Error.Message != "" {
		message = payload.Error.Message
	}
	if payload.Error.Code != "" {
		code = payload.Error.Code
	}
	if payload.Error.RateLimit.Reset > 0 {
		reset = payload.Error.RateLimit.Reset
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}

	typ, defaultCode := normalizedErrorTypeAndCode(status)
	if code == "" {
		code = defaultCode
	}

	retryAfter := header.Get("Retry-After")
	if status == http.StatusTooManyRequests && retryAfter == "" {
		retryAfter = retryAfterFromRateLimit(reset, message, time.Now())
	}
	requestID := header.Get("x-request-id")
	if requestID == "" {
		requestID = header.Get("lb-request-id")
	}
	return &upstreamAPIError{
		Status:     status,
		Message:    message,
		Type:       typ,
		Code:       code,
		RetryAfter: retryAfter,
		RequestID:  requestID,
	}
}

func normalizedErrorTypeAndCode(status int) (string, string) {
	switch status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return "invalid_request_error", "invalid_request"
	case http.StatusUnauthorized:
		return "authentication_error", "invalid_api_key"
	case http.StatusForbidden:
		return "permission_error", "permission_denied"
	case http.StatusNotFound:
		return "not_found_error", "not_found"
	case http.StatusTooManyRequests:
		return "rate_limit_error", "rate_limit_exceeded"
	default:
		if status >= http.StatusInternalServerError {
			return "server_error", "upstream_server_error"
		}
		return "api_error", "upstream_api_error"
	}
}

func retryAfterFromRateLimit(reset float64, message string, now time.Time) string {
	var resetAt time.Time
	if reset > 0 {
		seconds, fraction := math.Modf(reset)
		resetAt = time.Unix(int64(seconds), int64(fraction*float64(time.Second)))
	} else {
		lower := strings.ToLower(message)
		const marker = "resets at "
		if index := strings.Index(lower, marker); index >= 0 {
			fields := strings.Fields(message[index+len(marker):])
			if len(fields) > 0 {
				value := strings.Trim(fields[0], ".,;)]}")
				resetAt, _ = time.Parse(time.RFC3339Nano, value)
			}
		}
	}
	if resetAt.IsZero() || !resetAt.After(now) {
		return ""
	}
	seconds := int64((resetAt.Sub(now) + time.Second - 1) / time.Second)
	return strconv.FormatInt(seconds, 10)
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
func (c *CCClient) Send(ctx context.Context, req *ChatRequest) (*http.Response, error) {
	ccReq, err := openAIToCC(req)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(ccReq)
	if err != nil {
		return nil, fmt.Errorf("marshal cc request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/alpha/generate", bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		return nil, normalizeUpstreamError(resp.StatusCode, body, resp.Header)
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

// resolveModelName 将客户端传来的 model ID 映射为 CC API 期望的格式。
// 优先使用动态 modelCatalog（来自 /provider/v1/models），
// 回退到根据模型名推断 provider 前缀。
func resolveModelName(model string) string {
	// 已有 provider 前缀（含 /），直接使用
	if strings.Contains(model, "/") {
		return model
	}

	// 在动态 catalog 中查找匹配的 ID（catalog 中的 ID 已含正确前缀）
	for _, m := range modelCatalog {
		if m.ID == model || strings.HasSuffix(m.ID, "/"+model) {
			return m.ID
		}
	}

	// catalog 中未找到，根据模型名前缀推断 provider
	switch {
	case strings.HasPrefix(model, "gemini-"):
		return "google/" + model
	case strings.HasPrefix(model, "claude-"):
		return "anthropic/" + model
	case strings.HasPrefix(model, "gpt-"):
		return "openai/" + model
	default:
		return model
	}
}

const (
	defaultCCMaxTokens = 64_000
	maximumCCMaxTokens = 200_000
)

func openAIToCC(req *ChatRequest) (CCRequest, error) {
	tools := toolsToCC(req.Tools)
	msgs, err := messagesToCC(req.Messages)
	if err != nil {
		return CCRequest{}, err
	}
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
			Model:     resolveModelName(req.Model),
			Messages:  msgs,
			Tools:     tools,
			System:    system,
			MaxTokens: req.MaxTokens,
			Stream:    true, // CC API 只支持流式
		},
	}
	// command-code's main agent, print mode, and subagents all request 64k
	// output tokens. Match that behavior when OpenAI-compatible clients omit
	// max_tokens so long tool arguments are not cut off at the old 4k default.
	if cc.Params.MaxTokens <= 0 {
		cc.Params.MaxTokens = defaultCCMaxTokens
	}
	if cc.Params.MaxTokens > maximumCCMaxTokens {
		cc.Params.MaxTokens = maximumCCMaxTokens
	}
	return cc, nil
}

func extractSystem(msgs []Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if text := m.Content.PlainText(); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func messagesToCC(msgs []Message) ([]CCMsg, error) {
	var out []CCMsg
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			continue // 已提取到 top-level system
		}
		content, err := contentToCC(m)
		if err != nil {
			return nil, err
		}
		cc := CCMsg{Role: roleToCC(m.Role), Content: content}
		out = append(out, cc)
	}
	return out, nil
}

func roleToCC(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	default:
		return "user"
	}
}

func contentToCC(m Message) ([]CCPart, error) {
	if m.Role == "tool" {
		return []CCPart{{
			Type: "text",
			Text: fmt.Sprintf("Tool result from %s (%s):\n%s", m.Name, m.ToolCallID, m.Content.PlainText()),
		}}, nil
	}

	var parts []CCPart
	if text, ok := m.Content.TextValue(); ok && text != "" {
		parts = append(parts, CCPart{Type: "text", Text: text})
	}
	for _, part := range m.Content.PartsValue() {
		switch part.Type {
		case "text":
			if part.Text != "" {
				parts = append(parts, CCPart{Type: "text", Text: part.Text})
			}
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				return nil, &invalidRequestError{message: "image_url content requires a non-empty url"}
			}
			mediaType, data, err := parseDataURL(part.ImageURL.URL)
			if err != nil {
				return nil, &invalidRequestError{message: err.Error()}
			}
			parts = append(parts, CCPart{
				Type: "image",
				Source: map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			})
		default:
			return nil, &invalidRequestError{message: fmt.Sprintf("unsupported message content type %q", part.Type)}
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

	return parts, nil
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

// parseDataURL parses a base64 image data URL. Remote image URLs are rejected
// because the Command Code payload requires inline image data.
func parseDataURL(rawURL string) (string, string, error) {
	if !strings.HasPrefix(rawURL, "data:") {
		return "", "", fmt.Errorf("image_url must be a base64 data URL")
	}
	after := strings.TrimPrefix(rawURL, "data:")
	parts := strings.SplitN(after, ",", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("image_url contains an invalid data URL")
	}
	if !strings.HasSuffix(parts[0], ";base64") {
		return "", "", fmt.Errorf("image_url data URL must use base64 encoding")
	}
	mediaType := strings.TrimSuffix(parts[0], ";base64")
	if !strings.HasPrefix(mediaType, "image/") {
		return "", "", fmt.Errorf("image_url data URL must contain an image media type")
	}
	if _, err := base64.StdEncoding.DecodeString(parts[1]); err != nil {
		if _, rawErr := base64.RawStdEncoding.DecodeString(parts[1]); rawErr != nil {
			return "", "", fmt.Errorf("image_url contains invalid base64 data: %w", err)
		}
	}
	return mediaType, parts[1], nil
}
