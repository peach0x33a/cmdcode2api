package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// responsesToChatRequest converts a Responses API request into the internal
// ChatRequest shape so it can ride the existing Chat Completions ->
// Command Code pipeline (openAIToCC in cc.go) unchanged. Returned errors are
// always *invalidRequestError so dispatchToCC's existing errors.As branch
// (handler.go) maps them to HTTP 400 for free.
func responsesToChatRequest(req *ResponsesRequest) (*ChatRequest, error) {
	if req.PreviousResponseID != "" {
		return nil, &invalidRequestError{message: "previous_response_id is not supported; this gateway is stateless — send the full conversation in input"}
	}

	var messages []Message

	if req.Instructions != "" {
		messages = append(messages, Message{
			Role:    "system",
			Content: TextContent(req.Instructions),
		})
	}

	if text, ok := req.Input.TextValue(); ok {
		messages = append(messages, Message{
			Role:    "user",
			Content: TextContent(text),
		})
	} else {
		for _, item := range req.Input.Items() {
			msg, ok, err := responsesInputItemToMessage(item)
			if err != nil {
				return nil, err
			}
			if ok {
				messages = append(messages, msg)
			}
		}
	}

	tools, err := responsesToolsToChatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	return &ChatRequest{
		Model:               req.Model,
		Messages:            messages,
		Stream:              req.Stream,
		MaxCompletionTokens: req.MaxOutputTokens,
		Tools:               tools,
	}, nil
}

// responsesInputItemToMessage converts one "input" array item into a
// ChatRequest Message. ok is false for item types that are intentionally
// dropped (currently just "reasoning").
func responsesInputItemToMessage(item ResponsesInputItem) (Message, bool, error) {
	switch item.Type {
	case "message":
		content, err := responsesContentPartsToMessageContent(item.Content)
		if err != nil {
			return Message{}, false, err
		}
		return Message{Role: item.Role, Content: content}, true, nil

	case "function_call":
		return Message{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:   item.CallID,
				Type: "function",
				Function: CallFunc{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			}},
		}, true, nil

	case "function_call_output":
		return Message{
			Role:       "tool",
			ToolCallID: item.CallID,
			Content:    TextContent(item.Output),
		}, true, nil

	case "reasoning":
		return Message{}, false, nil

	default:
		return Message{}, false, &invalidRequestError{message: fmt.Sprintf("unsupported input item type: %q", item.Type)}
	}
}

// responsesContentPartsToMessageContent converts a "message" input item's
// content array into MessageContent, collapsing to plain text when there is
// exactly one text part.
func responsesContentPartsToMessageContent(parts []ResponsesContentPart) (MessageContent, error) {
	if len(parts) == 1 && (parts[0].Type == "input_text" || parts[0].Type == "output_text") {
		return TextContent(parts[0].Text), nil
	}

	var out []ContentPart
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			out = append(out, ContentPart{Type: "text", Text: part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return MessageContent{}, &invalidRequestError{message: "input_image requires inline image data (url or base64); file_id references are not supported"}
			}
			out = append(out, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: part.ImageURL}})
		default:
			return MessageContent{}, &invalidRequestError{message: fmt.Sprintf("unsupported content part type: %q", part.Type)}
		}
	}
	return PartsContent(out...), nil
}

