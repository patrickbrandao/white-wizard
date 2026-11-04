package engine_test

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// I20: latência por carga (latency.model per_token/polynomial) e
// speedup_ratio, end to end through the engine with clock: logical.

const i20Cfg = `
server: {seed: 20}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - id: m-pt
    errors: none
    latency: {model: per_token, prefill_overhead: 2ms, prefill_per_token: 1ms, clock: logical}
  - id: m-sp
    errors: none
    latency: {ttft: 40ms, speedup_ratio: 4, clock: logical}
`

// TestI20PerTokenPrefill: TTFT = prefill_overhead + prompt tokens × per token.
func TestI20PerTokenPrefill(t *testing.T) {
	e := newEngine(t, i20Cfg)
	c, _ := impChatCall(t, e, nil, impBody("m-pt", ""))
	rec := impLastRecord(t, e)
	if want := int64(2 + c.Usage.PromptTokens); rec.TTFTMS != want || c.Usage.PromptTokens == 0 {
		t.Fatalf("ttft %d ms, want %d (prompt %d tokens)", rec.TTFTMS, want, c.Usage.PromptTokens)
	}
	// A longer prompt takes longer.
	long := `{"model":"m-pt","messages":[{"role":"user","content":"` + longText(400) + `"}]}`
	c2, _ := impChatCall(t, e, nil, long)
	if rec2 := impLastRecord(t, e); rec2.TTFTMS != int64(2+c2.Usage.PromptTokens) || rec2.TTFTMS <= rec.TTFTMS {
		t.Fatalf("long prompt ttft %d ms (prompt %d)", rec2.TTFTMS, c2.Usage.PromptTokens)
	}
}

// TestI20SpeedupRatio: speedup_ratio divides every stage.
func TestI20SpeedupRatio(t *testing.T) {
	e := newEngine(t, i20Cfg)
	impChatCall(t, e, nil, impBody("m-sp", ""))
	if rec := impLastRecord(t, e); rec.TTFTMS != 10 {
		t.Fatalf("ttft %d ms, want 40/4 = 10", rec.TTFTMS)
	}
}

func longText(words int) string {
	b := make([]byte, 0, words*6)
	for i := 0; i < words; i++ {
		b = append(b, "lorem "...)
	}
	return string(b)
}

// i20CacheCfg: an automatic prompt cache of 64 tokens in steps of 32 and a
// per-token prefill: a hit is not prefilled (spec 008) and usage reports
// the tokens read from the cache (spec 007, ADR 0046).
const i20CacheCfg = `
server: {seed: 20}
models:
  - id: m-cache
    errors: none
    pricing: {input_per_m: 2, output_per_m: 0, cache_read_per_m: 1}
    latency: {model: per_token, prefill_overhead: 2ms, prefill_per_token: 1ms, clock: logical}
    prompt_cache: {kind: automatic, min_tokens: 64, granularity: 32}
`

// TestI20CachedTokensUsage: the second identical request reads the cache;
// each provider reports it in its own format, the TTFT skips it and the
// cost discounts it.
func TestI20CachedTokensUsage(t *testing.T) {
	e := newEngine(t, i20CacheCfg)
	body := `{"model":"m-cache","messages":[{"role":"user","content":"` + longText(100) + `"}]}`

	// OpenAI: usage.prompt_tokens_details.cached_tokens.
	miss := oaiCall(t, call(t, e, "openai", "/v1/chat/completions", impAuth, body))
	if rec := impLastRecord(t, e); miss.Usage.PromptTokensDetails.CachedTokens != 0 || rec.TTFTMS != int64(2+miss.Usage.PromptTokens) {
		t.Fatalf("miss: cached %d ttft %d", miss.Usage.PromptTokensDetails.CachedTokens, rec.TTFTMS)
	}
	oai := oaiCall(t, call(t, e, "openai", "/v1/chat/completions", impAuth, body))
	prompt := oai.Usage.PromptTokens
	cached := cachedOf(prompt, 64, 32)
	if prompt == 0 || oai.Usage.PromptTokensDetails.CachedTokens != cached {
		t.Fatalf("cached_tokens %d, want %d of %d", oai.Usage.PromptTokensDetails.CachedTokens, cached, prompt)
	}
	if rec := impLastRecord(t, e); rec.TTFTMS != int64(2+prompt-cached) {
		t.Fatalf("ttft %d ms, want %d (cached tokens not prefilled)", rec.TTFTMS, 2+prompt-cached)
	}

	// OpenAI streaming with include_usage carries the same usage.
	sbody := strings.Replace(body, `"messages"`, `"stream":true,"stream_options":{"include_usage":true},"messages"`, 1)
	sw := call(t, e, "openai", "/v1/chat/completions", impAuth, sbody)
	if want := `"cached_tokens":` + strconv.Itoa(cached); !strings.Contains(string(sw.Body()), want) {
		t.Fatalf("stream usage lacks %s: %s", want, sw.Body())
	}

	// Anthropic: usage.cache_read_input_tokens.
	ah := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	abody := `{"model":"m-cache","max_tokens":64,"messages":[{"role":"user","content":"` + longText(100) + `"}]}`
	call(t, e, "anthropic", "/v1/messages", ah, abody)
	aw := call(t, e, "anthropic", "/v1/messages", ah, abody)
	var anth struct {
		Usage struct {
			CacheRead int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(aw.Body(), &anth); err != nil || aw.StatusCode() != 200 {
		t.Fatalf("anthropic status %d %v: %s", aw.StatusCode(), err, aw.Body())
	}
	if anth.Usage.CacheRead == 0 {
		t.Fatalf("cache_read_input_tokens = 0: %s", aw.Body())
	}

	// OpenRouter: usage.cost bills cached tokens at cache_read_per_m.
	call(t, e, "openrouter", "/api/v1/chat/completions", impAuth, body)
	or := oaiCall(t, call(t, e, "openrouter", "/api/v1/chat/completions", impAuth, body))
	c := cachedOf(or.Usage.PromptTokens, 64, 32)
	if want := float64((or.Usage.PromptTokens-c)*2+c) / 1e6; math.Abs(or.Usage.Cost-want) > 1e-9 {
		t.Fatalf("cost %g, want %g (cached at the cache-read price)", or.Usage.Cost, want)
	}
}
