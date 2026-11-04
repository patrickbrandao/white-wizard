package openrouter_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestEngineDeclaration (task 402): the prompt cache of the upstream the
// slug's author names; the content filter is the driver's 403.
func TestEngineDeclaration(t *testing.T) {
	d := (&openrouter.Driver{}).EngineDeclaration()
	if d.ContentFilterAsFinish || d.ClaudeTokens || d.Text != nil || d.Template != nil || d.MalformedFrame != nil {
		t.Errorf("%+v", d)
	}
	for id, want := range map[string]driver.CacheStyle{
		"openai/gpt-5":       driver.CacheAutomatic,
		"anthropic/claude-4": driver.CacheBreakpoints,
		"google/gemini-3":    driver.CacheImplicit,
		"meta/llama-4":       driver.CacheNone,
		"no-author":          driver.CacheNone,
	} {
		if s := d.PromptCache(&config.ModelConfig{ID: id}, core.EndpointChat); s != want {
			t.Errorf("%s: cache style %d", id, s)
		}
	}
}