// responsesToolsToChatTools converts the Responses API's flat tool shape
// ({type,name,description,parameters}) into Chat Completions' nested Tool
// shape ({type:"function", function:{...}}).
func responsesToolsToChatTools(tools []ResponsesTool) ([]Tool, error) {
	if tools == nil {
		return nil, nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			return nil, &invalidRequestError{message: fmt.Sprintf("unsupported tool type %q: only \"function\" tools are supported", t.Type)}
		}
		out = append(out, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out, nil
}

// ============================================================================
// Responses API streaming emitter (us -> client).
//
// writeNamedSSE and responsesEmitter turn a normalized Chat-Completions-style
// event stream into the named-event SSE wire format the Responses API (and
// Codex CLI, its main consumer) expects. Unlike Chat Completions, every frame
// carries an explicit "event:" line and a monotonic sequence_number, and the
// stream has no "[DONE]" sentinel — the terminal event (response.completed /
// .incomplete / .failed) is itself the end of the stream.
//
// This file only builds and writes the frames; it does not know how to
// parse raw upstream tool-call deltas (that machinery already exists in
// events.go/handler.go and is reused, not duplicated, by whatever caller
// drives this emitter).
// ============================================================================

// writeNamedSSE writes one Server-Sent Event frame in the Responses API's
// named-event shape: "event: <event>\ndata: <json>\n\n". This is distinct
// from writeSSE (handler.go), which is hardcoded to ChatStreamChunk and
// writes bare "data:" frames with no "event:" line for Chat Completions.
//
// flusher may be nil, which makes this (and everything built on it) directly
// testable against a plain io.Writer/bytes.Buffer without a real HTTP
// response writer.
func writeNamedSSE(w io.Writer, flusher http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("write %s event: %w", event, err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// buildResponsesUsage converts the Chat-Completions-shaped usage numbers
// exposed by ccEventNormalizer (prompt/completion tokens, cache-read tokens,
// reasoning tokens) into the Responses API's ResponsesUsage shape.
func buildResponsesUsage(promptTokens, completionTokens, cacheReadTokens, reasoningTokens int) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:         promptTokens,
		InputTokensDetails:  ResponsesInputTokenDetails{CachedTokens: cacheReadTokens},
		OutputTokens:        completionTokens,
		OutputTokensDetails: ResponsesOutputTokenDetails{ReasoningTokens: reasoningTokens},
		TotalTokens:         promptTokens + completionTokens,
	}
}

// mustRandomHex panics on entropy-source failure, mirroring genStreamID's
// handling of the same (effectively never-failing) error in handler.go.
func mustRandomHex(n int) string {
	s, err := randomHex(n)
	if err != nil {
		panic(err)
	}
	return s
}

// responsesPendingToolCall is a tool call queued via AddToolCall. Its full
// added/delta/done/item.done event lifecycle is only emitted once a terminal
// method (Complete/Incomplete) runs, so tool-call items always appear after
// the reasoning and message items close — matching the fixed item-close
// ordering the Responses API/Codex CLI expects.
type responsesPendingToolCall struct {
	callID    string
	name      string
	arguments string
}

// responsesEmitter owns all protocol state for one streamed Responses API
// response (POST /v1/responses with stream:true) and renders it as the
// named-event SSE wire format described above. It writes only against an
// io.Writer + optional http.Flusher, so it is fully unit-testable without a
// real HTTP round trip (see responses_test.go).
//
// Call order: construct with newResponsesEmitter (this immediately emits
// response.created and response.in_progress), then interleave
// AddReasoningDelta / AddTextDelta as the underlying model stream produces
// content and AddToolCall for each tool call as it completes, and finish
// with exactly one of Complete, Incomplete, or Fail. No method may be called
// after any of those three terminal methods has run — doing so returns an
// error and writes nothing further.
type responsesEmitter struct {
	w       io.Writer
	flusher http.Flusher
	model   string

	responseID string
	createdAt  int64

	seq         int
	outputIndex int

	reasoningOpen   bool
	reasoningClosed bool
	reasoningID     string
	reasoningIndex  int
	reasoningBuf    strings.Builder

	messageOpen   bool
	messageClosed bool
	messageID     string
	messageIndex  int
	messageBuf    strings.Builder

	pendingToolCalls []responsesPendingToolCall

	// done is set the moment a terminal method (Complete/Incomplete/Fail) is
	// entered, before its events are written, so a caller that ignores a
	// write error and calls again still cannot reopen the stream.
	done bool

	// constructErr holds the first write error hit while emitting
	// response.created/response.in_progress from the constructor, which has
	// no error return of its own. Later methods return their own errors
	// directly; check Err() only if you need to confirm construction itself
	// succeeded.
	constructErr error
}

// newResponsesEmitter creates a responsesEmitter for one response to model
// and immediately emits response.created followed by response.in_progress.
func newResponsesEmitter(w io.Writer, flusher http.Flusher, model string) *responsesEmitter {
	e := &responsesEmitter{
		w:          w,
		flusher:    flusher,
		model:      model,
		responseID: "resp_" + mustRandomHex(24),
		createdAt:  time.Now().Unix(),
	}
	if err := e.emit("response.created", map[string]any{
		"response": e.responseObject("in_progress", nil, nil, nil, nil),
	}); err != nil {
		e.constructErr = err
	}
	if err := e.emit("response.in_progress", map[string]any{
		"response": e.responseObject("in_progress", nil, nil, nil, nil),
	}); err != nil && e.constructErr == nil {
		e.constructErr = err
	}
	return e
}

// Err returns the first write error encountered while constructing the
// stream, if any.
func (e *responsesEmitter) Err() error {
	return e.constructErr
}

// Done reports whether a terminal method (Complete/Incomplete/Fail) has
// already run, so a caller that hit an error partway through its own
// finish-handling logic (after some items were already emitted) can tell
// whether it is safe to call Fail as a last resort or whether the stream
// already has its one allowed terminal event.
func (e *responsesEmitter) Done() bool {
	return e.done
}

// emit assigns the next sequence number, stamps type/sequence_number onto
// payload, and writes the frame. Every call to emit — regardless of event
// name — consumes exactly one sequence number.
func (e *responsesEmitter) emit(eventType string, payload map[string]any) error {
	e.seq++
	payload["type"] = eventType
	payload["sequence_number"] = e.seq
	return writeNamedSSE(e.w, e.flusher, eventType, payload)
}

// nextOutputIndex allocates the next output_index. Indices are handed out
// lazily, in the order items are first opened, and are never reused even
// after the item they were assigned to closes.
func (e *responsesEmitter) nextOutputIndex() int {
	index := e.outputIndex
	e.outputIndex++
	return index
}

func (e *responsesEmitter) responseObject(status string, output []ResponsesOutputItem, usage *ResponsesUsage, incomplete *ResponsesIncompleteDetails, errObj any) ResponsesResponse {
	if output == nil {
		output = []ResponsesOutputItem{}
	}
	return ResponsesResponse{
		ID:                e.responseID,
		Object:            "response",
		CreatedAt:         e.createdAt,
		Status:            status,
		Model:             e.model,
		Output:            output,
		Usage:             usage,
		Store:             false,
		IncompleteDetails: incomplete,
		Error:             errObj,
	}
}

// AddReasoningDelta appends one incremental chunk of reasoning-summary text.
// The reasoning item is opened lazily on the first call. Calling this after
// the reasoning item has closed (i.e. after a text delta has arrived, or
// after a terminal method has run) is an error — reasoning must fully
// precede text, never interleave with it.
func (e *responsesEmitter) AddReasoningDelta(delta string) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: AddReasoningDelta called after terminal event")
	}
	if e.reasoningClosed {
		return fmt.Errorf("responsesEmitter: AddReasoningDelta called after reasoning item closed")
	}
	if delta == "" {
		return nil
	}
	if !e.reasoningOpen {
		if err := e.openReasoning(); err != nil {
			return err
		}
	}
	e.reasoningBuf.WriteString(delta)
	return e.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       e.reasoningID,
		"output_index":  e.reasoningIndex,
		"summary_index": 0,
		"delta":         delta,
	})
}

