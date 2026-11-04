package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// The model effective in the route's catalog (ADR 0072, item 16, revision
// of task 434; spec 003 "Campos por catálogo"): context_window, max_output,
// capabilities and pricing of models[].catalogs.<c> hold on the bases of
// that catalog, for the listing and for the request.

const catalogModelCfg = `
server: {seed: 42, auth: none}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    long: {mode: random, output_tokens: {dist: uniform, min: 400, max: 400}}
models:
  - id: minimax-m3
    drivers: [opencode]
    protocols: [openai]
    latency: fast
    generation: long
    context_window: 60
    max_output: 20
    pricing: {input_per_m: 0.3, output_per_m: 1.2}
    catalogs:
      opencode: {}
      opencode-go:
        context_window: 1000
        max_output: 50
        pricing: {input_per_m: 0.6, output_per_m: 2.4}
  - id: kimi-k3
    drivers: [opencode]
    latency: fast
    catalogs:
      opencode: {}
      opencode-go: {capabilities: {tools: false}}
  - id: plain
    drivers: [opencode]
    latency: fast
    context_window: 60
`

// longPrompt is a chat body for model whose prompt is well above 60
// tokens and well below 1000.
func longPrompt(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"` + strings.Repeat("lorem ipsum dolor ", 40) + `"}]}`
}

const toolsBody = `{"model":"kimi-k3","max_tokens":8,"messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}]}`

func TestCatalogEffectiveModelRequest(t *testing.T) {
	e := newEngine(t, catalogModelCfg)
	for _, c := range []struct {
		name, path, body string
		status           int
		want             string
	}{
		// context_length_exceeded with the catalog's context_window.
		{"payg context", "/inference/openai/v1/chat/completions", longPrompt("minimax-m3"), 400, "maximum context length is 60 tokens"},
		{"zen payg context", "/zen/v1/chat/completions", longPrompt("minimax-m3"), 400, "maximum context length is 60 tokens"},
		{"go context", "/inference/go/openai/v1/chat/completions", longPrompt("minimax-m3"), 200, ""},
		{"zen go context", "/zen/go/v1/chat/completions", longPrompt("minimax-m3"), 200, ""},
		// A model without catalog fields behaves as declared on both.
		{"plain payg", "/inference/v1/chat/completions", longPrompt("plain"), 400, "maximum context length is 60 tokens"},
		{"plain go", "/inference/go/v1/chat/completions", longPrompt("plain"), 400, "maximum context length is 60 tokens"},
		// capabilities.tools: false only in opencode-go.
		{"go tools", "/zen/go/v1/chat/completions", toolsBody, 400, "does not support tools"},
		{"payg tools", "/zen/v1/chat/completions", toolsBody, 200, ""},
	} {
		w := call(t, e, "opencode", c.path, ocHeaders, c.body)
		if w.StatusCode() != c.status || !strings.Contains(string(w.Body()), c.want) {
			t.Errorf("%s: %d %s, want %d with %q", c.name, w.StatusCode(), w.Body(), c.status, c.want)
		}
	}

	// The output cap and the price are the catalog's: 400 tokens generated
	// stop at max_output (20 pay-as-you-go, 50 Go), priced per catalog.
	short := `{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}]}`
	for _, c := range []struct {
		path string
		out  int
	}{
		{"/zen/v1/chat/completions", 20},
		{"/zen/go/v1/chat/completions", 50},
	} {
		w := call(t, e, "opencode", c.path, ocHeaders, short)
		var resp struct {
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(w.Body(), &resp); err != nil || w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s (%v)", c.path, w.StatusCode(), w.Body(), err)
		}
		if resp.Usage.Completion != c.out {
			t.Errorf("%s: completion_tokens %d, want max_output %d", c.path, resp.Usage.Completion, c.out)
		}
	}
}

// listProbe records the models the engine hands the driver's listing.
type listProbe struct {
	driver.Driver
	got *[]driver.ModelInfo
}

func (p listProbe) Unwrap() driver.Driver { return p.Driver }

func (p listProbe) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	*p.got = models
	p.Driver.ListModels(w, req, models)
}

// TestCatalogEffectiveModelListing: the engine lists, on each base, the
// model effective in its catalog (ModelInfo.Cfg), and the item shows it.
func TestCatalogEffectiveModelListing(t *testing.T) {
	e := newEngine(t, catalogModelCfg)
	drv, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, catalog, limit string
		ctx, out             int
		tools                bool
	}{
		{"/inference/v1/models", "opencode", `"limit":{"context":60,"output":20}`, 60, 20, true},
		{"/zen/go/v1/models", "opencode-go", `"limit":{"context":1000,"output":50}`, 1000, 50, false},
	} {
		var got []driver.ModelInfo
		w := core.NewMemWriter()
		e.Handle(context.Background(), engine.Input{Driver: listProbe{drv, &got}, Kind: driver.KindModels, Method: "GET", Path: c.path, Headers: driver.MapHeaders{}, ClientIP: "127.0.0.1"}, w)
		if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), c.limit) {
			t.Errorf("%s: %d %s, want %s", c.path, w.StatusCode(), w.Body(), c.limit)
		}
		if len(got) != 3 {
			t.Fatalf("%s: %d models", c.path, len(got))
		}
		mm, kimi := got[0], got[1]
		if mm.Catalog != c.catalog || mm.Cfg.ContextWindow != c.ctx || mm.Cfg.MaxOutput != c.out {
			t.Errorf("%s: minimax-m3 %s %d/%d", c.path, mm.Catalog, mm.Cfg.ContextWindow, mm.Cfg.MaxOutput)
		}
		if tools := kimi.Cfg.Capabilities.Tools == nil || *kimi.Cfg.Capabilities.Tools; tools != c.tools {
			t.Errorf("%s: kimi-k3 tools %v", c.path, tools)
		}
	}
}
