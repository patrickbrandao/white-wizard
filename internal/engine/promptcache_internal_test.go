package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

func intp(n int) *int { return &n }

func TestPCRuleFor(t *testing.T) {
	m := func(id string, pc *config.PromptCacheRule) *config.ModelConfig {
		return &config.ModelConfig{ID: id, PromptCache: pc}
	}
	cases := []struct {
		drv   string
		model *config.ModelConfig
		ep    core.Endpoint
		want  pcRule
	}{
		{"openai", m("x", nil), core.EndpointChat, pcRule{pcAutomatic, 1024, 128, driver.CacheAutomatic}},
		{"azure", m("x", nil), core.EndpointChat, pcRule{pcAutomatic, 1024, 128, driver.CacheAutomatic}},
		{"anthropic", m("x", nil), core.EndpointMessages, pcRule{pcExplicit, 512, 0, driver.CacheBreakpoints}},
		{"gemini", m("x", nil), core.EndpointChat, pcRule{pcAutomatic, 4096, 0, driver.CacheImplicit}},
		{"openrouter", m("openai/gpt", nil), core.EndpointChat, pcRule{pcAutomatic, 1024, 128, driver.CacheAutomatic}},
		{"openrouter", m("anthropic/claude", nil), core.EndpointChat, pcRule{pcExplicit, 512, 0, driver.CacheBreakpoints}},
		{"openrouter", m("google/gemini", nil), core.EndpointChat, pcRule{pcAutomatic, 4096, 0, driver.CacheImplicit}},
		{"openrouter", m("meta/llama", nil), core.EndpointChat, pcRule{kind: pcNone}},
		{"openrouter", m("meta/llama", &config.PromptCacheRule{Kind: pcExplicit}), core.EndpointChat, pcRule{kind: pcExplicit, style: driver.CacheBreakpoints}},
		{"openrouter", m("meta/llama", &config.PromptCacheRule{Kind: pcAutomatic, MinTokens: intp(10), Granularity: intp(0)}), core.EndpointChat, pcRule{pcAutomatic, 10, 0, driver.CacheAutomatic}},
		{"opencode", m("x", nil), core.EndpointChat, pcRule{pcAutomatic, 1024, 128, driver.CacheAutomatic}},
		{"opencode", m("x", nil), core.EndpointMessages, pcRule{pcExplicit, 512, 0, driver.CacheBreakpoints}},
		{"openai", m("x", &config.PromptCacheRule{Kind: pcNone}), core.EndpointChat, pcRule{pcNone, 1024, 128, driver.CacheAutomatic}},
		{"other", m("x", nil), core.EndpointChat, pcRule{kind: pcNone}},
	}
	for _, c := range cases {
		if got := pcRuleFor(engineDeclOf(t, c.drv), c.model, c.ep); got != c.want {
			t.Errorf("%s %s %s: %+v, want %+v", c.drv, c.model.ID, c.ep, got, c.want)
		}
	}
}

func TestPCAutoTTL(t *testing.T) {
	opts := func(ret, ttl string) *pcRawOpts {
		o := &pcRawOpts{Retention: ret}
		if ttl != "" {
			o.Options = &struct {
				TTL string `json:"ttl"`
			}{ttl}
		}
		return o
	}
	cases := []struct {
		style     driver.CacheStyle
		o         *pcRawOpts
		ttl, life time.Duration
	}{
		{driver.CacheAutomatic, opts("", ""), 10 * time.Minute, time.Hour},
		{driver.CacheAutomatic, opts("in_memory", ""), 10 * time.Minute, time.Hour},
		{driver.CacheAutomatic, opts("24h", ""), 24 * time.Hour, 24 * time.Hour},
		{driver.CacheAutomatic, opts("", "30m"), 30 * time.Minute, 30 * time.Minute},
		{driver.CacheAutomatic, opts("24h", "12h"), 12 * time.Hour, 12 * time.Hour},
		{driver.CacheAutomatic, opts("", "bogus"), 10 * time.Minute, time.Hour},
		{driver.CacheImplicit, opts("24h", ""), 5 * time.Minute, 0},
	}
	for _, c := range cases {
		if ttl, life := pcAutoTTL(c.style, c.o); ttl != c.ttl || life != c.life {
			t.Errorf("%d %+v: %v/%v, want %v/%v", c.style, c.o, ttl, life, c.ttl, c.life)
		}
	}
}

// pcEnv builds a request ready for promptCacheLookup.
type pcEnv struct {
	e   *Engine
	drv driver.Driver
	m   *config.ModelConfig
	key *config.KeyConfig
}

