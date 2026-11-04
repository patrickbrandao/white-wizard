package engine_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I16: usage.extended adds x-wwz-input-breakdown (input tokens by origin) to
// every inference response; the body and the provider usage never change
// (spec 013 "Decomposição da entrada", ADR 0027, ADR 0014).

const i16Cfg = `
server: {seed: 42}
usage: {extended: %s}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
    cachey: {model: per_token, prefill_per_token: 1us, clock: logical}
  errors:
    boom: {rate: 100, types: [bad_gateway]}
models:
  - {id: i16-plain, latency: fast, errors: none, context_window: 128000}
  - {id: i16-cache, latency: cachey, errors: none, context_window: 128000, prompt_cache: {kind: automatic, min_tokens: 8, granularity: 4}}
  - {id: i16-boom, latency: fast, errors: boom, context_window: 128000}
  - {id: i16-tiny, latency: fast, errors: none, context_window: 5}
`

func i16Engine(t *testing.T, extended bool) *engine.Engine {
	t.Helper()
	v := "false"
	if extended {
		v = "true"
	}
	return newEngine(t, strings.Replace(i16Cfg, "%s", v, 1))
}

type i16Route struct {
	drv, path string
	anth      bool
	headers   map[string]string
}

var i16Routes = []i16Route{
	{"openai", "/v1/chat/completions", false, map[string]string{"Authorization": "Bearer wwz-key-01"}},
	{"anthropic", "/v1/messages", true, map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}},
	{"openrouter", "/api/v1/chat/completions", false, map[string]string{"Authorization": "Bearer wwz-key-01"}},
	{"opencode", "/zen/go/v1/chat/completions", false, map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}},
}

// i16Body is a conversation with every origin: system, tool schemas, user,
// assistant with a tool call and the tool result.
func i16Body(rt i16Route, model string, stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	if rt.anth {
		return `{"model":"` + model + `","max_tokens":32,"stream":` + s + `,"system":"be terse",
 "tools":[{"name":"read_file","description":"reads","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
 "messages":[{"role":"user","content":"read a.txt"},
  {"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hello world"}]}]}`
	}
	return `{"model":"` + model + `","max_tokens":32,"stream":` + s + `,
 "tools":[{"type":"function","function":{"name":"read_file","description":"reads","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
 "messages":[{"role":"system","content":"be terse"},{"role":"user","content":"read a.txt"},
  {"role":"assistant","content":"ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},
  {"role":"tool","tool_call_id":"call_1","content":"hello world"}]}`
}

type i16Breakdown struct {
	System      int `json:"system"`
	ToolSchemas int `json:"tool_schemas"`
	User        int `json:"user"`
	Assistant   int `json:"assistant"`
	ToolCalls   int `json:"tool_calls"`
	ToolResults int `json:"tool_results"`
	Images      int `json:"images"`
	Cached      int `json:"cached"`
	Total       int `json:"total"`
}

var i16Keys = []string{"system", "tool_schemas", "user", "assistant", "tool_calls", "tool_results", "images", "cached", "total"}

func i16Parse(t *testing.T, h string) i16Breakdown {
	t.Helper()
	// Compact JSON with exactly the spec's keys, in order.
	re := regexp.MustCompile(`^\{"system":\d+,"tool_schemas":\d+,"user":\d+,"assistant":\d+,"tool_calls":\d+,"tool_results":\d+,"images":\d+,"cached":\d+,"total":\d+\}$`)
	if !re.MatchString(h) {
		t.Fatalf("x-wwz-input-breakdown %q is not the compact form", h)
	}
	var raw map[string]int
	if err := json.Unmarshal([]byte(h), &raw); err != nil || len(raw) != len(i16Keys) {
		t.Fatalf("breakdown %q: %v", h, err)
	}
	var b i16Breakdown
	_ = json.Unmarshal([]byte(h), &b)
	sum := b.System + b.ToolSchemas + b.User + b.Assistant + b.ToolCalls + b.ToolResults + b.Images
	if sum != b.Total {
		t.Fatalf("breakdown %+v: parts sum %d != total", b, sum)
	}
	for _, v := range []int{b.System, b.ToolSchemas, b.User, b.Assistant, b.ToolCalls, b.ToolResults} {
		if v == 0 {
			t.Fatalf("breakdown %+v: every origin in the request must count", b)
		}
	}
	return b
}

