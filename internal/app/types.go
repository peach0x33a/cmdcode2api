package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// OpenAI 兼容格式（客户端 ↔ 我们）
// ============================================================================

type ChatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Tools     []Tool    `json:"tools,omitempty"`
}

type Message struct {
	Role             string         `json:"role"`
	Content          MessageContent `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
}

// MessageContent is the OpenAI message content union: string, content parts,
// or null. Keeping the variants explicit prevents conversion code from
// silently ignoring valid JSON shapes.
type MessageContent struct {
	text  *string
	parts []ContentPart
}

func TextContent(text string) MessageContent {
	return MessageContent{text: &text}
}

func PartsContent(parts ...ContentPart) MessageContent {
	return MessageContent{parts: append([]ContentPart(nil), parts...)}
}

func (c MessageContent) TextValue() (string, bool) {
	if c.text == nil {
		return "", false
	}
	return *c.text, true
}

func (c MessageContent) PartsValue() []ContentPart {
	return append([]ContentPart(nil), c.parts...)
}

func (c MessageContent) PlainText() string {
	if c.text != nil {
		return *c.text
	}
	var out strings.Builder
	for _, part := range c.parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

func (c MessageContent) IsEmpty() bool {
	return c.PlainText() == "" && len(c.parts) == 0
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	*c = MessageContent{}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		c.text = &text
		return nil
	}

	var parts []ContentPart
	if err := json.Unmarshal(data, &parts); err == nil {
		c.parts = parts
		return nil
	}

	return fmt.Errorf("message content must be a string, an array of content parts, or null")
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.text != nil {
		return json.Marshal(*c.text)
	}
	if c.parts != nil {
		return json.Marshal(c.parts)
	}
	return []byte("null"), nil
}

type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // "function"
	Function CallFunc `json:"function"`

	// recoveredRawDSML distinguishes a protocol-recovery artifact from an
	// intentional structured call. It is internal-only and enables safe
	// cross-representation deduplication without collapsing two intentional,
	// semantically identical calls.
	recoveredRawDSML bool
}

type CallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// 非流式响应
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// 流式响应块
type ChatStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// StreamToolCall is the streaming delta variant of ToolCall.
// Unlike the non-streaming ToolCall (used in Message), it includes an
// index field required by the OpenAI streaming protocol so that AI SDK
// clients can associate delta chunks to the correct tool call position.
type StreamToolCall struct {
	Index    int       `json:"index"`
	ID       string    `json:"id,omitempty"`
	Type     string    `json:"type,omitempty"`
	Function *CallFunc `json:"function,omitempty"`
}

type StreamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []StreamToolCall `json:"tool_calls,omitempty"`
}

// MODELS 列表
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// CCProviderModel CC API /provider/v1/models 返回的单个模型
type CCProviderModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

// CCProviderModelList CC API /provider/v1/models 响应
type CCProviderModelList struct {
	Object string            `json:"object"`
	Data   []CCProviderModel `json:"data"`
}

// ============================================================================
// Command Code 内部格式（我们 ↔ CC 服务器）
// ============================================================================

type CCRequest struct {
	Config         CCConfig `json:"config"`
	Memory         string   `json:"memory"`
	Taste          string   `json:"taste"`
	Skills         any      `json:"skills"`
	PermissionMode string   `json:"permissionMode"`
	Params         CCParams `json:"params"`
}

type CCConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []any    `json:"recentCommits"`
}

type CCParams struct {
	Model     string   `json:"model"`
	Messages  []CCMsg  `json:"messages"`
	Tools     []CCTool `json:"tools"`
	System    string   `json:"system,omitempty"`
	MaxTokens int      `json:"max_tokens"`
	Stream    bool     `json:"stream"`
}

type CCMsg struct {
	Role    string   `json:"role"`
	Content []CCPart `json:"content"`
}

type CCPart struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Source     map[string]any `json:"source,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolName   string         `json:"toolName,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
}

type CCTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// CC SSE 事件
type CCStreamEvent struct {
	Type            string   `json:"type"`
	ID              string   `json:"id,omitempty"`
	MessageID       string   `json:"messageId,omitempty"`
	Text            string   `json:"text,omitempty"`
	Delta           string   `json:"delta,omitempty"`
	ToolCallID      string   `json:"toolCallId,omitempty"`
	ToolName        string   `json:"toolName,omitempty"`
	Input           any      `json:"input,omitempty"`
	Args            any      `json:"args,omitempty"`
	Arguments       any      `json:"arguments,omitempty"`
	FinishReason    string   `json:"finishReason,omitempty"`
	RawFinishReason string   `json:"rawFinishReason,omitempty"`
	Usage           *CCUsage `json:"usage,omitempty"`
	TotalUsage      *CCUsage `json:"totalUsage,omitempty"`
	Error           any      `json:"error,omitempty"`
}

type CCUsage struct {
	InputTokens       int             `json:"inputTokens"`
	OutputTokens      int             `json:"outputTokens"`
	TotalTokens       int             `json:"totalTokens"`
	ReasoningTokens   int             `json:"reasoningTokens"`
	InputTokenDetails *CCTokenDetails `json:"inputTokenDetails,omitempty"`
}

type CCTokenDetails struct {
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}
