package engine

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
)

// TestGeminiTokenizer (task 265): on the Gemini driver a model under auto
// counts with gtok and the Gemini template, as the driver declares (task
// 402); an explicit tokenizer and the other drivers keep theirs.
func TestGeminiTokenizer(t *testing.T) {
	gem, oai := engineDeclOf(t, "gemini"), engineDeclOf(t, "openai")
	for _, c := range []struct {
		m    config.ModelConfig
		decl driver.EngineDecl
		want any
	}{
		{config.ModelConfig{ID: "gemini-3.5-flash-lite"}, gem, gtok.Text},
		{config.ModelConfig{ID: "gemini-3.5-flash-lite", Tokenizer: config.TokenizerSimulated}, gem, simulatedText},
		{config.ModelConfig{ID: "gemini-3.5-flash-lite", Tokenizer: config.TokenizerO200k}, gem, o200kText},
		{config.ModelConfig{ID: "gemini-3.5-flash-lite"}, oai, simulatedText},
		{config.ModelConfig{ID: "gemini-3.5-flash-lite"}, driver.EngineDecl{}, simulatedText},
	} {
		if got := generatorOf(&c.m, c.decl); got != c.want {
			t.Errorf("%q on %+v: another generator", c.m.Tokenizer, c.decl)
		}
	}
	req := &core.Request{Messages: []core.Message{{Role: "user", Content: "a"}}}
	if n := countPrompt(gtok.Tokenizer{}, req, gem.Template).Total; n != 2 {
		t.Errorf("gemini template: %d", n)
	}
	if n := countPrompt(simulated, req, oai.Template).Total; n != 8 { // 3 + role + "a" + 3 priming
		t.Errorf("chat template: %d", n)
	}
}
