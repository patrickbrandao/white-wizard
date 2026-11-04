package gemini_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
)

// TestEngineDeclaration (task 402): gtok's tokenizer and template and the
// implicit prompt cache; the content filter is the driver's promptFeedback.
func TestEngineDeclaration(t *testing.T) {
	d := (&gemini.Driver{}).EngineDeclaration()
	if d.Text != gtok.Text || d.Template != (gtok.Template{}) || d.ContentFilterAsFinish || d.ClaudeTokens || d.MalformedFrame != nil {
		t.Errorf("%+v", d)
	}
	if s := d.PromptCache(&config.ModelConfig{ID: "gemini-3.5-flash"}, core.EndpointChat); s != driver.CacheImplicit {
		t.Errorf("cache style %d", s)
	}
}