func (e *responsesEmitter) openReasoning() error {
	e.reasoningOpen = true
	e.reasoningID = "rs_" + mustRandomHex(24)
	e.reasoningIndex = e.nextOutputIndex()
	return e.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       e.reasoningID,
		"output_index":  e.reasoningIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

// closeReasoning is a no-op if reasoning was never opened or already closed,
// so it is safe to call unconditionally from AddTextDelta and from the
// terminal methods.
func (e *responsesEmitter) closeReasoning() error {
	if !e.reasoningOpen || e.reasoningClosed {
		return nil
	}
	e.reasoningClosed = true
	text := e.reasoningBuf.String()
	if err := e.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id":       e.reasoningID,
		"output_index":  e.reasoningIndex,
		"summary_index": 0,
		"text":          text,
	}); err != nil {
		return err
	}
	return e.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id":       e.reasoningID,
		"output_index":  e.reasoningIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": text},
	})
}

// AddTextDelta appends one incremental chunk of assistant output text. The
// reasoning item (if any) is closed first — reasoning always fully precedes
// text — and the message item is opened lazily on the first call.
func (e *responsesEmitter) AddTextDelta(delta string) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: AddTextDelta called after terminal event")
	}
	if e.messageClosed {
		return fmt.Errorf("responsesEmitter: AddTextDelta called after message item closed")
	}
	if delta == "" {
		return nil
	}
	if err := e.closeReasoning(); err != nil {
		return err
	}
	if !e.messageOpen {
		if err := e.openMessage(); err != nil {
			return err
		}
	}
	e.messageBuf.WriteString(delta)
	return e.emit("response.output_text.delta", map[string]any{
		"item_id":       e.messageID,
		"output_index":  e.messageIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

func (e *responsesEmitter) openMessage() error {
	e.messageOpen = true
	e.messageID = "msg_" + mustRandomHex(24)
	e.messageIndex = e.nextOutputIndex()
	if err := e.emit("response.output_item.added", map[string]any{
		"item_id":      e.messageID,
		"output_index": e.messageIndex,
		"item": map[string]any{
			"id":      e.messageID,
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
	}); err != nil {
		return err
	}
	return e.emit("response.content_part.added", map[string]any{
		"item_id":       e.messageID,
		"output_index":  e.messageIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	})
}

// closeMessage is a no-op if the message item was never opened or already
// closed, so it is safe to call unconditionally from the terminal methods.
func (e *responsesEmitter) closeMessage() error {
	if !e.messageOpen || e.messageClosed {
		return nil
	}
	e.messageClosed = true
	text := e.messageBuf.String()
	if err := e.emit("response.output_text.done", map[string]any{
		"item_id":       e.messageID,
		"output_index":  e.messageIndex,
		"content_index": 0,
		"text":          text,
	}); err != nil {
		return err
	}
	if err := e.emit("response.content_part.done", map[string]any{
		"item_id":       e.messageID,
		"output_index":  e.messageIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": text},
	}); err != nil {
		return err
	}
	return e.emit("response.output_item.done", map[string]any{
		"item_id":      e.messageID,
		"output_index": e.messageIndex,
		"item": map[string]any{
			"id":     e.messageID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{"type": "output_text", "text": text},
			},
		},
	})
}

// AddToolCall queues one completed tool call (clean call_id/name/arguments —
// parsing and deduping raw upstream tool-call deltas into this shape is the
// caller's job, reusing NewToolCallParser/toolCallDeduper/validateToolCall).
// Nothing is written to the stream yet; the full added/delta/done/item.done
// lifecycle for every queued call is emitted, in the order queued, when a
// terminal method (Complete/Incomplete) runs — after the reasoning and
// message items have closed.
func (e *responsesEmitter) AddToolCall(callID, name, argumentsJSON string) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: AddToolCall called after terminal event")
	}
	e.pendingToolCalls = append(e.pendingToolCalls, responsesPendingToolCall{
		callID:    callID,
		name:      name,
		arguments: argumentsJSON,
	})
	return nil
}

