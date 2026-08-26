package app

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ============================================================================
// OpenAI Responses API wire types (client ↔ us).
//
// This protocol is distinct from Chat Completions (types.go): input is a
// union of string|array, tools are flat (not nested under "function"), and
// the response body is a single object with an "output" array rather than
// "choices". These types are pure data shapes — the adapter that converts
// them into the existing ChatRequest pipeline lives in responses.go.
// ============================================================================

// ResponsesRequest is the request body for POST /v1/responses.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              ResponsesInput  `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Store              bool            `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Include            []string        `json:"include,omitempty"`
}

// ResponsesInput is the "input" field union: either a plain string (a single
// user message) or an array of typed input items. It unmarshals by trying
// the string form first, then falling back to the array form.
type ResponsesInput struct {
	text  *string
	items []ResponsesInputItem
}

func (in ResponsesInput) TextValue() (string, bool) {
	if in.text == nil {
		return "", false
	}
	return *in.text, true
}

func (in ResponsesInput) Items() []ResponsesInputItem {
	return in.items
}

func (in *ResponsesInput) UnmarshalJSON(data []byte) error {
	*in = ResponsesInput{}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		in.text = &text
		return nil
	}

	var items []ResponsesInputItem
	if err := json.Unmarshal(data, &items); err == nil {
		in.items = items
		return nil
	}

	return fmt.Errorf("input must be a string or an array of input items")
}

func (in ResponsesInput) MarshalJSON() ([]byte, error) {
	if in.text != nil {
		return json.Marshal(*in.text)
	}
	if in.items != nil {
		return json.Marshal(in.items)
	}
	return []byte("null"), nil
}

// ResponsesInputItem is a union of every item type that can appear in the
// "input" array. Only the fields relevant to Type are populated; the rest
// stay zero. This mirrors the OpenAI Responses API's discriminated-union
// shape for input items.
type ResponsesInputItem struct {
	Type string `json:"type"`

	// "message"
	Role    string                 `json:"role,omitempty"`
	Content []ResponsesContentPart `json:"content,omitempty"`

	// "function_call"
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// "function_call_output"
	Output string `json:"output,omitempty"`

	// "reasoning" — content is intentionally left unmodeled beyond Type;
	// reasoning input items are detected and skipped, never round-tripped.
}

// ResponsesContentPart is one entry of a "message" input item's content
// array: "input_text"/"output_text" carry Text, "input_image" carries
// either an inline url/base64 string (ImageURL) or a stored file reference
// (FileID) — the adapter rejects the FileID form since CC requires inline
// image data.
type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// ResponsesTool is a flat tool definition — unlike Chat Completions' Tool
// (types.go), which nests name/description/parameters under "function".
type ResponsesTool struct {
	Type        string         `json:"type"` // "function" (only supported type)
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ============================================================================
// Response-side types. Not yet populated by this batch — defined now so
// the streaming emitter and non-streaming handler (later batches) have a
// stable shape to target.
// ============================================================================

// ResponsesOutputItem is a union covering every item type that can appear in
// a ResponsesResponse's "output" array: assistant messages, reasoning, and
// function calls.
type ResponsesOutputItem struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"` // "message" | "reasoning" | "function_call"
	Status string `json:"status,omitempty"`

	// "message"
	Role    string                       `json:"role,omitempty"`
	Content []ResponsesOutputContentPart `json:"content,omitempty"`

	// "reasoning"
	Summary []ResponsesReasoningSummary `json:"summary,omitempty"`

	// "function_call"
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ResponsesOutputContentPart is one entry of a "message" output item's
// content array.
type ResponsesOutputContentPart struct {
	Type string `json:"type"` // "output_text"
	Text string `json:"text,omitempty"`
}

// ResponsesReasoningSummary is one entry of a "reasoning" output item's
// summary array.
type ResponsesReasoningSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text,omitempty"`
}

// ResponsesUsage mirrors the Responses API's usage object — field names
// differ from Chat Completions' Usage (types.go).
type ResponsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  ResponsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails ResponsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type ResponsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesIncompleteDetails explains why a response was cut short.
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"` // e.g. "max_output_tokens"
}

// ResponsesResponse is the full non-streaming response body for
// POST /v1/responses.
type ResponsesResponse struct {
	ID                string                      `json:"id"`
	Object            string                      `json:"object"` // "response"
	CreatedAt         int64                       `json:"created_at"`
	Status            string                      `json:"status"` // "completed" | "incomplete" | "failed" | "in_progress"
	Model             string                      `json:"model"`
	Output            []ResponsesOutputItem       `json:"output"`
	Usage             *ResponsesUsage             `json:"usage,omitempty"`
	Store             bool                        `json:"store"`
	Instructions      string                      `json:"instructions,omitempty"`
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
	Error             any                         `json:"error,omitempty"`
}
