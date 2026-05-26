package app

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
	Role       string     `json:"role"`
	Content    any        `json:"content"` // string | []ContentPart | null
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
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

type StreamDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
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
	Output     *CCOutput      `json:"output,omitempty"`
}

type CCOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type CCTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// CC SSE 事件
type CCStreamEvent struct {
	Type         string         `json:"type"`
	Text         string         `json:"text,omitempty"`
	ToolCallID   string         `json:"toolCallId,omitempty"`
	ToolName     string         `json:"toolName,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Arguments    map[string]any `json:"arguments,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	TotalUsage   *CCUsage       `json:"totalUsage,omitempty"`
	Error        any            `json:"error,omitempty"`
}

type CCUsage struct {
	InputTokens       int             `json:"inputTokens"`
	OutputTokens      int             `json:"outputTokens"`
	InputTokenDetails *CCTokenDetails `json:"inputTokenDetails,omitempty"`
}

type CCTokenDetails struct {
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}