// emitToolCall writes one queued tool call's full item lifecycle and
// returns the ResponsesOutputItem to include in the terminal event's output
// array. The arguments are written as a single delta (the full JSON string)
// rather than streamed incrementally, per the emitter's contract.
func (e *responsesEmitter) emitToolCall(tc responsesPendingToolCall) (ResponsesOutputItem, error) {
	itemID := "fc_" + mustRandomHex(24)
	index := e.nextOutputIndex()

	if err := e.emit("response.output_item.added", map[string]any{
		"item_id":      itemID,
		"output_index": index,
		"item": map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": "",
			"status":    "in_progress",
		},
	}); err != nil {
		return ResponsesOutputItem{}, err
	}

	if err := e.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      itemID,
		"output_index": index,
		"delta":        tc.arguments,
	}); err != nil {
		return ResponsesOutputItem{}, err
	}

	if err := e.emit("response.function_call_arguments.done", map[string]any{
		"item_id":      itemID,
		"output_index": index,
		"arguments":    tc.arguments,
	}); err != nil {
		return ResponsesOutputItem{}, err
	}

	if err := e.emit("response.output_item.done", map[string]any{
		"item_id":      itemID,
		"output_index": index,
		"item": map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": tc.arguments,
			"status":    "completed",
		},
	}); err != nil {
		return ResponsesOutputItem{}, err
	}

	return ResponsesOutputItem{
		ID:        itemID,
		Type:      "function_call",
		Status:    "completed",
		CallID:    tc.callID,
		Name:      tc.name,
		Arguments: tc.arguments,
	}, nil
}

