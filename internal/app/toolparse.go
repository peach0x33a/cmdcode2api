package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// ToolCallParser — extracts OpenAI-format ToolCall entries from embedded
// plain-text "Assistant requested tool ..." lines inside SSE text-delta events.
//
// The inbound format (from cc.go:263):
//
//	Assistant requested tool {name} ({id}) with arguments: {json}
//
// Invalid variant (from cc.go:255):
//
//	Assistant requested tool {name} ({id}) with invalid arguments: {error}
//
// Usage:
//
//	p := NewToolCallParser()
//	for each SSE text-delta chunk:
//	    content, calls := p.Feed(chunk, false)
//	    // forward content, emit calls as tool_call deltas
//	content, calls := p.Feed("", true)  // flush remainder
// ---------------------------------------------------------------------------

// toolCallPrefixRe matches the prefix up to and including "arguments:".
//
// Groups:
//
//	1 — tool name
//	2 — tool ID
//	3 — "invalid " (empty when valid)
var toolCallPrefixRe = regexp.MustCompile(
	`(?i)Assistant\s+requested\s+tool\s+([^\s(]+)\s*\(([^)]+)\)\s+with\s+(invalid\s+)?arguments:`)

const defaultMaxBuf = 128 * 1024 // 128 KB

// ToolCallParser buffers incoming text chunks, scanning for embedded
// tool-call lines.  Non-tool text is returned as ordinary content;
// recognised tool calls are returned as []ToolCall.
type ToolCallParser struct {
	buf     strings.Builder
	maxSize int
}

// NewToolCallParser returns a ready-to-use parser with a 128 KB cap.
func NewToolCallParser() *ToolCallParser {
	return &ToolCallParser{maxSize: defaultMaxBuf}
}

// Feed ingests a chunk of text.  Call it for every SSE text-delta payload.
// When the stream ends call it once more with done=true to flush any
// remaining buffered text.
//
// Return values:
//
//	content — ordinary text that should be forwarded as-is
//	calls   — tool calls extracted from this chunk
func (p *ToolCallParser) Feed(chunk string, done bool) (content string, calls []ToolCall) {
	p.buf.WriteString(chunk)

	raw := p.buf.String()

	// ----- overflow protection -------------------------------------------
	if len(raw) > p.maxSize {
		p.buf.Reset()
		return raw, nil
	}

	// ----- scan buffer for tool-call lines -------------------------------
	var contentBuf strings.Builder
	pos := 0
	preserveRemaining := false

	for pos < len(raw) {
		rest := raw[pos:]

		loc := toolCallPrefixRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			if partial := partialToolCallPrefixStart(rest); partial >= 0 {
				contentBuf.WriteString(rest[:partial])
				pos += partial
				preserveRemaining = true
			} else {
				contentBuf.WriteString(rest)
				pos = len(raw)
			}
			break // no more complete tool-call prefixes
		}

		// flush everything before the match as content
		if loc[0] > 0 {
			contentBuf.WriteString(rest[:loc[0]])
		}
		matchStart := pos + loc[0]

		// captured groups (relative to rest, which is raw[pos:])
		prefixEnd := pos + loc[1] // right after "arguments:"
		toolName := rest[loc[2]:loc[3]]
		toolID := rest[loc[4]:loc[5]]
		isInvalid := loc[6] != -1 // group 3 captured "invalid "

		if isInvalid {
			// ---- invalid arguments variant ----
			lineEnd := scanLineEnd(raw, prefixEnd)
			contentBuf.WriteString(raw[pos+loc[0] : lineEnd])
			pos = lineEnd
			continue
		}

		// ---- valid tool call: extract JSON from raw[pos+loc[0]:] ---------
		jsonStart := prefixEnd
		// skip whitespace between "arguments:" and JSON value
		for jsonStart < len(raw) && isToolCallSpace(raw[jsonStart]) {
			jsonStart++
		}

		if jsonStart >= len(raw) {
			// JSON has not arrived yet — keep buffering
			pos = matchStart
			preserveRemaining = true
			break
		}

		jsonEnd, complete := scanJSON(raw, jsonStart)
		if !complete && !done {
			// still waiting for more data
			pos = matchStart
			preserveRemaining = true
			break
		}

		rawJSON := strings.TrimSpace(raw[jsonStart:jsonEnd])

		// validate JSON before emitting a ToolCall
		var v any
		if err := json.Unmarshal([]byte(rawJSON), &v); err != nil {
			// malformed — treat the whole line as content
			contentBuf.WriteString(raw[pos+loc[0] : jsonEnd])
			pos = jsonEnd
			continue
		}

		calls = append(calls, ToolCall{
			ID:   toolID,
			Type: "function",
			Function: CallFunc{
				Name:      toolName,
				Arguments: rawJSON,
			},
		})

		pos = jsonEnd
	}

	// ----- preserve unprocessed tail in buffer ---------------------------
	remaining := raw[pos:]
	if done {
		contentBuf.WriteString(remaining)
		p.buf.Reset()
	} else if preserveRemaining {
		p.buf.Reset()
		p.buf.WriteString(remaining)
	} else {
		p.buf.Reset()
	}

	return contentBuf.String(), calls
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// scanLineEnd returns the index of the next \n in s starting at offset,
// or len(s) if none exists (the line runs to EOF).
func scanLineEnd(s string, offset int) int {
	if idx := strings.IndexByte(s[offset:], '\n'); idx >= 0 {
		return offset + idx + 1 // include the newline
	}
	return len(s)
}

