package generate

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// TestCountPromptTemplateOverhead: every message costs 3 + tokens(role) +
// tokens(content), +1 with a name, and the request adds 3 for the reply
// priming (spec 007, ADR 0049). A tool result costs 5 + tokens(content)
// (Chat, call unknown: task 252).
func TestCountPromptTemplateOverhead(t *testing.T) {
	tk := Simulated{}
	probe := &core.Request{Messages: []core.Message{{Role: "user", Content: "Responda só: ok"}}}
	b := CountPrompt(tk, probe, 85)
	// 5 simulated tokens of content; o200k_base splits it into 6, hence the
	// real 13 (ADR 0053).
	if b.Total != 12 || b.User != 9 || b.Assistant != 3 {
		t.Fatalf("probe: %+v, want total 12 (user 9, priming 3)", b)
	}
	if b := CountPrompt(tokenize.O200k, probe, 85); b.Total != 13 || b.User != 10 {
		t.Fatalf("o200k probe: %+v, want total 13 (user 10, priming 3)", b)
	}
	req := &core.Request{System: "be brief", Messages: []core.Message{
		{Role: "developer", Content: "x"},
		{Role: "user", Content: "hi", Name: "ann"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", Content: "done"},
		{Role: "critic", Content: "hm"},
	}}
	b = CountPrompt(tk, req, 85)
	system := (3 + 1 + tk.Count("be brief")) + (3 + tk.Count("developer") + 1)
	user := (3 + 1 + 1 + 1) + (3 + tk.Count("critic") + 1)
	assistant := (3 + tk.Count("assistant") + 1) + 3
	if b.System != system || b.User != user || b.Assistant != assistant || b.ToolResults != 1+5 {
		t.Fatalf("breakdown %+v, want system %d user %d assistant %d tool_results 6", b, system, user, assistant)
	}
	if b.Total != system+user+assistant+6 {
		t.Fatalf("total %d", b.Total)
	}
	// A top-level system (Anthropic, Gemini) counts as one system message,
	// the same as the Chat Completions system message (task 245).
	top := CountPrompt(tk, &core.Request{System: "be brief", Messages: []core.Message{{Role: "user", Content: "hi"}}}, 85)
	inline := CountPrompt(tk, &core.Request{Messages: []core.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}}, 85)
	if top.System != 3+1+2 || top != inline {
		t.Fatalf("top-level %+v, inline %+v", top, inline)
	}
	if empty := CountPrompt(tk, &core.Request{}, 85); empty.Total != ReplyPriming {
		t.Fatalf("empty request: %+v", empty)
	}
}

// TestTextExactTarget: Text cuts at exactly target tokens, even when one
// sentence of the corpus is one token short of it.
func TestTextExactTarget(t *testing.T) {
	g := NewGenerator(Simulated{}, nil)
	for seed := int64(1); seed <= 50; seed++ {
		for _, n := range []int{1, 15, 16, 17, 64} {
			if got := (Simulated{}).Count(g.Text(NewRNG(seed, "t"), n)); got != n {
				t.Fatalf("seed %d target %d: %d tokens", seed, n, got)
			}
		}
	}
}
