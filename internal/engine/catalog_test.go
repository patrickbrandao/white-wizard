package engine_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// ADR 0048, spec 003: protocols is the wire dialect, drivers the catalog.
const catalogCfg = `
server: {seed: 42, auth: none}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: dia-openai, protocols: [openai], latency: fast}
  - {id: dia-anthropic, protocols: [anthropic], latency: fast}
  - {id: only-router, protocols: [anthropic], drivers: [openrouter], latency: fast}
  - {id: legacy-code, protocols: [opencode], latency: fast}
`

var ocHeaders = map[string]string{"x-opencode-session": "s1", "anthropic-version": "2023-06-01"}

func catalogBody(model string) string {
	return `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
}

func TestDialectAndCatalog(t *testing.T) {
	e := newEngine(t, catalogCfg)
	for _, c := range []struct {
		drv, path, model string
		want             int
	}{
		// protocols: [openai] without drivers: chat of openai, openrouter, opencode.
		{"openai", "/v1/chat/completions", "dia-openai", 200},
		{"openrouter", "/api/v1/chat/completions", "dia-openai", 200},
		{"opencode", "/zen/go/v1/chat/completions", "dia-openai", 200},
		// ... and the nonexistent-model error on anthropic's Messages.
		{"anthropic", "/v1/messages", "dia-openai", 404},
		// OpenCode serves its catalog on every route, whatever protocols
		// (ADR 0062).
		{"opencode", "/zen/go/v1/messages", "dia-openai", 200},
		{"opencode", "/inference/go/anthropic/v1/messages", "dia-openai", 200},
		{"opencode", "/zen/go/v1/messages", "dia-anthropic", 200},
		{"opencode", "/zen/go/v1/chat/completions", "dia-anthropic", 200},
		{"opencode", "/zen/go/v1/messages", "only-router", 0},
		{"openai", "/v1/chat/completions", "dia-anthropic", 404},
		// OpenRouter serves its catalog on every route, whatever protocols.
		{"openrouter", "/api/v1/messages", "dia-openai", 200},
		{"openrouter", "/api/v1/chat/completions", "only-router", 200},
		{"anthropic", "/v1/messages", "only-router", 404},
		// Legacy protocols: [opencode] → drivers [opencode], protocols [openai].
		{"opencode", "/zen/go/v1/chat/completions", "legacy-code", 200},
		{"openai", "/v1/chat/completions", "legacy-code", 404},
		{"openrouter", "/api/v1/chat/completions", "legacy-code", 0},
	} {
		w := call(t, e, c.drv, c.path, ocHeaders, catalogBody(c.model))
		switch {
		case c.want == 200 && w.StatusCode() != 200:
			t.Errorf("%s %s %s: %d %s", c.drv, c.path, c.model, w.StatusCode(), w.Body())
		case c.want == 404 && w.StatusCode() != 404:
			t.Errorf("%s %s %s: %d %s, want the nonexistent-model error", c.drv, c.path, c.model, w.StatusCode(), w.Body())
		case c.want == 0 && w.StatusCode() < 400:
			t.Errorf("%s %s %s: %d, want the driver's nonexistent-model error", c.drv, c.path, c.model, w.StatusCode())
		}
	}
	for drv, want := range map[string][]string{
		"openai":     {"dia-openai"},
		"anthropic":  {"dia-anthropic"},
		"openrouter": {"dia-openai", "dia-anthropic", "only-router"},
		"opencode":   {"dia-openai", "dia-anthropic", "legacy-code"},
	} {
		var got []string
		for _, m := range e.Models(drv) {
			got = append(got, m.Cfg.ID)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s /v1/models = %v, want %v", drv, got, want)
		}
	}
	// The listing routes serve the same catalog.
	for drv, path := range map[string]string{"openai": "/v1/models", "openrouter": "/api/v1/models", "opencode": "/zen/go/v1/models"} {
		w := call(t, e, drv, path, ocHeaders, "")
		if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), "dia-openai") {
			t.Errorf("%s %s: %d %s", drv, path, w.StatusCode(), w.Body())
		}
	}
}

func TestDialectOfKind(t *testing.T) {
	for k, want := range map[driver.Kind]string{
		driver.KindChat: "openai", driver.KindCompletions: "openai", driver.KindResponses: "openai",
		driver.KindEmbeddings: "openai", driver.KindMessages: "anthropic", driver.KindCountTokens: "anthropic",
	} {
		if got := engine.Dialect(k); got != want {
			t.Errorf("Dialect(%s) = %s, want %s", k, got, want)
		}
	}
}
