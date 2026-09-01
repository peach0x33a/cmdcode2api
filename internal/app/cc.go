package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// maxSSELineBytes bounds one SSE line. A tool-call event carries the entire
// tool input inline, so this must clear the largest response the upstream can
// produce: maximumCCMaxTokens is roughly 800 KB of text, and JSON escaping
// inflates that further. Overshooting is reported as an error rather than
// silently truncating the stream.
const maxSSELineBytes = 32 * 1024 * 1024

// readSSELine reads one newline-terminated line of any length.
//
// bufio.Scanner cannot do this: a token larger than its buffer stops the scan
// with ErrTooLong, so a single oversized tool-call event would discard every
// event after it — including the finish event — and the client would see a
// stream that just stops.
func readSSELine(r *bufio.Reader) (string, error) {
	var line strings.Builder
	for {
		chunk, isPrefix, err := r.ReadLine()
		if line.Len()+len(chunk) > maxSSELineBytes {
			return "", fmt.Errorf("sse line exceeds %d bytes", maxSSELineBytes)
		}
		line.Write(chunk)
		if err != nil {
			return line.String(), err
		}
		if !isPrefix {
			return line.String(), nil
		}
	}
}

func decodeSSEEvent(payload string) (CCStreamEvent, error) {
	var ev CCStreamEvent
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&ev); err != nil {
		return ev, fmt.Errorf("parse sse data: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return ev, fmt.Errorf("parse sse data: trailing content: %w", err)
	}
	return ev, nil
}

type streamEndKind uint8

const (
	streamEndEOF streamEndKind = iota + 1
	streamEndDone
)

// ParseStreamEvents reads upstream SSE events. Callers that need to distinguish
// an explicit [DONE] from a bare EOF use parseStreamEvents directly.
func ParseStreamEvents(resp *http.Response, onEvent func(CCStreamEvent) error) error {
	_, err := parseStreamEvents(resp, onEvent)
	return err
}

func parseStreamEvents(resp *http.Response, onEvent func(CCStreamEvent) error) (streamEndKind, error) {
	defer resp.Body.Close()
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var dataLines []string
	dataBytes := 0

	dispatch := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		dataBytes = 0
		if strings.TrimSpace(payload) == "" {
			return false, nil
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			return true, nil
		}
		ev, err := decodeSSEEvent(payload)
		if err != nil {
			return false, err
		}
		if err := onEvent(ev); err != nil {
			return false, err
		}
		return false, nil
	}

	for {
		raw, readErr := readSSELine(reader)
		line := strings.TrimSuffix(raw, "\r")

		switch {
		case line == "":
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
		case strings.HasPrefix(line, ":"):
			// SSE comment/keep-alive.
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			dataBytes += len(value)
			if dataBytes > maxSSELineBytes {
				return 0, fmt.Errorf("sse event exceeds %d bytes", maxSSELineBytes)
			}
			dataLines = append(dataLines, value)
		case isSSEFieldLine(line):
			// event, id and retry fields do not carry the JSON payload.
		default:
			// Preserve compatibility with upstreams that send bare JSON lines.
			if len(dataLines) > 0 {
				done, err := dispatch()
				if err != nil {
					return 0, err
				}
				if done {
					return streamEndDone, nil
				}
			}
			dataLines = append(dataLines, strings.TrimSpace(line))
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
		}

		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return 0, readErr
			}
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
			return streamEndEOF, nil
		}
	}
}

// isSSEFieldLine reports whether a line begins with a known non-data SSE field.
func isSSEFieldLine(line string) bool {
	for _, field := range []string{"event:", "id:", "retry:"} {
		if strings.HasPrefix(line, field) {
			return true
		}
	}
	return false
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
			MaxTokens: req.OutputTokenBudget(),
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
	toolNames := make(map[string]string)
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			continue // 已提取到 top-level system
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" && tc.Function.Name != "" {
					toolNames[tc.ID] = tc.Function.Name
				}
			}
		}
		if m.Role == "tool" && m.Name == "" {
			m.Name = toolNames[m.ToolCallID]
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
	case "assistant", "tool":
		return role
	default:
		return "user"
	}
}

func contentToCC(m Message) ([]CCPart, error) {
	if m.Role == "tool" {
		if m.ToolCallID == "" {
			return nil, &invalidRequestError{message: "tool message requires tool_call_id"}
		}
		if m.Name == "" {
			return nil, &invalidRequestError{message: fmt.Sprintf("cannot resolve tool name for tool_call_id %q", m.ToolCallID)}
		}
		for _, part := range m.Content.PartsValue() {
			if part.Type != "text" {
				return nil, &invalidRequestError{message: fmt.Sprintf("unsupported tool result content type %q", part.Type)}
			}
		}
		return []CCPart{{
			Type:       "tool-result",
			ToolCallID: m.ToolCallID,
			ToolName:   m.Name,
			Output: &CCOutput{
				Type:  "text",
				Value: m.Content.PlainText(),
			},
		}}, nil
	}

	var parts []CCPart
	if m.Role == "assistant" && m.ReasoningContent != "" {
		parts = append(parts, CCPart{Type: "reasoning", Text: m.ReasoningContent})
	}
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

	for _, tc := range m.ToolCalls {
		if tc.ID == "" {
			return nil, &invalidRequestError{message: "assistant tool call requires a non-empty id"}
		}
		if tc.Function.Name == "" {
			return nil, &invalidRequestError{message: fmt.Sprintf("assistant tool call %q requires a non-empty function name", tc.ID)}
		}
		input, err := validateToolInputObject(tc.Function.Arguments)
		if err != nil {
			return nil, &invalidRequestError{message: fmt.Sprintf("assistant tool call %q has invalid arguments: %v", tc.ID, err)}
		}
		parts = append(parts, CCPart{
			Type:       "tool-call",
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Input:      input,
		})
	}

	return parts, nil
}

func validateToolInputObject(arguments string) (json.RawMessage, error) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("trailing content: %w", err)
	}
	return json.RawMessage(arguments), nil
}

func toolsToCC(tools []Tool) []CCTool {
	if tools == nil {
		return []CCTool{}
	}
	out := make([]CCTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			// OpenAI lets a no-argument tool omit parameters, but the upstream
			// expects every tool to carry a schema. Send the canonical empty
			// object so one no-arg tool does not invalidate the whole request.
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, CCTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
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
