package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type normalizedEventKind uint8

const (
	normalizedText normalizedEventKind = iota + 1
	normalizedReasoning
	normalizedToolCall
	normalizedTextEnd
	normalizedReasoningEnd
	normalizedFinish
)

type normalizedCCEvent struct {
	kind         normalizedEventKind
	text         string
	toolCall     *ToolCall
	finishReason string
	usage        Usage
	// truncated reports an upstream length finish. No incomplete input is ever
	// repaired into an executable call.
	truncated bool
}

type ccEventNormalizer struct {
	toolInputBuf map[string]string
	// toolInputOrder makes finish-time provisional cleanup deterministic.
	toolInputOrder []string
	toolCalls      toolCallDeduper
	usage          Usage
	cacheRead      int
	cacheWrite     int
	finished       bool
	truncated      bool
}

func newCCEventNormalizer() *ccEventNormalizer {
	return &ccEventNormalizer{toolInputBuf: make(map[string]string)}
}

func (n *ccEventNormalizer) Consume(ev CCStreamEvent) ([]normalizedCCEvent, error) {
	switch ev.Type {
	case "text-delta":
		return []normalizedCCEvent{{kind: normalizedText, text: streamEventText(ev)}}, nil
	case "reasoning-delta":
		return []normalizedCCEvent{{kind: normalizedReasoning, text: streamEventText(ev)}}, nil
	case "tool-call":
		call, ok, err := toolCallFromEvent(ev)
		if err != nil {
			return nil, err
		}
		if ok {
			n.toolCalls.Add(call)
		}
		return nil, nil
	case "tool-input-start":
		id := eventToolCallID(ev)
		if id == "" {
			return nil, fmt.Errorf("tool-input-start missing tool call id")
		}
		n.trackToolInput(id)
		n.toolInputBuf[id] = ""
		return nil, nil
	case "tool-input-delta":
		id := eventToolCallID(ev)
		if id == "" {
			return nil, fmt.Errorf("tool-input-delta missing tool call id")
		}
		n.trackToolInput(id)
		n.toolInputBuf[id] += ev.Delta
		return nil, nil
	case "tool-input-end", "tool-input-available":
		// These events are only provisional telemetry. Validate and discard the
		// buffer, but never promote it across the execution boundary. Only a
		// final authoritative tool-call event can add a call.
		_ = n.finishToolInput(ev)
		return nil, nil
	case "tool-error":
		n.forgetToolInput(eventToolCallID(ev))
		return nil, nil
	case "finish-step":
		if n.finished {
			return nil, nil
		}
		if ev.Usage != nil {
			n.setUsage(ev.Usage)
		}
		return nil, nil
	case "finish":
		if n.finished {
			return nil, nil
		}
		reason := ev.FinishReason
		if strings.TrimSpace(reason) == "" {
			reason = ev.RawFinishReason
		}
		finishReason, err := normalizeFinishReason(reason)
		if err != nil {
			return nil, err
		}
		if ev.TotalUsage != nil {
			n.setUsage(ev.TotalUsage)
		}
		// A buffered input without a structured end event is incomplete. Drop it
		// rather than guessing where its JSON or business content should end.
		for _, id := range append([]string(nil), n.toolInputOrder...) {
			n.forgetToolInput(id)
		}

		n.finished = true
		n.truncated = finishReason == "length"
		out := make([]normalizedCCEvent, 0, len(n.toolCalls.kept)+1)
		for i := range n.toolCalls.kept {
			call := n.toolCalls.kept[i]
			toolCall := call
			out = append(out, normalizedCCEvent{kind: normalizedToolCall, toolCall: &toolCall})
		}
		return append(out, normalizedCCEvent{
			kind:         normalizedFinish,
			finishReason: finishReason,
			usage:        n.usage,
			truncated:    n.truncated,
		}), nil
	case "text-end":
		return []normalizedCCEvent{{kind: normalizedTextEnd}}, nil
	case "reasoning-end":
		return []normalizedCCEvent{{kind: normalizedReasoningEnd}}, nil
	case "error":
		return nil, fmt.Errorf("cc stream error: %v", ev.Error)
	default:
		return nil, nil
	}
}

func (n *ccEventNormalizer) Usage() (prompt, completion, cacheRead, cacheWrite int) {
	return n.usage.PromptTokens, n.usage.CompletionTokens, n.cacheRead, n.cacheWrite
}

func (n *ccEventNormalizer) FinalUsage() (prompt, completion, cacheRead, cacheWrite int) {
	if !n.finished {
		return 0, 0, 0, 0
	}
	return n.Usage()
}

func (n *ccEventNormalizer) FinalUsageInfo() Usage {
	if !n.finished {
		return Usage{}
	}
	return n.usage
}

