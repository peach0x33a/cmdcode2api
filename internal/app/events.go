package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	// truncated reports that at least one tool input arrived incomplete and had
	// to be repaired. The client must not be told the turn ended cleanly.
	truncated bool
}

type ccEventNormalizer struct {
	toolInputBuf      map[string]string
	toolInputToolName map[string]string
	// toolInputOrder and toolCalls preserve the model's first-seen order. A
	// higher-authority event replaces a provisional call in that same slot.
	toolInputOrder []string
	toolCalls      toolCallDeduper
	usage          Usage
	cacheRead      int
	cacheWrite     int
	finished       bool
	truncated      bool
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
		n.toolInputToolName[id] = ev.ToolName
		return nil, nil
	case "tool-input-delta":
		id := eventToolCallID(ev)
		if id == "" {
			return nil, fmt.Errorf("tool-input-delta missing tool call id")
		}
		n.trackToolInput(id)
		n.toolInputBuf[id] += ev.Delta
		if ev.ToolName != "" {
			n.toolInputToolName[id] = ev.ToolName
		}
		return nil, nil
	case "tool-input-end", "tool-input-available", "tool-error":
		call, ok, err := n.finishToolInput(ev)
		if err != nil {
			return nil, err
		}
		if ok {
			n.toolCalls.Add(call)
		}
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
		drained, err := n.drainToolInputs()
		if err != nil {
			return nil, err
		}
		for _, event := range drained {
			if event.toolCall != nil {
				n.toolCalls.Add(*event.toolCall)
			}
		}

		n.finished = true
		n.truncated = false
		out := make([]normalizedCCEvent, 0, len(n.toolCalls.kept)+1)
		for i := range n.toolCalls.kept {
			call := n.toolCalls.kept[i]
			if call.unsafeArguments() {
				n.truncated = true
			}
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

// trackToolInput registers an id on first sight so drainToolInputs can replay
// buffered inputs in arrival order.
func (n *ccEventNormalizer) trackToolInput(id string) {
	if _, seen := n.toolInputBuf[id]; seen {
		return
	}
	n.toolInputBuf[id] = ""
	n.toolInputOrder = append(n.toolInputOrder, id)
}

func (n *ccEventNormalizer) forgetToolInput(id string) {
	delete(n.toolInputBuf, id)
	delete(n.toolInputToolName, id)
	for i, pending := range n.toolInputOrder {
		if pending == id {
			n.toolInputOrder = append(n.toolInputOrder[:i], n.toolInputOrder[i+1:]...)
			break
		}
	}
}

// drainToolInputs emits every tool input still buffered when the stream ends.
// Without this, an upstream that never sends tool-input-end silently discards
// the call.
func (n *ccEventNormalizer) drainToolInputs() ([]normalizedCCEvent, error) {
	pending := append([]string(nil), n.toolInputOrder...)
	out := make([]normalizedCCEvent, 0, len(pending))
	for _, id := range pending {
		if n.toolInputBuf[id] == "" {
			// Nothing was ever buffered for this id; there is no call to recover.
			n.forgetToolInput(id)
			continue
		}
		log.Printf("%s stream ended with buffered tool input %q (tool %q); recovering",
			colorize("[WARN]", ansiYellow), id, n.toolInputToolName[id])
		call, ok, err := n.finishToolInput(CCStreamEvent{Type: "tool-input-end", ID: id})
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		toolCall := call
		out = append(out, normalizedCCEvent{kind: normalizedToolCall, toolCall: &toolCall})
	}
	return out, nil
}

func (n *ccEventNormalizer) finishToolInput(ev CCStreamEvent) (ToolCall, bool, error) {
	id := eventToolCallID(ev)
	if id == "" {
		return ToolCall{}, false, fmt.Errorf("%s missing tool call id", ev.Type)
	}
	name := n.toolInputToolName[id]
	if name == "" {
		name = ev.ToolName
	}
	raw := n.toolInputBuf[id]
	n.forgetToolInput(id)
	if name == "" {
		return ToolCall{}, false, fmt.Errorf("tool call %q missing function name", id)
	}

	repairKind := toolInputRepairNone
	if raw == "" {
		input := eventToolInput(ev)
		if input == nil {
			return ToolCall{}, false, nil
		}
		encoded, kind, err := encodeToolCallArguments(input, name)
		if err != nil {
			return ToolCall{}, false, fmt.Errorf("marshal tool input %q: %w", id, err)
		}
		raw = encoded
		repairKind = kind
		if kind != toolInputRepairNone {
			log.Printf("%s normalized inline tool input %q for tool %q with repair kind %s",
				colorize("[WARN]", ansiYellow), id, name, kind)
		}
	} else {
		encoded, kind, err := normalizeToolInput(raw, name)
		if err != nil {
			return ToolCall{}, false, fmt.Errorf("normalize tool input %q: %w", id, err)
		}
		if kind != toolInputRepairNone {
			log.Printf("%s normalized tool input %q for tool %q with repair kind %s",
				colorize("[WARN]", ansiYellow), id, name, kind)
		}
		raw = encoded
		repairKind = kind
	}

	return ToolCall{
		ID:         id,
		Type:       "function",
		source:     toolCallSourceToolInput,
		repairKind: repairKind,
		repaired:   repairKind != toolInputRepairNone,
		Function: CallFunc{
			Name:      name,
			Arguments: raw,
		},
	}, true, nil
}

func normalizeToolInput(raw, toolName string) (string, toolInputRepairKind, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", toolInputRepairNone, nil
	}
	if strings.HasPrefix(raw, "{") {
		if _, ok := decodeToolInputObject(raw, 0); ok {
			return raw, toolInputRepairNone, nil
		}
	}

	candidates := []string{raw}
	if escaped := escapeControlCharsInJSONStrings(raw); escaped != raw {
		candidates = append(candidates, escaped)
	}
	for index, candidate := range candidates {
		if input, ok := decodeToolInputObject(candidate, 0); ok {
			encoded, err := json.Marshal(input)
			if err != nil {
				return "", toolInputRepairNone, err
			}
			kind := toolInputRepairSyntax
			if index == 0 && strings.HasPrefix(candidate, "{") {
				kind = toolInputRepairNone
			}
			return string(encoded), kind, nil
		}
		if repaired, ok, truncated := tryRepairJSONDetailed(candidate); ok {
			if input, ok := decodeToolInputObject(repaired, 0); ok {
				encoded, err := json.Marshal(input)
				if err != nil {
					return "", toolInputRepairNone, err
				}
				kind := toolInputRepairSyntax
				if truncated {
					kind = toolInputRepairTruncated
				}
				return string(encoded), kind, nil
			}
		}
	}

	fallback := make(map[string]any)
	switch strings.ToLower(toolName) {
	case "bash", "exec", "command", "sh":
		fallback["command"] = raw
	case "read", "view", "cat":
		fallback["path"] = raw
	case "write":
		// Omitting content makes schema validation fail instead of truncating a
		// target file to zero bytes.
		fallback["path"] = raw
	default:
		fallback["command"] = raw
		fallback["input"] = raw
	}
	encoded, err := json.Marshal(fallback)
	if err != nil {
		return "", toolInputRepairNone, err
	}
	return string(encoded), toolInputRepairFallback, nil
}

func repairOrFallbackToolInput(raw string, toolName string) string {
	encoded, _, err := normalizeToolInput(raw, toolName)
	if err != nil {
		return "{}"
	}
	return encoded
}

func parseToolInputObject(raw string, depth int) (map[string]any, bool) {
	if depth > 2 {
		return nil, false
	}
	candidates := []string{raw}
	if escaped := escapeControlCharsInJSONStrings(raw); escaped != raw {
		candidates = append(candidates, escaped)
	}
	for _, candidate := range candidates {
		if input, ok := decodeToolInputObject(candidate, depth); ok {
			return input, true
		}
		if repaired, ok, _ := tryRepairJSONDetailed(candidate); ok {
			if input, ok := decodeToolInputObject(repaired, depth); ok {
				return input, true
			}
		}
	}
	return nil, false
}

func decodeToolInputObject(raw string, depth int) (map[string]any, bool) {
	if depth > 2 {
		return nil, false
	}
	// UseNumber keeps large integer identifiers exact through normalization.
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case string:
		return decodeToolInputObject(strings.TrimSpace(typed), depth+1)
	case []any:
		if len(typed) == 1 {
			if input, ok := typed[0].(map[string]any); ok {
				return input, true
			}
		}
	}
	return nil, false
}