func newPCEnv(t *testing.T, drvName string, store limits.Store, pc *config.PromptCacheRule) *pcEnv {
	t.Helper()
	drv, err := driver.New(drvName, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &pcEnv{
		e:   &Engine{Store: store, Log: slog.New(slog.DiscardHandler)},
		drv: drv,
		m:   &config.ModelConfig{ID: "m", PromptCache: pc},
		key: &config.KeyConfig{Label: "k", Account: "acct"},
	}
}

func (p *pcEnv) run(t *testing.T, kind driver.Kind, body string, commit bool) *pcState {
	t.Helper()
	req, perr := p.drv.ParseRequest(kind, []byte(body), driver.MapHeaders{})
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	req.ClientIP = "198.51.100.1"
	r := &request{in: Input{Driver: p.drv}, decl: driver.EngineDeclOf(p.drv), req: req, model: p.m, key: p.key, tk: tokenizerFor(p.m)}
	r.prompt = generate.CountPrompt(r.tk, req, 85)
	p.e.promptCacheLookup(context.Background(), r)
	if commit {
		p.e.promptCacheCommit(context.Background(), r)
		p.e.promptCacheCommit(context.Background(), r) // once only
	}
	if r.pc == nil {
		return &pcState{}
	}
	if r.pc.read+r.pc.write > r.prompt.Total {
		t.Fatalf("read %d + write %d > prompt %d", r.pc.read, r.pc.write, r.prompt.Total)
	}
	return r.pc
}

func words(n int, w string) string { return strings.TrimSpace(strings.Repeat(w+" ", n)) }

// TestPCExplicitBlocks: tools (both shapes), system blocks, every content
// block type, breakpoints on blocks, messages and the top level.
func TestPCExplicitBlocks(t *testing.T) {
	p := newPCEnv(t, "anthropic", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(16)})
	long := words(40, "lorem")
	body := `{"model":"m","max_tokens":5,
 "tools":[{"name":"t1","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
 "system":[{"type":"text","text":"` + long + `"},{"type":"text","text":"more","cache_control":{"type":"ephemeral","ttl":"1h"}}],
 "messages":[{"role":"user","content":[{"type":"text","text":"` + long + `"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
  {"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"tool_use","id":"toolu_1","name":"t1","input":{"a":1}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"r1"},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r2"}]},{"type":"text","text":"end","cache_control":{"type":"ephemeral"}}]}]}`
	first := p.run(t, driver.KindMessages, body, true)
	if first.read != 0 || first.write == 0 || first.write1h == 0 || first.write1h >= first.write {
		t.Fatalf("first: %+v", first)
	}
	second := p.run(t, driver.KindMessages, body, true)
	if second.read != first.write || second.write != 0 || second.write1h != 0 {
		t.Fatalf("second: %+v (first wrote %d)", second, first.write)
	}
	// The top-level cache_control caches the last block.
	top := `{"model":"m","max_tokens":5,"cache_control":{"type":"ephemeral"},"system":"` + long + `","messages":[{"role":"user","content":"` + long + `"}]}`
	a := p.run(t, driver.KindMessages, top, true)
	b := p.run(t, driver.KindMessages, top, false)
	if a.write == 0 || b.read != a.write {
		t.Fatalf("top level: %+v then %+v", a, b)
	}
}

// TestPCOpenRouterAnthropic: an anthropic/ slug on OpenRouter follows the
// Anthropic rule over the OpenAI body: function tools and cache_control on
// content parts.
func TestPCOpenRouterAnthropic(t *testing.T) {
	p := newPCEnv(t, "openrouter", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(16)})
	p.m.ID = "anthropic/claude"
	long := words(40, "lorem")
	body := `{"model":"anthropic/claude","max_tokens":5,
 "tools":[{"type":"function","function":{"name":"t1","description":"d","parameters":{"type":"object"}}}],
 "messages":[{"role":"system","content":[{"type":"text","text":"` + long + `","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"hi"}]}`
	first := p.run(t, driver.KindChat, body, true)
	second := p.run(t, driver.KindChat, body, true)
	if first.write == 0 || first.read != 0 || second.read != first.write || second.write != 0 {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

// TestPCBreakpointLimits: only the first 4 breakpoints count, and a read
// looks back at most 20 blocks before a breakpoint.
func TestPCBreakpointLimits(t *testing.T) {
	p := newPCEnv(t, "anthropic", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(1)})
	blocks := func(n int, bpAt ...int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			cc := ""
			for _, at := range bpAt {
				if at == i {
					cc = `,"cache_control":{"type":"ephemeral"}`
				}
			}
			b.WriteString(`{"type":"text","text":"block ` + strings.Repeat("x", i%7+1) + `"` + cc + `}`)
		}
		return `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[` + b.String() + `]}]}`
	}
	// Five breakpoints: the fifth is ignored, so the write ends at the fourth.
	five := p.run(t, driver.KindMessages, blocks(10, 1, 2, 3, 4, 8), false)
	four := p.run(t, driver.KindMessages, blocks(10, 1, 2, 3, 4), false)
	if five.write != four.write || five.write == 0 {
		t.Fatalf("five breakpoints wrote %d, four %d", five.write, four.write)
	}
	// Write a prefix ending at block 2, then ask with a breakpoint at block
	// 30: block 2 is more than 20 blocks back, so it is a miss; with the
	// breakpoint at block 21 it is found.
	p.run(t, driver.KindMessages, blocks(40, 2), true)
	if far := p.run(t, driver.KindMessages, blocks(40, 30), false); far.read != 0 {
		t.Fatalf("lookback beyond 20 blocks read %d", far.read)
	}
	if near := p.run(t, driver.KindMessages, blocks(40, 21), false); near.read == 0 {
		t.Fatal("lookback within 20 blocks missed")
	}
}

