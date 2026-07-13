package app

import (
	"fmt"
	"strings"
	"testing"
)

type feed struct {
	chunk string
	done  bool
}

func TestToolCallParser(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(p *ToolCallParser)
		feeds       []feed
		wantContent string
		wantCalls   []ToolCall
	}{
		// -----------------------------------------------------------------------
		//  1. Single tool call in one chunk
		// -----------------------------------------------------------------------
		{
			name: "1_single_tool_call_in_one_chunk",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_00_abc) with arguments: {"file":"test.go"}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_00_abc", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"file":"test.go"}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  2. Tool call fragmented across chunks (3+ Feed calls + flush)
		// -----------------------------------------------------------------------
		{
			name: "2_fragmented_across_chunks",
			feeds: []feed{
				{chunk: `Some text. Assistant requested tool`, done: false},
				{chunk: ` read (call_x) with arguments:`, done: false},
				{chunk: ` {"a":1} More text.`, done: false},
				{chunk: "", done: true}, // flush
			},
			wantContent: "Some text.  More text.",
			wantCalls: []ToolCall{
				{ID: "call_x", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":1}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  3. Text before tool call — preserved as content
		// -----------------------------------------------------------------------
		{
			name: "3_text_before_tool_call",
			feeds: []feed{
				{chunk: `Prefix content. Assistant requested tool read (call_pre) with arguments: {"k":"v"}`, done: true},
			},
			wantContent: "Prefix content. ",
			wantCalls: []ToolCall{
				{ID: "call_pre", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"k":"v"}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  4. Text after tool call — preserved as content
		// -----------------------------------------------------------------------
		{
			name: "4_text_after_tool_call",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_post) with arguments: {} trailing text`, done: true},
			},
			wantContent: " trailing text",
			wantCalls: []ToolCall{
				{ID: "call_post", Type: "function", Function: CallFunc{Name: "read", Arguments: `{}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  5. Multiple tool calls in one buffer
		// -----------------------------------------------------------------------
		{
			name: "5_multiple_tool_calls",
			feeds: []feed{
				{chunk: "Assistant requested tool read (call_1) with arguments: {\"a\":1}\nAssistant requested tool write (call_2) with arguments: {\"b\":2}", done: true},
			},
			wantContent: "\n",
			wantCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":1}`}},
				{ID: "call_2", Type: "function", Function: CallFunc{Name: "write", Arguments: `{"b":2}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  6. Nested JSON arguments — brace counting matches outer }
		// -----------------------------------------------------------------------
		{
			name: "6_nested_json_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_nest) with arguments: {"config":{"nested":{"deep":42}}}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_nest", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"config":{"nested":{"deep":42}}}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  7. Empty arguments {}
		// -----------------------------------------------------------------------
		{
			name: "7_empty_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_empty) with arguments: {}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_empty", Type: "function", Function: CallFunc{Name: "read", Arguments: `{}`}},
			},
		},

		// -----------------------------------------------------------------------
		//  8. Array arguments — handled by bracket counting
		// -----------------------------------------------------------------------
		{
			name: "8_array_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_arr) with arguments: ["a","b"]`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_arr", Type: "function", Function: CallFunc{Name: "read", Arguments: `["a","b"]`}},
			},
		},

		// -----------------------------------------------------------------------
		//  9. String arguments — quoted string value
		// -----------------------------------------------------------------------
		{
			name: "9_string_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_str) with arguments: "hello"`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_str", Type: "function", Function: CallFunc{Name: "read", Arguments: `"hello"`}},
			},
		},

		// -----------------------------------------------------------------------
		// 10. Number arguments — numeric JSON value
		// -----------------------------------------------------------------------
		{
			name: "10_number_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_num) with arguments: 42.5`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_num", Type: "function", Function: CallFunc{Name: "read", Arguments: `42.5`}},
			},
		},

		// -----------------------------------------------------------------------
		// 11. Null arguments — JSON null literal
		// -----------------------------------------------------------------------
		{
			name: "11_null_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_null) with arguments: null`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_null", Type: "function", Function: CallFunc{Name: "read", Arguments: `null`}},
			},
		},

		// -----------------------------------------------------------------------
		// 12. Invalid arguments variant — NO tool call, text returned as content
		// -----------------------------------------------------------------------
		{
			name: "12_invalid_arguments_variant",
			feeds: []feed{
				{chunk: `Assistant requested tool bad (call_x) with invalid arguments: some error`, done: true},
			},
			wantContent: `Assistant requested tool bad (call_x) with invalid arguments: some error`,
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 13. Malformed JSON — NO tool call, text returned as content
		// -----------------------------------------------------------------------
		{
			name: "13_malformed_json",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_x) with arguments: {broken`, done: true},
			},
			wantContent: `Assistant requested tool read (call_x) with arguments: {broken`,
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 14. False positive protection — "Assistant requested tool" but no
		//     valid JSON after "arguments:"
		// -----------------------------------------------------------------------
		{
			name: "14_false_positive_protection",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_x) with arguments: not_json`, done: true},
			},
			wantContent: `Assistant requested tool read (call_x) with arguments: not_json`,
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 15. UTF-8 content — tool name with non‑ASCII, args with CJK
		// -----------------------------------------------------------------------
		{
			name: "15_utf8_content",
			feeds: []feed{
				{chunk: `Assistant requested tool café (café_01) with arguments: {"msg":"你好世界"}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "café_01", Type: "function", Function: CallFunc{Name: "café", Arguments: `{"msg":"你好世界"}`}},
			},
		},

		// -----------------------------------------------------------------------
		// 16. Buffer overflow — text exceeding maxSize without tool pattern
		//     (maxSize lowered to make test practical)
		// -----------------------------------------------------------------------
		{
			name: "16_buffer_overflow",
			setup: func(p *ToolCallParser) {
				p.maxSize = 16 // lower threshold for testability
			},
			feeds: []feed{
				{chunk: "xxxxxxxxxxxxxxxxxxxxx", done: false}, // 21 bytes > 16
			},
			wantContent: "xxxxxxxxxxxxxxxxxxxxx",
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 17. Partial prefix at chunk boundary
		// -----------------------------------------------------------------------
		{
			name: "17_partial_prefix_at_chunk_boundary",
			feeds: []feed{
				{chunk: `Assistant reques`, done: false},
				{chunk: `ted tool read (call_part) with arguments: {"x":1}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_part", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"x":1}`}},
			},
		},

		// -----------------------------------------------------------------------
		// 18. Case-insensitive matching
		// -----------------------------------------------------------------------
		{
			name: "18a_uppercase_matching",
			feeds: []feed{
				{chunk: `ASSISTANT REQUESTED TOOL read (call_upper) WITH ARGUMENTS: {"a":1}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_upper", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":1}`}},
			},
		},
		{
			name: "18b_lowercase_matching",
			feeds: []feed{
				{chunk: `assistant requested tool read (call_lower) with arguments: {"a":1}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_lower", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":1}`}},
			},
		},
		{
			name: "18c_mixed_case_matching",
			feeds: []feed{
				{chunk: `Assistant Requested Tool read (call_mix) With Arguments: {"a":1}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_mix", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":1}`}},
			},
		},

		// -----------------------------------------------------------------------
		// 19. Braces inside JSON string values — brace counter must NOT count
		//     braces inside strings
		// -----------------------------------------------------------------------
		{
			name: "19_braces_inside_json_strings",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_brace) with arguments: {"a":"{b}"}`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_brace", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"a":"{b}"}`}},
			},
		},

		// -----------------------------------------------------------------------
		// 20. done=false with incomplete JSON — no tool call, content empty
		// -----------------------------------------------------------------------
		{
			name: "20_done_false_incomplete_json",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_inc) with arguments: {"a":`, done: false},
			},
			wantContent: "",
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 21. Empty chunks
		// -----------------------------------------------------------------------
		{
			name: "21_empty_chunks",
			feeds: []feed{
				{chunk: "", done: false},
				{chunk: "", done: true},
			},
			wantContent: "",
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// 22. Consecutive chunks that slowly build a complete pattern
		//     (simulates real streaming)
		// -----------------------------------------------------------------------
		{
			name: "22_consecutive_chunks_streaming",
			feeds: []feed{
				{chunk: `Hello, `, done: false},
				{chunk: `Assistant`, done: false},
				{chunk: ` requested `, done: false},
				{chunk: `tool read `, done: false},
				{chunk: `(call_sim) with arguments:`, done: false},
				{chunk: ` {"key":"val"}`, done: false},
				{chunk: ` world`, done: false},
				{chunk: "", done: true}, // flush
			},
			wantContent: "Hello,  world",
			wantCalls: []ToolCall{
				{ID: "call_sim", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"key":"val"}`}},
			},
		},

		// -----------------------------------------------------------------------
		// Bonus: tool call with text both before and after, multiple chunks
		// -----------------------------------------------------------------------
		{
			name: "bonus_text_before_and_after_streaming",
			feeds: []feed{
				{chunk: `pre`, done: false},
				{chunk: `fix Assistant requested tool read (call_both)`, done: false},
				{chunk: ` with arguments: {"z":9}`, done: false},
				{chunk: ` postfix`, done: false},
				{chunk: "", done: true},
			},
			wantContent: "prefix  postfix",
			wantCalls: []ToolCall{
				{ID: "call_both", Type: "function", Function: CallFunc{Name: "read", Arguments: `{"z":9}`}},
			},
		},

		// -----------------------------------------------------------------------
		// Bonus: tool call with negative number and scientific notation
		// -----------------------------------------------------------------------
		{
			name: "bonus_negative_and_scientific_number",
			feeds: []feed{
				{chunk: `Assistant requested tool calc (call_sci) with arguments: -3.14e10`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_sci", Type: "function", Function: CallFunc{Name: "calc", Arguments: `-3.14e10`}},
			},
		},

		// -----------------------------------------------------------------------
		// Bonus: boolean literal arguments (true / false)
		// -----------------------------------------------------------------------
		{
			name: "bonus_boolean_true_arguments",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_bool) with arguments: true`, done: true},
			},
			wantContent: "",
			wantCalls: []ToolCall{
				{ID: "call_bool", Type: "function", Function: CallFunc{Name: "read", Arguments: `true`}},
			},
		},

		// -----------------------------------------------------------------------
		// Bonus: done=true flushes incomplete JSON as content
		// -----------------------------------------------------------------------
		{
			name: "bonus_done_true_flushes_incomplete_json",
			feeds: []feed{
				{chunk: `Assistant requested tool read (call_inc2) with arguments: {"x":`, done: true},
			},
			wantContent: `Assistant requested tool read (call_inc2) with arguments: {"x":`,
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// Bonus: multiple feeds then overflow on accumulated buffer
		// -----------------------------------------------------------------------
		{
			name: "bonus_accumulated_overflow",
			setup: func(p *ToolCallParser) {
				p.maxSize = 20
			},
			feeds: []feed{
				{chunk: "1234567890", done: false},  // 10 bytes
				{chunk: "abcdefghijk", done: false}, // 11 bytes, total 21 > 20
			},
			wantContent: "1234567890abcdefghijk",
			wantCalls:   nil,
		},

		// -----------------------------------------------------------------------
		// Bonus: CJK text accumulating past maxToolCallTail — verifies UTF‑8
		//        byte-level truncation does not split multi-byte characters
		// -----------------------------------------------------------------------
		{
			name: "bonus_cjk_tail_truncation",
			feeds: []feed{
				{chunk: "你好世界这是一个测试文本用来", done: false}, // 13 chars = 39 bytes
				{chunk: "触发缓冲区尾部截断逻辑", done: false},    // 10 chars = 30 bytes → total 69 > 64
				{chunk: "确保UTF8不会被截断", done: false},    // more CJK
				{chunk: "", done: true},
			},
			wantContent: "你好世界这是一个测试文本用来触发缓冲区尾部截断逻辑确保UTF8不会被截断",
			wantCalls:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewToolCallParser()
			if tt.setup != nil {
				tt.setup(p)
			}

			var allContent strings.Builder
			var allCalls []ToolCall

			for _, f := range tt.feeds {
				content, calls := p.Feed(f.chunk, f.done)
				allContent.WriteString(content)
				allCalls = append(allCalls, calls...)
			}

			if got := allContent.String(); got != tt.wantContent {
				t.Fatalf("content = %q, want %q", got, tt.wantContent)
			}

			if len(allCalls) != len(tt.wantCalls) {
				t.Fatalf("got %d calls, want %d", len(allCalls), len(tt.wantCalls))
			}
			for i := range allCalls {
				if allCalls[i].ID != tt.wantCalls[i].ID {
					t.Fatalf("call[%d].ID = %q, want %q", i, allCalls[i].ID, tt.wantCalls[i].ID)
				}
				if allCalls[i].Type != tt.wantCalls[i].Type {
					t.Fatalf("call[%d].Type = %q, want %q", i, allCalls[i].Type, tt.wantCalls[i].Type)
				}
				if allCalls[i].Function.Name != tt.wantCalls[i].Function.Name {
					t.Fatalf("call[%d].Function.Name = %q, want %q", i, allCalls[i].Function.Name, tt.wantCalls[i].Function.Name)
				}
				if allCalls[i].Function.Arguments != tt.wantCalls[i].Function.Arguments {
					t.Fatalf("call[%d].Function.Arguments = %q, want %q", i, allCalls[i].Function.Arguments, tt.wantCalls[i].Function.Arguments)
				}
			}
		})
	}
}

func TestToolCallParserKeepsLongFragmentedPrefix(t *testing.T) {
	p := NewToolCallParser()
	toolID := "call_00_a4J0yCJ48n7O5Yul0Q4u9242"
	prefix := "Assistant requested tool read (" + toolID + ") with argu"

	content, calls := p.Feed(prefix, false)
	if content != "" || len(calls) != 0 {
		t.Fatalf("prefix feed = (%q, %v), want no output while arguments are pending", content, calls)
	}

	content, calls = p.Feed(`ments: {"filePath":"internal/app/handler.go"}`, false)
	if content != "" {
		t.Fatalf("content = %q, want tool-call text to be stripped", content)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != toolID || calls[0].Function.Name != "read" {
		t.Fatalf("tool call = %+v, want id=%q name=read", calls[0], toolID)
	}
}

func TestToolCallParserAcceptsWhitespaceBeforeArguments(t *testing.T) {
	p := NewToolCallParser()
	content, calls := p.Feed("Assistant requested tool read (call_ws) with arguments:\n\t{\"file\":\"test.go\"}", true)

	if content != "" {
		t.Fatalf("content = %q, want tool-call text to be stripped", content)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].Function.Arguments != `{"file":"test.go"}` {
		t.Fatalf("arguments = %q, want JSON object", calls[0].Function.Arguments)
	}
}

func TestToolCallParserHandlesEveryTwoChunkBoundary(t *testing.T) {
	toolID := "call_00_a4J0yCJ48n7O5Yul0Q4u9242"
	input := "Assistant requested tool read (" + toolID + `) with arguments: {"file":"test.go"}`

	for split := 1; split < len(input); split++ {
		t.Run(fmt.Sprintf("split_%d", split), func(t *testing.T) {
			p := NewToolCallParser()
			var content strings.Builder
			var calls []ToolCall

			part, parsed := p.Feed(input[:split], false)
			content.WriteString(part)
			calls = append(calls, parsed...)
			part, parsed = p.Feed(input[split:], true)
			content.WriteString(part)
			calls = append(calls, parsed...)

			if content.String() != "" {
				t.Fatalf("raw tool-call text leaked at split %d: %q", split, content.String())
			}
			if len(calls) != 1 || calls[0].ID != toolID {
				t.Fatalf("calls at split %d = %+v, want one call with id %q", split, calls, toolID)
			}
		})
	}
}