func escapeControlCharsInJSONStrings(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) + 8)
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if !inString {
			b.WriteByte(ch)
			if ch == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			b.WriteByte(ch)
			escaped = true
			continue
		}
		if ch == '"' {
			b.WriteByte(ch)
			inString = false
			continue
		}
		switch ch {
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if ch < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, ch)
			} else {
				b.WriteByte(ch)
			}
		}
	}
	return b.String()
}

func tryRepairJSON(s string) (string, bool) {
	repaired, ok, _ := tryRepairJSONDetailed(s)
	return repaired, ok
}

func tryRepairJSONDetailed(s string) (string, bool, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		return "", false, false
	}

	var stack []byte
	b := make([]byte, 0, len(s)+8)
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			b = append(b, ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
			b = append(b, ch)
		case '{':
			stack = append(stack, '}')
			b = append(b, ch)
		case '[':
			stack = append(stack, ']')
			b = append(b, ch)
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == ch {
				stack = stack[:len(stack)-1]
				b = append(b, ch)
			}
			// Unmatched closing delimiters are syntax noise, not evidence that
			// the payload was cut off.
		default:
			b = append(b, ch)
		}
	}

	truncated := inString || len(stack) > 0
	if inString {
		if escaped && len(b) > 0 && b[len(b)-1] == '\\' {
			b = b[:len(b)-1]
		}
		b = append(b, '"')
	}

	for len(b) > 0 {
		last := b[len(b)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			b = b[:len(b)-1]
			continue
		}
		if last == ',' || last == ':' {
			truncated = true
			b = b[:len(b)-1]
			continue
		}
		break
	}

	for i := len(stack) - 1; i >= 0; i-- {
		b = append(b, stack[i])
	}

	candidate := string(b)
	if _, ok := decodeToolInputObject(candidate, 0); ok {
		return candidate, true, truncated
	}
	return "", false, false
}

