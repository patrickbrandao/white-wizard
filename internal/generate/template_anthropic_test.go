package generate

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestClaudeFamilyOf: the generation by the version in the id, dates
// ignored, anything else on claude45.
func TestClaudeFamilyOf(t *testing.T) {
	cases := map[string]*claudeFamily{
		"claude-haiku-4-5":           &claude45,
		"claude-haiku-4-5-20251001":  &claude45,
		"claude-sonnet-4":            &claude45,
		"claude-sonnet-4-20250514":   &claude45,
		"claude-3-5-sonnet-20241022": &claude45,
		"claude-x-abc":               &claude45,
		"claude-opus-x":              &claude45,
		"claude-":                    &claude45,
		"gpt-4o":                     &claude45,
		"":                           &claude45,
		"claude-opus-4-6":            &claude46,
		"Claude-Sonnet-4-6":          &claude46,
		"claude-opus-4-7":            &claude47,
		"claude-opus-4-8":            &claude5,
		"claude-fable-5":             &claude5,
		"claude-opus-5":              &claude5Opus,
		"claude-sonnet-5-20260101":   &claude5Sonnet,
		"claude-opus-5-5":            &claude55,
		"claude-haiku-5-5":           &claude55,
		"claude-fable-5-1":           &claude55,
		"claude-opus-6":              &claude55,
	}
	for id, want := range cases {
		if got := claudeFamilyOf(id); got != want {
			t.Errorf("%q: %+v", id, *got)
		}
	}
	if claude55.toolsAuto != 276 || claude55.toolsAny != 396 || claude55.toolsNamed != 398 || claude55.reply != 6 {
		t.Errorf("claude55: %+v", claude55)
	}
}

// TestAnthropicToolsTextUTF8: from Claude 4.6 on the tools text keeps
// non-ASCII as UTF-8 and escapes only control characters; invalid UTF-8
// becomes U+FFFD.
func TestAnthropicToolsTextUTF8(t *testing.T) {
	tools := []core.ToolDef{{Name: "a", Description: "Olá\n\x01 \xff 😀", Parameters: []byte(`{"enum":["é"]}`)}}
	got := AnthropicToolsTextFor("claude-opus-5-5", tools)
	want := "<functions>\n<function>{\"description\": \"Olá\\n\\u0001 � 😀\", \"name\": \"a\", \"parameters\": {\"enum\": [\"é\"]}}</function>\n</functions>"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := AnthropicToolsTextFor("claude-haiku-4-5", tools); !strings.Contains(got, "Ol\\u00e1") {
		t.Errorf("4.5 escapes: %s", got)
	}
}

// TestClaudeTextScale: from Claude 4.7 on the simulated count of the text
// grows by textPct (rounded per text) while the tokens Split gives stay
// Simulated's; a conversation that opens with assistant costs
// assistantFirst more (task 301).
func TestClaudeTextScale(t *testing.T) {
	s := scaled{pct: 132}
	if s.Count("hello world") != 3 || s.Count("x") != 1 || s.Count("") != 0 {
		t.Errorf("counts: %d %d %d", s.Count("hello world"), s.Count("x"), s.Count(""))
	}
	if got := s.Split("hello world"); len(got) != 2 || got[0]+got[1] != "hello world" {
		t.Errorf("split: %q", got)
	}
	if claude45.textPct != 0 || claude47.textPct != 132 || claude5.textPct != 132 || claude55.textPct != 132 {
		t.Error("textPct by generation")
	}
	for _, c := range []struct {
		model string
		f     *claudeFamily
	}{{"claude-haiku-4-5", &claude45}, {"claude-opus-4-7", &claude47}, {"claude-haiku-5-5", &claude55}} {
		ua := &core.Request{Model: c.model, Messages: []core.Message{{Role: "user", Content: "x"}, {Role: "assistant", Content: "x"}}}
		au := &core.Request{Model: c.model, Messages: []core.Message{{Role: "assistant", Content: "x"}, {Role: "user", Content: "x"}}}
		bu, ba := countAnthropicPrompt(Simulated{}, ua, 0), countAnthropicPrompt(Simulated{}, au, 0)
		if ba.Total-bu.Total != c.f.assistantFirst+c.f.msgHeader || ba.User-bu.User != c.f.assistantFirst {
			t.Errorf("%s: ua %+v au %+v", c.model, bu, ba)
		}
	}
}

// TestCountAnthropicPrompt: the breakdown of a conversation with the
// simulated tokenizer adds up and puts each part in its place; images keep
// their cost.
func TestCountAnthropicPrompt(t *testing.T) {
	req := &core.Request{
		Driver: "anthropic", Endpoint: core.EndpointMessages, Model: "claude-fable-5", System: "s",
		Messages: []core.Message{
			{Role: "user", Content: "x", ImageParts: 1},
			{Role: "assistant", Content: "t", ToolCalls: []core.ToolCall{{Name: "a", Arguments: `{"q":"r","n":[1]}`}}},
			{Role: "user", ToolResults: []core.ToolResult{{Content: "y", IsError: true}}},
			{Role: "user", Content: "z"},
		},
	}
	b := CountPrompt(Simulated{}, req, 85)
	f := claude5
	want := PromptBreakdown{
		System:      f.sysHeader + 1,
		User:        f.msgHeader + 1 + 1 + f.afterResultsText,
		Assistant:   1 + f.reply,
		ToolCalls:   f.callPair + 1 + 2*f.callParam + 1 + 1 + 1 + scaled{pct: f.textPct}.Count("[1]") + f.textBeforeCalls,
		ToolResults: 1 + f.resultError + f.textAfterResults,
		Images:      85,
	}
	want.Total = want.System + want.User + want.Assistant + want.ToolCalls + want.ToolResults + want.Images
	if b != want {
		t.Errorf("got  %+v\nwant %+v", b, want)
	}
}