// partialToolCallPrefixStart returns the first byte offset whose suffix can
// still grow into a tool-call prefix. This avoids flushing long call IDs when
// the upstream splits the prefix before "arguments:" or before its JSON value.
func partialToolCallPrefixStart(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != 'a' && s[i] != 'A' {
			continue
		}
		if couldBeToolCallPrefix(s[i:]) {
			return i
		}
	}
	return -1
}

func couldBeToolCallPrefix(s string) bool {
	i := 0
	for _, word := range []string{"Assistant", "requested", "tool"} {
		complete, ok := consumeWordPrefix(s, &i, word)
		if !ok || !complete {
			return ok
		}
		if !consumeRequiredSpace(s, &i) {
			return i == len(s)
		}
	}

	nameStart := i
	for i < len(s) && !isToolCallSpace(s[i]) && s[i] != '(' {
		i++
	}
	if i == nameStart || i == len(s) {
		return i == len(s)
	}
	consumeSpaces(s, &i)
	if i == len(s) {
		return true
	}
	if s[i] != '(' {
		return false
	}
	i++

	idStart := i
	for i < len(s) && s[i] != ')' {
		i++
	}
	if i == len(s) {
		return true
	}
	if i == idStart {
		return false
	}
	i++
	consumeSpaces(s, &i)

	complete, ok := consumeWordPrefix(s, &i, "with")
	if !ok || !complete {
		return ok
	}
	if !consumeRequiredSpace(s, &i) {
		return i == len(s)
	}

	if i < len(s) && (s[i] == 'i' || s[i] == 'I') {
		complete, ok = consumeWordPrefix(s, &i, "invalid")
		if !ok || !complete {
			return ok
		}
		if !consumeRequiredSpace(s, &i) {
			return i == len(s)
		}
	}

	complete, ok = consumeWordPrefix(s, &i, "arguments")
	if !ok || !complete {
		return ok
	}
	return i == len(s) || s[i] == ':'
}

func consumeWordPrefix(s string, pos *int, word string) (complete, ok bool) {
	remaining := len(s) - *pos
	compareLen := len(word)
	if remaining < compareLen {
		compareLen = remaining
	}
	if !strings.EqualFold(s[*pos:*pos+compareLen], word[:compareLen]) {
		return false, false
	}
	*pos += compareLen
	return compareLen == len(word), true
}

func consumeRequiredSpace(s string, pos *int) bool {
	if *pos >= len(s) || !isToolCallSpace(s[*pos]) {
		return false
	}
	consumeSpaces(s, pos)
	return true
}

func consumeSpaces(s string, pos *int) {
	for *pos < len(s) && isToolCallSpace(s[*pos]) {
		(*pos)++
	}
}

func isToolCallSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

// scanJSON determines the end of a JSON value starting at s[start].
// It returns (end, complete) where end is one-past-the-last-byte of the
// value and complete is false when the value is truncated.
//
// Uses byte-level brace/bracket counting rather than a greedy regex so that
// nested objects and escaped strings are handled correctly.
func scanJSON(s string, start int) (end int, complete bool) {
	if start >= len(s) {
		return start, false
	}

	switch s[start] {
	case '{':
		return scanDelimited(s, start, '{', '}')
	case '[':
		return scanDelimited(s, start, '[', ']')
	case '"':
		return scanString(s, start)
	case 'n', 't', 'f':
		// null / true / false — consume identifier
		return scanLiteral(s, start)
	default:
		// number or unexpected character — consume token
		return scanNumber(s, start)
	}
}

// scanDelimited uses a depth counter to find the matching close-delimiter,
// skipping over strings so that delimiters inside quoted text are ignored.
func scanDelimited(s string, start int, open, close byte) (end int, complete bool) {
	depth := 1
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, true
			}
		case '"':
			// skip escaped string content
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++ // skip escaped char
				}
				i++
			}
			// i now points at closing " (or past end)
		}
	}
	return len(s), false
}

// scanString finds the closing unescaped double-quote.
func scanString(s string, start int) (end int, complete bool) {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // skip escaped char
			continue
		}
		if s[i] == '"' {
			return i + 1, true
		}
	}
	return len(s), false
}

// scanLiteral consumes a JSON literal (null / true / false).
func scanLiteral(s string, start int) (end int, complete bool) {
	i := start
	for i < len(s) && isJSONIdent(rune(s[i])) {
		i++
	}
	return i, true // literals are always "complete" at the current boundary
}

// scanNumber consumes a JSON number token (including negative, decimal, and
// scientific notation).
func scanNumber(s string, start int) (end int, complete bool) {
	i := start
	if i < len(s) && s[i] == '-' {
		i++
	}
	// integer part
	if i < len(s) && s[i] >= '0' && s[i] <= '9' {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	// fractional part
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	// exponent part
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	return i, true
}

func isJSONIdent(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
