package anthropic_test

import (
	"bytes"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
)

// TestEngineDeclaration (task 402): the Claude 4.7+ tokens, the
// cache_control breakpoints, the content filter as a 200 that ends in
// refusal and a malformed frame in the Messages stream's shape.
func TestEngineDeclaration(t *testing.T) {
	d := (&anthropic.Driver{}).EngineDeclaration()
	if !d.ClaudeTokens || !d.ContentFilterAsFinish || d.Text != nil || d.Template != nil {
		t.Errorf("%+v", d)
	}
	if !bytes.HasPrefix(d.MalformedFrame, []byte("event: content_block_delta\ndata: {")) {
		t.Errorf("frame %q", d.MalformedFrame)
	}
	if s := d.PromptCache(&config.ModelConfig{ID: "claude-opus-5-5"}, core.EndpointMessages); s != driver.CacheBreakpoints {
		t.Errorf("cache style %d", s)
	}
}