// finishOutputItems closes any still-open reasoning/message items and emits
// every queued tool call's lifecycle, returning the completed items in
// allocation order (reasoning, then message, then tool calls) for the
// terminal event's "output" array. Called exactly once, from Complete or
// Incomplete.
func (e *responsesEmitter) finishOutputItems() ([]ResponsesOutputItem, error) {
	if err := e.closeReasoning(); err != nil {
		return nil, err
	}
	if err := e.closeMessage(); err != nil {
		return nil, err
	}

	output := []ResponsesOutputItem{}
	if e.reasoningOpen {
		output = append(output, ResponsesOutputItem{
			ID:      e.reasoningID,
			Type:    "reasoning",
			Status:  "completed",
			Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: e.reasoningBuf.String()}},
		})
	}
	if e.messageOpen {
		output = append(output, ResponsesOutputItem{
			ID:      e.messageID,
			Type:    "message",
			Role:    "assistant",
			Status:  "completed",
			Content: []ResponsesOutputContentPart{{Type: "output_text", Text: e.messageBuf.String()}},
		})
	}
	for _, tc := range e.pendingToolCalls {
		item, err := e.emitToolCall(tc)
		if err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	e.pendingToolCalls = nil
	return output, nil
}

// Complete closes out the stream normally: it closes any open reasoning/
// message items, emits every queued tool call, and finishes with
// response.completed. No further methods may be called afterward.
func (e *responsesEmitter) Complete(usage ResponsesUsage) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: Complete called after terminal event")
	}
	e.done = true
	output, err := e.finishOutputItems()
	if err != nil {
		return err
	}
	return e.emit("response.completed", map[string]any{
		"response": e.responseObject("completed", output, &usage, nil, nil),
	})
}

// Incomplete closes out the stream the same way Complete does, but finishes
// with response.incomplete (response.status:"incomplete" and
// incomplete_details.reason set to reason, e.g. "max_output_tokens") instead
// of response.completed. No further methods may be called afterward.
func (e *responsesEmitter) Incomplete(reason string, usage ResponsesUsage) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: Incomplete called after terminal event")
	}
	e.done = true
	output, err := e.finishOutputItems()
	if err != nil {
		return err
	}
	return e.emit("response.incomplete", map[string]any{
		"response": e.responseObject("incomplete", output, &usage, &ResponsesIncompleteDetails{Reason: reason}, nil),
	})
}

// Fail terminates the stream immediately with response.failed and an error
// object carrying message — for upstream failures (a dropped connection, a
// parse error) where there is no clean way to close out whatever items were
// in flight. No further methods may be called afterward.
func (e *responsesEmitter) Fail(message string) error {
	if e.done {
		return fmt.Errorf("responsesEmitter: Fail called after terminal event")
	}
	e.done = true
	return e.emit("response.failed", map[string]any{
		"response": e.responseObject("failed", []ResponsesOutputItem{}, nil, nil, map[string]any{
			"message": message,
			"type":    "server_error",
		}),
	})
}

// ============================================================================
// HTTP handlers for POST /v1/responses (us <-> client). These are structural
// mirrors of handleNonStreamForAccount/handleStreamForAccount/
// handleChatCompletions in handler.go, adapted to drive a ResponsesResponse
// (non-stream) or a responsesEmitter (stream) instead of the Chat
// Completions wire shapes. See handler.go for the shared plumbing
// (dispatchToCC, toolCallDeduper, validateToolCall, resolveFinishReason)
// this reuses rather than duplicates.
// ============================================================================

