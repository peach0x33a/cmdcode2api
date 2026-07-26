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
	// toolInputOrder preserves arrival order so a drain at end-of-stream emits
	// buffered calls in the order the model produced them. Map iteration order
	// would otherwise shuffle them.
	toolInputOrder []string
	// heldCalls holds repair-derived calls until the stream ends, so an
	// authoritative tool-call event can supersede them.
	heldCalls  map[string]ToolCall
	heldOrder  []string
	usage      Usage
	cacheRead  int
	cacheWrite int
	finished   bool
	truncated  bool
}

func newCCEventNormalizer() *ccEventNormalizer {
	return &ccEventNormalizer{
		toolInputBuf:      make(map[string]string),
		toolInputToolName: make(map[string]string),
		heldCalls:         make(map[string]ToolCall),
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
		// This event carries the upstream's own complete input, so it supersedes
		// anything reconstructed from deltas for the same id.
		n.dropHeldCall(call.ID)
		return []normalizedCCEvent{{kind: normalizedToolCall, toolCall: &call}}, nil
	case "tool-input-start":
		id := eventToolCallID(ev)
		if id != "" {
			n.trackToolInput(id)
			n.toolInputBuf[id] = ""
			n.toolInputToolName[id] = ev.ToolName
		}
		return nil, nil
	case "tool-input-delta":
		id := eventToolCallID(ev)
		if id != "" {
			n.trackToolInput(id)
			n.toolInputBuf[id] += ev.Delta
			if ev.ToolName != "" {
				n.toolInputToolName[id] = ev.ToolName
			}
		}
		return nil, nil
	case "tool-input-end", "tool-input-available", "tool-error":
		// tool-error carries the same identifiers as tool-input-end and can
		// arrive in its place. Treat it as a terminator so the buffered input is
		// emitted instead of being dropped; a duplicate is suppressed downstream
		// by toolCallDeduper.
		call, ok, err := n.finishToolInput(ev)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		if call.repaired {
			// The deltas were cut off and only parse because repair patched
			// them. The upstream usually follows with a tool-call event holding
			// the complete input, so hold this guess back rather than racing it
			// to the client — once emitted, a streamed call cannot be corrected.
			n.holdCall(call)
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
		// The upstream can end a stream while tool inputs are still buffered,
		// for example when it is cut off by the output-token cap. Emit them
		// rather than dropping the model's intent on the floor.
		out, err := n.drainToolInputs()
		if err != nil {
			return nil, err
		}
		return append(out, normalizedCCEvent{
			kind:         normalizedFinish,
			finishReason: normalizeFinishReason(ev.FinishReason),
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

// holdCall parks a repair-derived call until the stream ends, so a later
// authoritative tool-call event for the same id can replace it.
func (n *ccEventNormalizer) holdCall(call ToolCall) {
	if _, seen := n.heldCalls[call.ID]; !seen {
		n.heldOrder = append(n.heldOrder, call.ID)
	}
	n.heldCalls[call.ID] = call
}

func (n *ccEventNormalizer) dropHeldCall(id string) {
	if _, held := n.heldCalls[id]; !held {
		return
	}
	delete(n.heldCalls, id)
	for i, pending := range n.heldOrder {
		if pending == id {
			n.heldOrder = append(n.heldOrder[:i], n.heldOrder[i+1:]...)
			break
		}
	}
}

// releaseHeldCalls emits the repair-derived calls no authoritative event
// superseded. They are the model's intent as best it can be reconstructed, so
// dropping them would lose the call entirely.
func (n *ccEventNormalizer) releaseHeldCalls() []normalizedCCEvent {
	out := make([]normalizedCCEvent, 0, len(n.heldOrder))
	for _, id := range n.heldOrder {
		call := n.heldCalls[id]
		delete(n.heldCalls, id)
		toolCall := call
		// A repaired guess is only a truncation the client must know about once
		// it is the version actually delivered. If an authoritative event had
		// superseded it, it would never reach here.
		n.markTruncated(toolCall)
		out = append(out, normalizedCCEvent{kind: normalizedToolCall, toolCall: &toolCall})
	}
	n.heldOrder = nil
	return out
}

func (n *ccEventNormalizer) markTruncated(call ToolCall) {
	if call.repaired {
		n.truncated = true
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
		log.Printf("%s stream ended with tool input %q (tool %q) still buffered; recovering it",
			colorize("[WARN]", ansiYellow), id, n.toolInputToolName[id])
		call, ok, err := n.finishToolInput(CCStreamEvent{Type: "tool-input-end", ID: id})
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		toolCall := call
		n.markTruncated(toolCall)
		out = append(out, normalizedCCEvent{kind: normalizedToolCall, toolCall: &toolCall})
	}
	// Nothing more can supersede a held call once the stream is over.
	return append(out, n.releaseHeldCalls()...), nil
}

func (n *ccEventNormalizer) finishToolInput(ev CCStreamEvent) (ToolCall, bool, error) {
	id := eventToolCallID(ev)
	name := n.toolInputToolName[id]
	if name == "" {
		name = ev.ToolName
	}
	raw := n.toolInputBuf[id]
	n.forgetToolInput(id)
	repaired := false

	if raw == "" {
		input := eventToolInput(ev)
		if input == nil {
			return ToolCall{}, false, nil
		}
		encoded, err := marshalToolInput(input)
		if err != nil {
			return ToolCall{}, false, fmt.Errorf("marshal tool input %q: %w", id, err)
		}
		raw = encoded
	} else {
		var input any
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			log.Printf("%s parse tool input %q for tool %q failed: %v, attempting repair/fallback", colorize("[WARN]", ansiYellow), id, name, err)
			repaired = true
			raw = repairOrFallbackToolInput(raw, name)
		}
	}

	if id == "" || name == "" {
		log.Printf("%s dropping tool call with empty id/name (id=%q name=%q, %d bytes of arguments)",
			colorize("[WARN]", ansiYellow), id, name, len(raw))
		return ToolCall{}, false, nil
	}
	return ToolCall{
		ID:       id,
		Type:     "function",
		repaired: repaired,
		Function: CallFunc{
			Name:      name,
			Arguments: raw,
		},
	}, true, nil
}

func repairOrFallbackToolInput(raw string, toolName string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}

	// Match command-code's normalization order: accept objects, recursively
	// unwrap JSON-stringified objects, escape bare control characters, repair
	// truncated JSON, and unwrap a single-object array.
	if input, ok := parseToolInputObject(raw, 0); ok {
		encoded, err := json.Marshal(input)
		if err == nil {
			return string(encoded)
		}
	}

	// Fallback to wrapping raw non-JSON text as a valid tool object.
	fallback := make(map[string]any)
	switch strings.ToLower(toolName) {
	case "bash", "exec", "command", "sh":
		fallback["command"] = raw
	case "read", "view", "cat":
		fallback["path"] = raw
	case "write":
		fallback["path"] = raw
		fallback["content"] = ""
	default:
		fallback["command"] = raw
		fallback["input"] = raw
	}

	encoded, err := json.Marshal(fallback)
	if err != nil {
		return "{}"
	}
	return string(encoded)
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
		if repaired, ok := tryRepairJSON(candidate); ok {
			if input, ok := decodeToolInputObject(repaired, depth); ok {
				return input, true
			}
		}
	}
	return nil, false
}

func decodeToolInputObject(raw string, depth int) (map[string]any, bool) {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case string:
		return parseToolInputObject(strings.TrimSpace(typed), depth+1)
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
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		return "", false
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
			// Ignore unmatched closing delimiters. This repairs a common model
			// output shape where an otherwise complete tool object ends in `}}`.
		default:
			b = append(b, ch)
		}
	}

	if inString {
		if escaped && len(b) > 0 && b[len(b)-1] == '\\' {
			b = b[:len(b)-1]
		}
		b = append(b, '"')
	}

	for len(b) > 0 {
		last := b[len(b)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' || last == ',' || last == ':' {
			b = b[:len(b)-1]
		} else {
			break
		}
	}

	for i := len(stack) - 1; i >= 0; i-- {
		b = append(b, stack[i])
	}

	candidate := string(b)
	var v any
	if json.Unmarshal([]byte(candidate), &v) == nil {
		return candidate, true
	}

	return "", false
}

