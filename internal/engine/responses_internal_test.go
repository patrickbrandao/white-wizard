package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func TestCutBelowAndTail(t *testing.T) {
	auto := pcRule{kind: pcAutomatic, min: 1024, gran: 128}
	for n, want := range map[int]int{1000: 0, 1024: 1024, 1151: 1024, 1697: 1664} {
		if got := cutBelow(n, auto); got != want {
			t.Errorf("cutBelow(%d) = %d, want %d", n, got, want)
		}
	}
	if got := cutBelow(1500, pcRule{kind: pcAutomatic, min: 1024}); got != 1500 {
		t.Errorf("exact boundary: %d", got)
	}
	chat := &request{req: &core.Request{Endpoint: core.EndpointChat}}
	resp := &request{req: &core.Request{Endpoint: core.EndpointResponses}}
	if respCacheTail(chat, auto) != 0 || respCacheTail(resp, auto) != 128 {
		t.Error("tail")
	}
}

func TestOutputTokensWithCalls(t *testing.T) {
	m := &config.ModelConfig{ID: "gpt-4o-mini"}
	r := &request{tk: tokenizerFor(m)}
	plain := outputTokens(r, &core.Response{Content: "Hello there!"})
	if plain != 7 {
		t.Fatalf("Hello there! as a message: %d, want 7", plain)
	}
	with := outputTokens(r, &core.Response{Content: "Hello there!", ToolCalls: []core.ToolCall{{Name: "f", Arguments: "{}"}}})
	if with <= plain+8 {
		t.Errorf("tool call adds name, arguments and 8: %d", with)
	}
}

// TestResponsesCacheFailures: a failing prompt cache is a miss on the
// chained call and only logs on the record's write.
func TestResponsesCacheFailures(t *testing.T) {
	drv, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &config.ModelConfig{ID: "gpt-4o-mini"}
	e := &Engine{Store: failingCacher{limits.NewMemory()}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := &core.Request{Endpoint: core.EndpointResponses, Messages: []core.Message{{Role: "user", Content: strings.Repeat("x ", 10)}}}
	r := &request{in: Input{Driver: drv}, decl: driver.EngineDeclOf(drv), req: req, model: m, tk: tokenizerFor(m)}
	r.rs = &respState{store: true, reg: limits.NewMemory(), prev: &limits.ResponseRecord{Tokens: 3000, Prefix: "p"}}
	r.prompt.Total = 3100
	e.chainedCacheLookup(context.Background(), r)
	if r.pc == nil || r.pc.read != 0 || len(r.pc.commit) != 1 {
		t.Fatalf("failing lookup: %+v", r.pc)
	}
	r.resp = &core.Response{ID: "resp_x", Content: "ok"}
	e.responsesPut(context.Background(), r) // commit fails: logged, record written
	if got, _ := r.rs.reg.ResponseGet(context.Background(), "resp_x", "", r.req.Received); got == nil || got.Tokens < 3000 {
		t.Fatalf("record %+v", got)
	}
	// Nothing to read: no prefix, or a model without the automatic cache.
	r.pc, r.rs.prev.Prefix = nil, ""
	e.chainedCacheLookup(context.Background(), r)
	r.rs.prev.Prefix = "p"
	r.model = &config.ModelConfig{ID: "m", PromptCache: &config.PromptCacheRule{Kind: pcNone}}
	e.chainedCacheLookup(context.Background(), r)
	if r.pc != nil {
		t.Fatalf("no cache: %+v", r.pc)
	}
}
