package opencode_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// TestEngineDeclaration (task 402): the prompt cache of the endpoint's
// native API; the content filter is the driver's error.
func TestEngineDeclaration(t *testing.T) {
	d := (&opencode.Driver{}).EngineDeclaration()
	if d.ContentFilterAsFinish || d.ClaudeTokens || d.Text != nil || d.Template != nil || d.MalformedFrame != nil {
		t.Errorf("%+v", d)
	}
	m := &config.ModelConfig{ID: "kimi-k2"}
	for ep, want := range map[core.Endpoint]driver.CacheStyle{
		core.EndpointMessages:  driver.CacheBreakpoints,
		core.EndpointChat:      driver.CacheAutomatic,
		core.EndpointResponses: driver.CacheAutomatic,
	} {
		if s := d.PromptCache(m, ep); s != want {
			t.Errorf("%s: cache style %d", ep, s)
		}
	}
}
