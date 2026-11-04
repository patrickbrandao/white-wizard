package engine

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestClaudeTokens (task 342): Claude 4.7 and later count ~32% more tokens
// than Simulated in the same text, as the prompt template scales it; Split
// is Count's tokens and joins back to the text, multi-byte characters
// kept whole.
func TestClaudeTokens(t *testing.T) {
	rng := generate.NewRNG(42, "claude")
	text := simulatedText.Text(rng, 5000) + " ação café 日本語 ñandú"
	var ct claudeTokens
	sim, got := simulated.Count(text), ct.Count(text)
	if ratio := float64(got) / float64(sim); ratio < 1.30 || ratio > 1.33 {
		t.Errorf("claude %d / simulated %d = %.3f, want ~1.32", got, sim, ratio)
	}
	toks := ct.Split(text)
	if len(toks) != got || strings.Join(toks, "") != text {
		t.Errorf("split: %d tokens (count %d), joined equal %v", len(toks), got, strings.Join(toks, "") == text)
	}
	for _, tok := range toks {
		if !utf8.ValidString(tok) {
			t.Fatalf("token %q splits a character", tok)
		}
	}
	// The max_tokens cut lands on these tokens.
	if n := ct.Count(generate.TrimToTokens(ct, text, 100)); n != 100 {
		t.Errorf("trim to 100: %d", n)
	}
	for tok, want := range map[string]int{"": 0, "a": 0, "ab": 1, "aé": 1, "é": 0, "日本": 3} {
		if got := midRune(tok); got != want {
			t.Errorf("midRune(%q) = %d, want %d", tok, got, want)
		}
	}
}

// TestClaudeGenerator: on Anthropic's dialect a Claude 4.7 or later (by
// id, version or alias) writes and counts its output with claudeTokens;
// earlier Claudes, other dialects and BPE tokenizers keep theirs. The
// prompt is counted the same with either tokenizer.
func TestClaudeGenerator(t *testing.T) {
	anth, oai := engineDeclOf(t, "anthropic"), engineDeclOf(t, "openai")
	for _, c := range []struct {
		m    config.ModelConfig
		decl driver.EngineDecl
		want *generate.Generator
	}{
		{config.ModelConfig{ID: "claude-opus-5-5"}, anth, claudeText},
		{config.ModelConfig{ID: "claude-opus-4-7"}, anth, claudeText},
		{config.ModelConfig{ID: "claude-haiku-5-20260101"}, anth, claudeText},
		{config.ModelConfig{ID: "wwz-claude", Version: "claude-sonnet-4-8-20260301"}, anth, claudeText},
		{config.ModelConfig{ID: "wwz-claude", Aliases: []string{"x", "claude-fable-5-1"}}, anth, claudeText},
		{config.ModelConfig{ID: "claude-opus-5-5", Tokenizer: config.TokenizerSimulated}, anth, claudeText},
		{config.ModelConfig{ID: "claude-opus-4-6"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-haiku-4-5-20251001"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-3-7-sonnet"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-opus"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-opus-x"}, anth, simulatedText},
		{config.ModelConfig{ID: "claude-opus-5-5"}, oai, simulatedText},
		{config.ModelConfig{ID: "claude-opus-5-5", Tokenizer: config.TokenizerO200k}, anth, o200kText},
	} {
		got := generatorOf(&c.m, c.decl)
		if got != c.want {
			t.Errorf("%+v on %+v: another generator", c.m, c.decl)
		}
	}
	if generatorOf(&config.ModelConfig{ID: "claude-opus-5-5"}, driver.EngineDecl{}) != simulatedText {
		t.Error("no driver: simulated")
	}
	req := &core.Request{
		Driver: "anthropic", Endpoint: core.EndpointMessages, Model: "claude-opus-5-5", System: "be brief",
		Messages: []core.Message{{Role: "user", Content: "hello there, how are you today?"}},
	}
	if a, b := countPrompt(claudeTokens{}, req, anth.Template), countPrompt(simulated, req, anth.Template); a != b {
		t.Errorf("prompt: %+v with claudeTokens, %+v with simulated", a, b)
	}
}