// marshalToolInput renders a JSON object for OpenAI function.arguments.
func marshalToolInput(input any) (string, error) {
	if input == nil {
		return "{}", nil
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
	// A singleton array is accepted by the upstream normalizer as a recovery
	// shape, but OpenAI function.arguments itself must be an object.
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

// toolCallFromEvent converts an authoritative "tool-call" SSE event. It never
// rejects recoverable payloads: the upstream emits the same truncated or
// non-object inputs here that the buffered tool-input path repairs, and a hard
// error would abort the whole stream after the client already received text.
// The bool result is false only when the event cannot yield a call at all.
func toolCallFromEvent(ev CCStreamEvent) (ToolCall, bool, error) {
	id := eventToolCallID(ev)
	if strings.TrimSpace(id) == "" {
		synthesized, ok := newSyntheticCallID("call_recovered_")
		if !ok {
			log.Printf("%s tool-call event for tool %q missing id; no synthetic id available, skipping",
				colorize("[WARN]", ansiYellow), ev.ToolName)
			return ToolCall{}, false, nil
		}
		log.Printf("%s tool-call event for tool %q missing id; synthesized %s",
			colorize("[WARN]", ansiYellow), ev.ToolName, synthesized)
		id = synthesized
	}

	encoded, repairKind, err := encodeToolCallArguments(eventToolInput(ev), ev.ToolName)
	if err != nil {
		return ToolCall{}, false, fmt.Errorf("marshal tool call %q: %w", id, err)
	}
	if repairKind != toolInputRepairNone {
		log.Printf("%s normalized tool-call input for tool %q with repair kind %s",
			colorize("[WARN]", ansiYellow), ev.ToolName, repairKind)
	}
	call := ToolCall{
		ID:         id,
		Type:       "function",
		source:     toolCallSourceAuthoritative,
		repairKind: repairKind,
		repaired:   repairKind != toolInputRepairNone,
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

// encodeToolCallArguments renders an upstream tool input as an OpenAI
// function.arguments string. Clean objects pass through untouched; anything
// else — escaped strings, string-wrapped arrays, truncated JSON, plain text —
// runs through the same repair ladder as buffered tool inputs instead of
// failing the stream.
func encodeToolCallArguments(input any, toolName string) (string, toolInputRepairKind, error) {
	if text, ok := input.(string); ok {
		return normalizeToolInput(text, toolName)
	}
	encoded, err := marshalToolInput(input)
	if err == nil {
		return encoded, toolInputRepairNone, nil
	}
	// Scalar and multi-element JSON values still become text for the repair
	// ladder, which ends in a tool-shaped fallback object.
	asText, marshalErr := json.Marshal(input)
	if marshalErr != nil {
		return "", toolInputRepairNone, marshalErr
	}
	return normalizeToolInput(string(asText), toolName)
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
	return toolCallSemanticKey(call)
}

func toolCallSemanticKey(call ToolCall) string {
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		var trailing any
		if decoder.Decode(&trailing) == io.EOF {
			if canonical, err := json.Marshal(value); err == nil {
				return call.Function.Name + "\x00" + string(canonical)
			}
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(call.Function.Arguments)) == nil {
		return call.Function.Name + "\x00" + compact.String()
	}
	return call.Function.Name + "\x00" + call.Function.Arguments
}

type toolCallDeduper struct {
	kept   []ToolCall
	paired []bool
}

// Add preserves the first-seen slot while allowing a higher-authority
// representation to replace provisional arguments before wire emission.
func (d *toolCallDeduper) Add(candidate ToolCall) bool {
	for i, existing := range d.kept {
		if existing.ID != "" && existing.ID == candidate.ID {
			if candidate.source > existing.source {
				d.kept[i] = candidate
			}
			return false
		}
	}

	semanticKey := toolCallSemanticKey(candidate)
	for i, existing := range d.kept {
		crossRepresentation := existing.source != candidate.source ||
			existing.recoveredRawDSML != candidate.recoveredRawDSML
		if d.paired[i] || !crossRepresentation {
			continue
		}
		if toolCallSemanticKey(existing) == semanticKey {
			if candidate.source > existing.source {
				d.kept[i] = candidate
			}
			d.paired[i] = true
			return false
		}
	}
	d.kept = append(d.kept, candidate)
	d.paired = append(d.paired, false)
	return true
}

func (d *toolCallDeduper) hasUnsafeArguments() bool {
	for _, call := range d.kept {
		if call.unsafeArguments() {
			return true
		}
	}
	return false
}
