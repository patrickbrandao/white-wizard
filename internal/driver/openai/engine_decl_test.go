package openai_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

// TestEngineDeclaration (task 402): the automatic prompt cache and the
// content filter as a filtered 200; the chat template and frame.
func TestEngineDeclaration(t *testing.T) {
	d := (&openai.Driver{}).EngineDeclaration()
	if !d.ContentFilterAsFinish || d.ClaudeTokens || d.Text != nil || d.Template != nil || d.MalformedFrame != nil {
		t.Errorf("%+v", d)
	}
	if s := d.PromptCache(&config.ModelConfig{ID: "gpt-5"}, core.EndpointResponses); s != driver.CacheAutomatic {
		t.Errorf("cache style %d", s)
	}
}
