package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Current OpenAI clients send max_completion_tokens; max_tokens is deprecated.
// Reading only the old field silently discarded the caller's budget.
func TestOutputTokenBudget(t *testing.T) {
	cases := []struct {
		name                string
		maxTokens, maxCompl int
		want                int
	}{
		{"neither set", 0, 0, 0},
		{"legacy max_tokens only", 4096, 0, 4096},
		{"modern max_completion_tokens only", 0, 384000, 384000},
		{"both set, modern wins", 4096, 384000, 384000},
	}
	for _, c := range cases {
		req := &ChatRequest{MaxTokens: c.maxTokens, MaxCompletionTokens: c.maxCompl}
		if got := req.OutputTokenBudget(); got != c.want {
			t.Errorf("%s: OutputTokenBudget() = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestOpenAIToCCUsesCompletionTokenBudget(t *testing.T) {
	msgs := []Message{{Role: "user", Content: TextContent("hi")}}

	// The exact shape every request in the captured production logs used.
	cc, err := openAIToCC(&ChatRequest{Model: "test/m", Messages: msgs, MaxCompletionTokens: 384000})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if cc.Params.MaxTokens != maximumCCMaxTokens {
		t.Errorf("max_completion_tokens=384000 gave upstream max_tokens=%d, want the %d clamp",
			cc.Params.MaxTokens, maximumCCMaxTokens)
	}

	cc, err = openAIToCC(&ChatRequest{Model: "test/m", Messages: msgs, MaxCompletionTokens: 8192})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if cc.Params.MaxTokens != 8192 {
		t.Errorf("upstream max_tokens = %d, want 8192", cc.Params.MaxTokens)
	}

	cc, err = openAIToCC(&ChatRequest{Model: "test/m", Messages: msgs})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if cc.Params.MaxTokens != defaultCCMaxTokens {
		t.Errorf("unset budget gave %d, want the %d default", cc.Params.MaxTokens, defaultCCMaxTokens)
	}
}

// A tool-call event carries the whole tool input inline. bufio.Scanner's token
// cap used to abort the scan on such a line, discarding every later event —
// including finish — so the client saw a stream that simply stopped.
func TestParseStreamEventsHandlesOversizedLine(t *testing.T) {
	huge := strings.Repeat("y", 3*1024*1024)
	body := strings.Join([]string{
		`data: {"type":"text-delta","text":"before"}`,
		fmt.Sprintf(`data: {"type":"tool-call","toolCallId":"c1","toolName":"write","input":{"path":"/a","content":%q}}`, huge),
		`data: {"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
		"data: [DONE]",
		"",
	}, "\n\n")

	var kinds []string
	var gotContent int
	err := ParseStreamEvents(
		&http.Response{Body: io.NopCloser(strings.NewReader(body))},
		func(ev CCStreamEvent) error {
			kinds = append(kinds, ev.Type)
			if ev.Type == "tool-call" {
				if input, ok := ev.Input.(map[string]any); ok {
					gotContent = len(input["content"].(string))
				}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("ParseStreamEvents: %v", err)
	}
	want := []string{"text-delta", "tool-call", "finish"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v (an oversized line must not truncate the stream)", kinds, want)
	}
	if gotContent != len(huge) {
		t.Fatalf("tool input content = %d bytes, want %d", gotContent, len(huge))
	}
}

func TestParseStreamEventsSurvivesMissingTrailingNewline(t *testing.T) {
	body := `data: {"type":"text-delta","text":"a"}` + "\n\n" +
		`data: {"type":"finish","finishReason":"stop"}` // no trailing newline

	var kinds []string
	err := ParseStreamEvents(
		&http.Response{Body: io.NopCloser(strings.NewReader(body))},
		func(ev CCStreamEvent) error { kinds = append(kinds, ev.Type); return nil })
	if err != nil {
		t.Fatalf("ParseStreamEvents: %v", err)
	}
	if strings.Join(kinds, ",") != "text-delta,finish" {
		t.Fatalf("events = %v, want [text-delta finish]", kinds)
	}
}
