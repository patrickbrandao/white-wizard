package azure_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/azure"
)

// TestEngineDeclaration (task 402): OpenAI's automatic prompt cache; the
// content filter is the driver's 400.
func TestEngineDeclaration(t *testing.T) {
	d := (&azure.Driver{}).EngineDeclaration()
	if d.ContentFilterAsFinish || d.ClaudeTokens || d.Text != nil || d.Template != nil || d.MalformedFrame != nil {
		t.Errorf("%+v", d)
	}
	if s := d.PromptCache(&config.ModelConfig{ID: "gpt-5"}, core.EndpointChat); s != driver.CacheAutomatic {
		t.Errorf("cache style %d", s)
	}
}