// handleResponsesNonStream reads the upstream's full non-streaming CC-shaped
// response, recovers any tool calls embedded as plain text in either the
// visible text or the reasoning text, and writes the result as a single
// ResponsesResponse JSON body. Mirrors handleNonStreamForAccount.
func handleResponsesNonStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, account string) {
	var toolCalls toolCallDeduper
	normalizer := newCCEventNormalizer()
	var textContent strings.Builder
	var reasoningContent strings.Builder
	var finishReason string
	var truncated bool
	addToolCall := func(call ToolCall) error {
		if err := validateToolCall(call); err != nil {
			return err
		}
		toolCalls.Add(call)
		return nil
	}

	endKind, err := parseStreamEvents(resp, func(ev CCStreamEvent) error {
		if cfg.Debug {
			raw, _ := json.Marshal(ev)
			log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
		}
		events, err := normalizer.Consume(ev)
		if err != nil {
			return err
		}
		for _, event := range events {
			switch event.kind {
			case normalizedText:
				textContent.WriteString(event.text)
			case normalizedReasoning:
				reasoningContent.WriteString(event.text)
			case normalizedToolCall:
				if event.toolCall != nil {
					if err := addToolCall(*event.toolCall); err != nil {
						return err
					}
				}
			case normalizedFinish:
				finishReason = event.finishReason
				truncated = event.truncated
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s responses non-stream parse: %v", colorize("[ERROR]", ansiRed), err)
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "upstream_stream_error", "upstream stream error: "+err.Error())
		return
	}
	if !normalizer.finished {
		message := "upstream connection closed before a finish event"
		if endKind == streamEndDone {
			message = "upstream sent [DONE] before a finish event"
		}
		log.Printf("%s %s", colorize("[ERROR]", ansiRed), message)
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "upstream_stream_incomplete", message)
		return
	}

	// Extract embedded tool calls only after the upstream turn completed —
	// same ordering as handleNonStreamForAccount, and for the same reason:
	// a tool call can be split across chunks that only make sense once the
	// whole turn has arrived.
	visibleText := textContent.String()
	if visibleText != "" {
		tcp := NewToolCallParser()
		strippedContent, parsedCalls := tcp.Feed(visibleText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				if err := addToolCall(call); err != nil {
					writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", err.Error())
					return
				}
			}
			visibleText = strippedContent
		}
	}

	reasoningText := reasoningContent.String()
	if reasoningText != "" {
		tcp := NewToolCallParser()
		strippedReasoning, parsedCalls := tcp.Feed(reasoningText, true)
		if len(parsedCalls) > 0 {
			for _, call := range parsedCalls {
				if err := addToolCall(call); err != nil {
					writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", err.Error())
					return
				}
			}
			reasoningText = strippedReasoning
		}
	}

	if finishReason == "tool_calls" && len(toolCalls.kept) == 0 {
		writeErrorWithCode(w, http.StatusBadGateway, "server_error", "invalid_tool_call", "finish reason tool_calls contained no valid tool calls")
		return
	}
	truncated = truncated || toolCalls.hasUnsafeArguments()
	finishReason = resolveFinishReason(finishReason, len(toolCalls.kept) > 0, truncated)

	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.FinalUsage()
	usage.RecordFor(account, promptTokens, completionTokens, cacheRead, cacheWrite)

	// Allocation order mirrors the streaming emitter's finishOutputItems:
	// reasoning first (if present), then the assistant message, then each
	// tool call in first-seen order.
	output := []ResponsesOutputItem{}
	if reasoningText != "" {
		output = append(output, ResponsesOutputItem{
			ID:      "rs_" + mustRandomHex(24),
			Type:    "reasoning",
			Status:  "completed",
			Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: reasoningText}},
		})
	}
	if visibleText != "" {
		output = append(output, ResponsesOutputItem{
			ID:      "msg_" + mustRandomHex(24),
			Type:    "message",
			Role:    "assistant",
			Status:  "completed",
			Content: []ResponsesOutputContentPart{{Type: "output_text", Text: visibleText}},
		})
	}
	for _, tc := range toolCalls.kept {
		output = append(output, ResponsesOutputItem{
			ID:        "fc_" + mustRandomHex(24),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	status := "completed"
	var incomplete *ResponsesIncompleteDetails
	if finishReason == "length" {
		status = "incomplete"
		incomplete = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}

	responsesUsage := buildResponsesUsage(promptTokens, completionTokens, cacheRead, normalizer.ReasoningTokens())
	res := ResponsesResponse{
		ID:                "resp_" + mustRandomHex(24),
		Object:            "response",
		CreatedAt:         time.Now().Unix(),
		Status:            status,
		Model:             model,
		Output:            output,
		Usage:             &responsesUsage,
		Store:             false,
		IncompleteDetails: incomplete,
	}

	if cfg.Debug {
		raw, _ := json.Marshal(res)
		log.Printf("%s %s %s", colorize("[DEBUG]", ansiDim), colorize(">> response", ansiGreen), colorize(string(raw), ansiCyan))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// handleResponsesStream drives a responsesEmitter from the same normalized
// upstream event stream handleStreamForAccount consumes for Chat
// Completions, using the identical dual-parser pattern: text deltas and
// reasoning deltas are fed through two independent ToolCallParser instances
// so a tool call embedded as plain text in either channel is recovered
// before anything reaches the client. includeUsage is accepted for
// signature parity with handleStreamForAccount, but the Responses API
// protocol always attaches usage to its terminal event (there is no
// equivalent of Chat Completions' stream_options.include_usage opt-in), so
// it does not currently gate anything here.
func handleResponsesStream(w http.ResponseWriter, resp *http.Response, model string, usage *UsageTracker, cfg *Config, includeUsage bool, account string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "server_error", "streaming not supported")
		return
	}

	// The Responses API's wire format (named "event:" SSE frames, no [DONE]
	// sentinel) is written by responsesEmitter/writeNamedSSE, not writeSSE —
	// but the transport-level headers are the same as Chat Completions'
	// stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	emitter := newResponsesEmitter(w, flusher, model)
	if err := emitter.Err(); err != nil {
		log.Printf("%s responses stream: failed to write opening events: %v", colorize("[ERROR]", ansiRed), err)
		return
	}

	normalizer := newCCEventNormalizer()
	textParser := NewToolCallParser()
	reasoningParser := NewToolCallParser()
	var collectedToolCalls toolCallDeduper

	collectToolCall := func(tc ToolCall) error {
		if err := validateToolCall(tc); err != nil {
			return err
		}
		collectedToolCalls.Add(tc)
		return nil
	}

	flushParser := func(parser *ToolCallParser, reasoning bool) error {
		content, calls := parser.Feed("", true)
		var emitErr error
		if content != "" {
			if reasoning {
				emitErr = emitter.AddReasoningDelta(content)
			} else {
				emitErr = emitter.AddTextDelta(content)
			}
		}
		if emitErr != nil {
			return emitErr
		}
		for _, tc := range calls {
			if err := collectToolCall(tc); err != nil {
				return err
			}
		}
		return nil
	}

	// finishStream is the commit point: provisional calls have now had a
	// chance to be replaced by authoritative events and the emitter's one
	// terminal event (Complete or Incomplete) can be written exactly once.
	finishStream := func(reason string, truncated bool) error {
		if err := flushParser(reasoningParser, true); err != nil {
			return err
		}
		if err := flushParser(textParser, false); err != nil {
			return err
		}

		hasToolCalls := len(collectedToolCalls.kept) > 0
		if reason == "tool_calls" && !hasToolCalls {
			return fmt.Errorf("finish reason tool_calls contained no valid tool calls")
		}
		truncated = truncated || collectedToolCalls.hasUnsafeArguments()
		for _, tc := range collectedToolCalls.kept {
			if err := emitter.AddToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}

		finish := resolveFinishReason(reason, hasToolCalls, truncated)
		promptTokens, completionTokens, cacheRead, _ := normalizer.Usage()
		responsesUsage := buildResponsesUsage(promptTokens, completionTokens, cacheRead, normalizer.ReasoningTokens())
		if finish == "length" {
			return emitter.Incomplete("max_output_tokens", responsesUsage)
		}
		return emitter.Complete(responsesUsage)
	}

	endKind, err := parseStreamEvents(resp, func(ev CCStreamEvent) error {
		if cfg.Debug {
			raw, _ := json.Marshal(ev)
			log.Printf("%s %s event type=%s raw=%s", colorize("[DEBUG]", ansiDim), colorize("<< cc", ansiCyan), ev.Type, colorize(string(raw), ansiCyan))
		}
		events, err := normalizer.Consume(ev)
		if err != nil {
			return err
		}
		for _, event := range events {
			switch event.kind {
			case normalizedText:
				content, calls := textParser.Feed(event.text, false)
				if content != "" {
					if err := emitter.AddTextDelta(content); err != nil {
						return err
					}
				}
				for _, call := range calls {
					if err := collectToolCall(call); err != nil {
						return err
					}
				}
			case normalizedReasoning:
				content, calls := reasoningParser.Feed(event.text, false)
				if content != "" {
					if err := emitter.AddReasoningDelta(content); err != nil {
						return err
					}
				}
				for _, call := range calls {
					if err := collectToolCall(call); err != nil {
						return err
					}
				}
			case normalizedToolCall:
				if event.toolCall != nil {
					if err := collectToolCall(*event.toolCall); err != nil {
						return err
					}
				}
			case normalizedReasoningEnd:
				if err := flushParser(reasoningParser, true); err != nil {
					return err
				}
			case normalizedTextEnd:
				if err := flushParser(textParser, false); err != nil {
					return err
				}
			case normalizedFinish:
				if err := finishStream(event.finishReason, event.truncated); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("%s responses stream parse: %v", colorize("[ERROR]", ansiRed), err)
		if !emitter.Done() {
			if ferr := emitter.Fail("upstream stream error: " + err.Error()); ferr != nil {
				log.Printf("%s responses stream: Fail also failed: %v", colorize("[ERROR]", ansiRed), ferr)
			}
		}
	} else if !normalizer.finished {
		message := "upstream connection closed before a finish event"
		if endKind == streamEndDone {
			message = "upstream sent [DONE] before a finish event"
		}
		log.Printf("%s %s", colorize("[ERROR]", ansiRed), message)
		if !emitter.Done() {
			if ferr := emitter.Fail(message); ferr != nil {
				log.Printf("%s responses stream: Fail also failed: %v", colorize("[ERROR]", ansiRed), ferr)
			}
		}
	}

	promptTokens, completionTokens, cacheRead, cacheWrite := normalizer.Usage()
	usage.RecordFor(account, promptTokens, completionTokens, cacheRead, cacheWrite)
}

// handleResponses is the HTTP entrypoint for POST /v1/responses. It mirrors
// handleChatCompletions's shape: decode the wire request, adapt it onto the
// existing ChatRequest -> Command Code pipeline via responsesToChatRequest
// and dispatchToCC, then branch on stream:true to drive either
// handleResponsesStream or handleResponsesNonStream with the account that
// actually served the request.
func handleResponses(pool *AccountPool, cfg *Config, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxChatRequestBytes)

		var req ResponsesRequest
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

		chatReq, err := responsesToChatRequest(&req)
		if err != nil {
			var invalid *invalidRequestError
			if errors.As(err, &invalid) {
				writeError(w, http.StatusBadRequest, "invalid_request_error", invalid.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		disp, ok := dispatchToCC(w, r.Context(), pool, cfg, chatReq)
		if !ok {
			return
		}

		if chatReq.Stream {
			handleResponsesStream(w, disp.resp, chatReq.Model, usage, cfg, true, disp.account)
		} else {
			handleResponsesNonStream(w, disp.resp, chatReq.Model, usage, cfg, disp.account)
		}
		if err := usage.save(); err != nil {
			log.Printf("%s save usage: %v", colorize("[ERROR]", ansiRed), err)
		}
	}
}