// i16Usage reads prompt and cached tokens from a non-streaming body in the
// route's dialect.
func i16Usage(t *testing.T, rt i16Route, body []byte) (prompt, cached int) {
	t.Helper()
	var v struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			InputTokens         int `json:"input_tokens"`
			CacheRead           int `json:"cache_read_input_tokens"`
			CacheWrite          int `json:"cache_creation_input_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if rt.anth {
		// Anthropic input_tokens excludes the cache; the prompt is the sum.
		return v.Usage.InputTokens + v.Usage.CacheRead + v.Usage.CacheWrite, v.Usage.CacheRead
	}
	return v.Usage.PromptTokens, v.Usage.PromptTokensDetails.CachedTokens
}

// volatile masks ids and timestamps minted per request, so two bodies can be
// compared byte for byte (also in indented JSON, where a space follows the
// colon; "created" has one-second resolution, so it differed across a tick).
var i16Volatile = regexp.MustCompile(`"(id|created|created_at|system_fingerprint)":\s*("[^"]*"|\d+)|(chatcmpl|msg|gen|req)[-_][A-Za-z0-9]+`)

func i16Mask(b []byte) string { return i16Volatile.ReplaceAllString(string(b), "<v>") }

// TestI16HeaderConsistentWithUsage: with usage.extended the header is present
// in the 4 drivers, stream and non-stream, with total == usage prompt tokens
// and cached == the cache read the body reports.
func TestI16HeaderConsistentWithUsage(t *testing.T) {
	for _, model := range []string{"i16-plain", "i16-cache"} {
		for _, rt := range i16Routes {
			e := i16Engine(t, true)
			if model == "i16-cache" {
				call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, model, false)) // writes the cache
			}
			w := call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, model, false))
			if w.StatusCode() != 200 {
				t.Fatalf("%s %s: status %d: %s", rt.drv, model, w.StatusCode(), w.Body())
			}
			b := i16Parse(t, w.Headers()["x-wwz-input-breakdown"])
			prompt, cached := i16Usage(t, rt, w.Body())
			if b.Total != prompt || b.Cached != cached {
				t.Fatalf("%s %s: breakdown %+v vs usage prompt=%d cached=%d", rt.drv, model, b, prompt, cached)
			}
			if model == "i16-cache" && b.Cached == 0 {
				t.Fatalf("%s: the cache hit must show in cached: %+v", rt.drv, b)
			}

			ws := call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, model, true))
			if ws.StatusCode() != 200 {
				t.Fatalf("%s stream: status %d", rt.drv, ws.StatusCode())
			}
			bs := i16Parse(t, ws.Headers()["x-wwz-input-breakdown"])
			if bs.Total != b.Total {
				t.Fatalf("%s: stream total %d != non-stream %d", rt.drv, bs.Total, b.Total)
			}
		}
	}
}

// TestI16BodyUnchanged: with extended false the header is absent and the body
// is byte-identical (ids and timestamps masked) to the extended one.
func TestI16BodyUnchanged(t *testing.T) {
	for _, rt := range i16Routes {
		for _, stream := range []bool{false, true} {
			on := call(t, i16Engine(t, true), rt.drv, rt.path, rt.headers, i16Body(rt, "i16-cache", stream))
			off := call(t, i16Engine(t, false), rt.drv, rt.path, rt.headers, i16Body(rt, "i16-cache", stream))
			if _, ok := off.Headers()["x-wwz-input-breakdown"]; ok {
				t.Fatalf("%s stream=%v: header present with extended false", rt.drv, stream)
			}
			if _, ok := on.Headers()["x-wwz-input-breakdown"]; !ok {
				t.Fatalf("%s stream=%v: header missing with extended true", rt.drv, stream)
			}
			if a, b := i16Mask(on.Body()), i16Mask(off.Body()); a != b {
				t.Fatalf("%s stream=%v: body changed\non:  %s\noff: %s", rt.drv, stream, a, b)
			}
		}
	}
}

// TestI16ErrorsAfterPrompt: an error decided after the prompt was counted
// (injected chaos, context length) carries the header in all 4
// drivers; an error before counting (bad key) does not.
func TestI16ErrorsAfterPrompt(t *testing.T) {
	e := i16Engine(t, true)
	for _, rt := range i16Routes {
		for _, model := range []string{"i16-boom", "i16-tiny"} {
			w := call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, model, false))
			if w.StatusCode() < 400 {
				t.Fatalf("%s %s: status %d, want an error", rt.drv, model, w.StatusCode())
			}
			i16Parse(t, w.Headers()["x-wwz-input-breakdown"])
		}
		w := call(t, e, rt.drv, rt.path, map[string]string{"Authorization": "Bearer nope"}, i16Body(rt, "i16-plain", false))
		if w.StatusCode() != 401 {
			t.Fatalf("%s: status %d, want 401", rt.drv, w.StatusCode())
		}
		if _, ok := w.Headers()["x-wwz-input-breakdown"]; ok {
			t.Fatalf("%s: header on an error before the prompt count", rt.drv)
		}
	}
}

// TestI16Reload: usage.extended is read per request, so swapping the
// configuration turns the header on and off without a restart.
func TestI16Reload(t *testing.T) {
	e := i16Engine(t, false)
	rt := i16Routes[0]
	if _, ok := call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, "i16-boom", false)).Headers()["x-wwz-input-breakdown"]; ok {
		t.Fatal("header with extended false")
	}
	for _, v := range []string{"true", "false"} {
		cfg, err := config.Parse([]byte(strings.Replace(i16Cfg, "%s", v, 1)))
		if err != nil {
			t.Fatal(err)
		}
		e.SetConfig(cfg)
		// i16-boom reuses the same chaos path: a stale header must not leak
		// from the previous request's error.
		_, got := call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, "i16-boom", false)).Headers()["x-wwz-input-breakdown"]
		if got != (v == "true") {
			t.Fatalf("extended %s: header present=%v", v, got)
		}
		_, got = call(t, e, rt.drv, rt.path, rt.headers, i16Body(rt, "i16-plain", false)).Headers()["x-wwz-input-breakdown"]
		if got != (v == "true") {
			t.Fatalf("extended %s (200): header present=%v", v, got)
		}
	}
}
