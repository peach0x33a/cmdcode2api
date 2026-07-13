package app

import (
	"encoding/json"
	"fmt"
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
}

type ccEventNormalizer struct {
	toolInputBuf      map[string]string
	toolInputToolName map[string]string
	usage             Usage
	cacheRead         int
	cacheWrite        int
	finished          bool
}

func newCCEventNormalizer() *ccEventNormalizer {
	return &ccEventNormalizer{
		toolInputBuf:      make(map[string]string),
		toolInputToolName: make(map[string]string),
	}
}

func (n *ccEventNormalizer) Consume(ev CCStreamEvent) ([]normalizedCCEvent, error) {
	switch ev.Type {
	case "text-delta":
		return []normalizedCCEvent{{kind: normalizedText, text: streamEventText(ev)}}, nil
	case "reasoning-delta":
		return []normalizedCCEvent{{kind: normalizedReasoning, text: streamEventText(ev)}}, nil
	case "tool-call":
		call, err := toolCallFromEvent(ev)
		if err != nil {
			return nil, err
		}
		return []normalizedCCEvent{{kind: normalizedToolCall, toolCall: &call}}, nil
	case "tool-input-start":
		id := eventToolCallID(ev)
		if id != "" {
			n.toolInputBuf[id] = ""
			n.toolInputToolName[id] = ev.ToolName
		}
		return nil, nil
	case "tool-input-delta":
		id := eventToolCallID(ev)
		if id != "" {
			n.toolInputBuf[id] += ev.Delta
			if ev.ToolName != "" {
				n.toolInputToolName[id] = ev.ToolName
			}
		}
		return nil, nil
	case "tool-input-end", "tool-input-available":
		call, ok, err := n.finishToolInput(ev)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []normalizedCCEvent{{kind: normalizedToolCall, toolCall: &call}}, nil
	case "finish-step":
		if n.finished {
			return nil, nil
		}
		if ev.Usage != nil {
			n.setUsage(ev.Usage)
		}
		return nil, nil
	case "finish":
		if ev.TotalUsage != nil {
			n.setUsage(ev.TotalUsage)
		}
		n.finished = true
		return []normalizedCCEvent{{
			kind:         normalizedFinish,
			finishReason: normalizeFinishReason(ev.FinishReason),
			usage:        n.usage,
		}}, nil
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

func (n *ccEventNormalizer) finishToolInput(ev CCStreamEvent) (ToolCall, bool, error) {
	id := eventToolCallID(ev)
	name := n.toolInputToolName[id]
	if name == "" {
		name = ev.ToolName
	}
	raw := n.toolInputBuf[id]
	delete(n.toolInputBuf, id)
	delete(n.toolInputToolName, id)

	if raw == "" {
		input := eventToolInput(ev)
		if input == nil {
			return ToolCall{}, false, nil
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return ToolCall{}, false, fmt.Errorf("marshal tool input %q: %w", id, err)
		}
		raw = string(encoded)
	} else {
		var input any
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return ToolCall{}, false, fmt.Errorf("parse tool input %q: %w", id, err)
		}
	}

	if id == "" || name == "" {
		return ToolCall{}, false, nil
	}
	return ToolCall{
		ID:   id,
		Type: "function",
		Function: CallFunc{
			Name:      name,
			Arguments: raw,
		},
	}, true, nil
}

func toolCallFromEvent(ev CCStreamEvent) (ToolCall, error) {
	encoded, err := json.Marshal(eventToolInput(ev))
	if err != nil {
		return ToolCall{}, fmt.Errorf("marshal tool call %q: %w", eventToolCallID(ev), err)
	}
	return ToolCall{
		ID:   eventToolCallID(ev),
		Type: "function",
		Function: CallFunc{
			Name:      ev.ToolName,
			Arguments: string(encoded),
		},
	}, nil
}

func eventToolCallID(ev CCStreamEvent) string {
	if ev.ToolCallID != "" {
		return ev.ToolCallID
	}
	return ev.ID
}

func eventToolInput(ev CCStreamEvent) any {
	if ev.Input != nil {
		return ev.Input
	}
	if ev.Args != nil {
		return ev.Args
	}
	return ev.Arguments
}

func toolCallKey(call ToolCall) string {
	if call.ID != "" {
		return call.ID
	}
	return call.Function.Name + "\x00" + call.Function.Arguments
}

func appendUniqueToolCall(calls []ToolCall, call ToolCall) []ToolCall {
	key := toolCallKey(call)
	for _, existing := range calls {
		if toolCallKey(existing) == key {
			return calls
		}
	}
	return append(calls, call)
}