// TestPCAutomaticRules: granularity, exact boundaries, the minimum, the
// candidate cap, prompt_cache_key isolation and the accounts.
func TestPCAutomaticRules(t *testing.T) {
	mem := limits.NewMemory()
	chat := func(n int, extra string) string {
		return `{"model":"m",` + extra + `"messages":[{"role":"system","content":"` + words(n, "lorem") + `"},{"role":"user","content":"hi"}]}`
	}
	p := newPCEnv(t, "openai", mem, &config.PromptCacheRule{MinTokens: intp(64), Granularity: intp(32)})
	if s := p.run(t, driver.KindChat, chat(10, ""), true); s.read != 0 || s.commit != nil {
		t.Fatalf("below the minimum: %+v", s)
	}
	p.run(t, driver.KindChat, chat(200, ""), true)
	hit := p.run(t, driver.KindChat, chat(200, ""), false)
	if hit.read < 64 || (hit.read-64)%32 != 0 || hit.write != 0 {
		t.Fatalf("hit %+v", hit)
	}
	// Another prompt_cache_key misses; the same key hits.
	if s := p.run(t, driver.KindChat, chat(200, `"prompt_cache_key":"a",`), true); s.read != 0 {
		t.Fatalf("other key read %d", s.read)
	}
	if s := p.run(t, driver.KindChat, chat(200, `"prompt_cache_key":"a",`), false); s.read != hit.read {
		t.Fatalf("same key read %d, want %d", s.read, hit.read)
	}
	// Another account misses; a key without account and no key at all are
	// their own owners.
	p.key = &config.KeyConfig{Label: "k2", Account: "other"}
	if s := p.run(t, driver.KindChat, chat(200, ""), true); s.read != 0 || s.account != (limits.Entity{Kind: limits.KindAccount, ID: "other"}) {
		t.Fatalf("other account: %+v", s)
	}
	p.key = &config.KeyConfig{Label: "k3"}
	if s := p.run(t, driver.KindChat, chat(200, ""), false); s.account != (limits.Entity{Kind: limits.KindKey, ID: "k3"}) {
		t.Fatalf("key without account: %+v", s.account)
	}
	p.key = nil
	if s := p.run(t, driver.KindChat, chat(200, ""), false); s.account != (limits.Entity{Kind: limits.KindIP, ID: "198.51.100.1"}) {
		t.Fatalf("no key: %+v", s.account)
	}

	// Granularity 0: multiples of 128 and block ends; the whole prompt is
	// a candidate, so an identical prompt reads all of it.
	ex := newPCEnv(t, "openai", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(10), Granularity: intp(0)})
	ex.run(t, driver.KindChat, chat(300, ""), true)
	full := ex.run(t, driver.KindChat, chat(300, ""), false)
	n := generate.CountPrompt(generate.Simulated{}, mustParse(t, ex.drv, chat(300, "")), 85).Total
	if full.read != n {
		t.Fatalf("exact boundary read %d, want the whole %d", full.read, n)
	}
	// The cap keeps the 1 024 longest candidates.
	capd := newPCEnv(t, "openai", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(1), Granularity: intp(1)})
	if s := capd.run(t, driver.KindChat, chat(1500, ""), false); len(s.commit) != limits.PromptCacheMaxCandidates {
		t.Fatalf("candidates %d, want %d", len(s.commit), limits.PromptCacheMaxCandidates)
	}
	// kind none never calls the store.
	none := newPCEnv(t, "openai", failingCacher{limits.NewMemory()}, &config.PromptCacheRule{Kind: pcNone})
	if s := none.run(t, driver.KindChat, chat(200, ""), true); s.commit != nil {
		t.Fatalf("kind none: %+v", s)
	}
}

func mustParse(t *testing.T, drv driver.Driver, body string) *core.Request {
	t.Helper()
	req, perr := drv.ParseRequest(driver.KindChat, []byte(body), driver.MapHeaders{})
	if perr != nil {
		t.Fatal(perr)
	}
	return req
}

