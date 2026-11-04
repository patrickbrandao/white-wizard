package engine

import (
	"bytes"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// engineDeclOf is what the registered driver name declares to the engine;
// the zero EngineDecl for a name that is not registered.
func engineDeclOf(t *testing.T, name string) driver.EngineDecl {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		return driver.EngineDecl{}
	}
	return driver.EngineDeclOf(d)
}

// TestMalformedFrameByDeclaration (task 402): the malformed fault writes
// the frame the driver declares, a chat completion chunk by default.
func TestMalformedFrameByDeclaration(t *testing.T) {
	if got := malformedFrame(engineDeclOf(t, "anthropic")); !bytes.HasPrefix(got, []byte("event: content_block_delta\n")) {
		t.Errorf("anthropic: %q", got)
	}
	for _, name := range []string{"openai", "azure", "gemini", "openrouter", "opencode", "unknown"} {
		if got := malformedFrame(engineDeclOf(t, name)); !bytes.Equal(got, chatMalformedFrame) {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// TestContentFilterAsFinishByDeclaration (task 393, 402): only OpenAI and
// Anthropic answer the content filter with a 200 that ends filtered.
func TestContentFilterAsFinishByDeclaration(t *testing.T) {
	for name, want := range map[string]bool{"openai": true, "anthropic": true, "azure": false, "gemini": false, "openrouter": false, "opencode": false} {
		if got := engineDeclOf(t, name).ContentFilterAsFinish; got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