// marshalToolInput renders an event's tool input as the JSON object the OpenAI
// arguments field must contain.
//
// The upstream is inconsistent: tool-call events carry input as an object,
// while tool-error events carry the same payload as a pre-encoded JSON string.
// Marshalling a string yields a quoted string, so the client's json.loads
// returns text instead of arguments and the call is unusable. Pass through a
// string that is already valid JSON.
func marshalToolInput(input any) (string, error) {
	if text, ok := input.(string); ok {
		trimmed := strings.TrimSpace(text)
		if json.Valid([]byte(trimmed)) {
			return trimmed, nil
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func toolCallFromEvent(ev CCStreamEvent) (ToolCall, error) {
	encoded, err := marshalToolInput(eventToolInput(ev))
	if err != nil {
		return ToolCall{}, fmt.Errorf("marshal tool call %q: %w", eventToolCallID(ev), err)
	}
	return ToolCall{
		ID:   eventToolCallID(ev),
		Type: "function",
		Function: CallFunc{
			Name:      ev.ToolName,
			Arguments: encoded,
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

// Add performs one-to-one cross-representation deduplication. One structured
// call suppresses at most one equivalent recovered DSML call, preserving the
// multiplicity of intentional repeated invokes.
func (d *toolCallDeduper) Add(candidate ToolCall) bool {
	for _, existing := range d.kept {
		if toolCallKey(existing) == toolCallKey(candidate) {
			return false
		}
	}
	semanticKey := toolCallSemanticKey(candidate)
	for i, existing := range d.kept {
		if d.paired[i] || existing.recoveredRawDSML == candidate.recoveredRawDSML {
			continue
		}
		if toolCallSemanticKey(existing) == semanticKey {
			d.paired[i] = true
			return false
		}
	}
	d.kept = append(d.kept, candidate)
	d.paired = append(d.paired, false)
	return true
}