// failingCacher is a store whose prompt cache always fails.
type failingCacher struct{ *limits.Memory }

func (failingCacher) PromptCacheLookup(context.Context, limits.Entity, []string, time.Time) (int, error) {
	return -1, errors.New("down")
}

func (failingCacher) PromptCacheCommit(context.Context, limits.Entity, []limits.PromptCacheEntry, time.Time) (int, error) {
	return 0, errors.New("down")
}

// plainStore hides the optional interfaces.
type plainStore struct{ limits.Store }

// TestPCStoreFailures: a lookup error is a miss, a commit error only logs,
// a store without the interface and an unreadable body skip the cache.
func TestPCStoreFailures(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"` + words(300, "lorem") + `"}]}`
	p := newPCEnv(t, "openai", failingCacher{limits.NewMemory()}, &config.PromptCacheRule{MinTokens: intp(16)})
	if s := p.run(t, driver.KindChat, body, true); s.read != 0 {
		t.Fatalf("failing store read %d", s.read)
	}
	p.e.Log = nil
	p.run(t, driver.KindChat, body, true) // no logger: nothing to log to
	q := newPCEnv(t, "openai", plainStore{limits.NewMemory()}, &config.PromptCacheRule{MinTokens: intp(16)})
	if s := q.run(t, driver.KindChat, body, true); s.commit != nil {
		t.Fatalf("store without PromptCacher: %+v", s)
	}
	// An explicit rule needs the raw body.
	a := newPCEnv(t, "anthropic", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(1)})
	req := &core.Request{Raw: []byte("{"), Messages: []core.Message{{Role: "user", Content: words(50, "x")}}}
	r := &request{in: Input{Driver: a.drv}, decl: driver.EngineDeclOf(a.drv), req: req, model: a.m, key: a.key, tk: tokenizerFor(a.m)}
	r.prompt = generate.CountPrompt(r.tk, req, 85)
	a.e.promptCacheLookup(context.Background(), r)
	if r.pc != nil {
		t.Fatalf("broken body: %+v", r.pc)
	}
	// No model: nothing to do.
	r.model = nil
	a.e.promptCacheLookup(context.Background(), r)
	if r.pc != nil {
		t.Fatal("no model")
	}
}

// TestCostWithCacheWrites: writes at cache_write_per_m (input when unset),
// 1 h writes at 2 × input, reads at cache_read_per_m.
func TestCostWithCacheWrites(t *testing.T) {
	u := core.Usage{PromptTokens: 1000, CachedTokens: 100, CacheWriteTokens: 300, CacheWrite1hTokens: 100, CompletionTokens: 10}
	p := config.Pricing{InputPerM: 1, OutputPerM: 5, CacheReadPerM: 0.1, CacheWritePerM: 1.25}
	if got, want := costMicroUSD(p, u), int64(600+10+250+200+50); got != want {
		t.Fatalf("cost %d, want %d", got, want)
	}
	p.CacheWritePerM = 0
	if got, want := costMicroUSD(p, u), int64(600+10+200+200+50); got != want {
		t.Fatalf("cost without write price %d, want %d", got, want)
	}
}

// TestPCTailAfterLastBreakpoint: with the last breakpoint on the last user
// block, the closing of the turn stays in input_tokens (task 394, fixture
// anthropic/mixed_ttl), also when the Claude template counts the prompt
// below the layout's chat frame.
func TestPCTailAfterLastBreakpoint(t *testing.T) {
	long := words(300, "lorem")
	for _, model := range []string{"m", "claude-haiku-4-5", "claude-opus-5-5"} {
		p := newPCEnv(t, "anthropic", limits.NewMemory(), &config.PromptCacheRule{MinTokens: intp(16)})
		p.m.ID = model
		body := `{"model":"` + model + `","max_tokens":5,
 "system":[{"type":"text","text":"` + long + `","cache_control":{"type":"ephemeral","ttl":"1h"}}],
 "messages":[{"role":"user","content":[{"type":"text","text":"` + long + `","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`
		req, perr := p.drv.ParseRequest(driver.KindMessages, []byte(body), driver.MapHeaders{})
		if perr != nil {
			t.Fatal(perr)
		}
		total := generate.CountPrompt(tokenizerFor(p.m), req, 85).Total
		first := p.run(t, driver.KindMessages, body, true)
		second := p.run(t, driver.KindMessages, body, true)
		if first.write == 0 || first.write1h == 0 || first.write1h == first.write || total-first.write <= 0 {
			t.Fatalf("%s first: %+v of %d", model, first, total)
		}
		if second.read != first.write || second.write != 0 || total-second.read != total-first.write {
			t.Fatalf("%s second: %+v (first %+v) of %d", model, second, first, total)
		}
	}
}