func (n *ccEventNormalizer) setUsage(usage *CCUsage) {
	n.usage.PromptTokens = usage.InputTokens
	n.usage.CompletionTokens = usage.OutputTokens
	if usage.TotalTokens > 0 {
		n.usage.TotalTokens = usage.TotalTokens
	} else {
		n.usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.InputTokenDetails != nil {
		n.cacheRead = usage.InputTokenDetails.CacheReadTokens
		n.cacheWrite = usage.InputTokenDetails.CacheWriteTokens
	}
}

// trackToolInput registers an id on first sight so provisional state can be
// discarded deterministically at end/error/finish.
func (n *ccEventNormalizer) trackToolInput(id string) {
	if _, seen := n.toolInputBuf[id]; seen {
		return
	}
	n.toolInputBuf[id] = ""
	n.toolInputOrder = append(n.toolInputOrder, id)
}

func (n *ccEventNormalizer) forgetToolInput(id string) {
	delete(n.toolInputBuf, id)
	for i, pending := range n.toolInputOrder {
		if pending == id {
			n.toolInputOrder = append(n.toolInputOrder[:i], n.toolInputOrder[i+1:]...)
			break
		}
	}
}

func (n *ccEventNormalizer) finishToolInput(ev CCStreamEvent) error {
	id := eventToolCallID(ev)
	if id == "" {
		return fmt.Errorf("%s missing tool call id", ev.Type)
	}
	raw, tracked := n.toolInputBuf[id]
	n.forgetToolInput(id)

	if raw != "" {
		if _, err := normalizeToolInput(raw); err != nil {
			return fmt.Errorf("normalize provisional tool input %q: %w", id, err)
		}
		return nil
	}
	input, present := eventToolInput(ev)
	if !present {
		if tracked {
			return fmt.Errorf("provisional tool input %q is empty", id)
		}
		return nil
	}
	if _, err := encodeToolCallArguments(input); err != nil {
		return fmt.Errorf("marshal provisional tool input %q: %w", id, err)
	}
	return nil
}

func normalizeToolInput(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("tool arguments must not be empty")
	}
	if _, ok := decodeToolInputObject(raw, 0); !ok {
		return "", fmt.Errorf("tool arguments must be one complete JSON object")
	}
	return raw, nil
}

func decodeToolInputObject(raw string, _ int) (map[string]any, bool) {
	// UseNumber keeps large integer identifiers exact through normalization.
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return value, true
}

// marshalToolInput renders a JSON object for OpenAI function.arguments.
func marshalToolInput(input any) (string, error) {
	if input == nil {
		return "", fmt.Errorf("tool arguments must be present and non-null")
	}
	if text, ok := input.(string); ok {
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, "{") {
			return "", fmt.Errorf("tool arguments must be a JSON object")
		}
		if _, ok := decodeToolInputObject(trimmed, 0); !ok {
			return "", fmt.Errorf("tool arguments must be one valid JSON object")
		}
		return trimmed, nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	object, ok := decodeToolInputObject(string(encoded), 0)
	if !ok {
		return "", fmt.Errorf("tool arguments must be a JSON object")
	}
	// Re-marshal the validated object with json.Number values intact so large
	// integer identifiers are not rounded through float64.
	canonical, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func validateToolCall(call ToolCall) error {
	if strings.TrimSpace(call.ID) == "" {
		return fmt.Errorf("tool call missing id")
	}
	if strings.TrimSpace(call.Function.Name) == "" {
		return fmt.Errorf("tool call %q missing function name", call.ID)
	}
	if call.Type != "function" {
		return fmt.Errorf("tool call %q has unsupported type %q", call.ID, call.Type)
	}
	arguments := strings.TrimSpace(call.Function.Arguments)
	if !strings.HasPrefix(arguments, "{") {
		return fmt.Errorf("tool call %q arguments must be one JSON object", call.ID)
	}
	if _, ok := decodeToolInputObject(arguments, 0); !ok {
		return fmt.Errorf("tool call %q arguments must be one JSON object", call.ID)
	}
	return nil
}

// toolCallFromEvent converts an authoritative structured tool-call event. It
// fails closed: missing IDs/names and malformed or non-object arguments are
// protocol errors and never become executable calls.
func toolCallFromEvent(ev CCStreamEvent) (ToolCall, bool, error) {
	id := eventToolCallID(ev)
	if strings.TrimSpace(id) == "" {
		return ToolCall{}, false, fmt.Errorf("tool-call event missing tool call id")
	}

	input, present := eventToolInput(ev)
	if !present {
		return ToolCall{}, false, fmt.Errorf("tool-call event %q missing input", id)
	}
	encoded, err := encodeToolCallArguments(input)
	if err != nil {
		return ToolCall{}, false, fmt.Errorf("marshal tool call %q: %w", id, err)
	}
	call := ToolCall{
		ID:     id,
		Type:   "function",
		source: toolCallSourceAuthoritative,
		Function: CallFunc{
			Name:      ev.ToolName,
			Arguments: encoded,
		},
	}
	if err := validateToolCall(call); err != nil {
		return ToolCall{}, false, err
	}
	return call, true, nil
}

// encodeToolCallArguments renders a complete upstream JSON object as OpenAI
// function.arguments. No truncation repair, scalar wrapping, or fallback is
// permitted on the execution boundary.
func encodeToolCallArguments(input any) (string, error) {
	if text, ok := input.(string); ok {
		return normalizeToolInput(text)
	}
	return marshalToolInput(input)
}

func eventToolCallID(ev CCStreamEvent) string {
	if ev.ToolCallID != "" {
		return ev.ToolCallID
	}
	return ev.ID
}

func eventToolInput(ev CCStreamEvent) (any, bool) {
	if ev.inputPresent || ev.Input != nil {
		return ev.Input, true
	}
	if ev.argsPresent || ev.Args != nil {
		return ev.Args, true
	}
	if ev.argumentsPresent || ev.Arguments != nil {
		return ev.Arguments, true
	}
	return nil, false
}

type toolCallDeduper struct {
	kept []ToolCall
}

// Add deduplicates authoritative calls by stable tool-call id while preserving
// first-seen order.
func (d *toolCallDeduper) Add(candidate ToolCall) bool {
	for i, existing := range d.kept {
		if existing.ID != "" && existing.ID == candidate.ID {
			if candidate.source > existing.source {
				d.kept[i] = candidate
			}
			return false
		}
	}
	d.kept = append(d.kept, candidate)
	return true
}
