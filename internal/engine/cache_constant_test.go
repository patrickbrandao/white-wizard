package engine_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// cacheConstantCfg: a fixed TTFT (latency model constant) and an automatic
// prompt cache of 64 tokens in steps of 32 (spec 007/008, ADR 0046).
const cacheConstantCfg = `
server: {seed: 100}
models:
  - id: m-const
    errors: none
    pricing: {input_per_m: 2, output_per_m: 0, cache_read_per_m: 1}
    latency: {model: constant, ttft: 100ms, cache_ttft_reduction: 0.5, clock: logical}
    prompt_cache: {kind: automatic, min_tokens: 64, granularity: 32}
`

// cachedOf is the automatic hit of a prompt of n tokens: the longest
// min + k·step that fits.
func cachedOf(n, min, step int) int {
	if n < min {
		return 0
	}
	return min + (n-min)/step*step
}

type oaiUsage struct {
	Usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func oaiCall(t *testing.T, w interface {
	Body() []byte
	StatusCode() int
},
) oaiUsage {
	t.Helper()
	var u oaiUsage
	if err := json.Unmarshal(w.Body(), &u); err != nil || w.StatusCode() != 200 {
		t.Fatalf("status %d %v: %s", w.StatusCode(), err, w.Body())
	}
	return u
}

// TestCacheHitConstantUsage: the first request misses; the same request
// again reads the longest cached prefix, the fixed TTFT shrinks by
// cache_ttft_reduction × cached/prompt and the cost bills the read at the
// cache-read price.
func TestCacheHitConstantUsage(t *testing.T) {
	e := newEngine(t, cacheConstantCfg)
	body := `{"model":"m-const","messages":[{"role":"user","content":"` + longText(100) + `"}]}`

	first := oaiCall(t, call(t, e, "openai", "/v1/chat/completions", impAuth, body))
	if first.Usage.PromptTokensDetails.CachedTokens != 0 {
		t.Fatalf("first request cached %d, want a miss", first.Usage.PromptTokensDetails.CachedTokens)
	}
	if rec := impLastRecord(t, e); rec.TTFTMS != 100 {
		t.Fatalf("miss ttft %d ms, want 100", rec.TTFTMS)
	}
	u := oaiCall(t, call(t, e, "openai", "/v1/chat/completions", impAuth, body))
	prompt := u.Usage.PromptTokens
	want := cachedOf(prompt, 64, 32)
	if prompt < 64 || u.Usage.PromptTokensDetails.CachedTokens != want {
		t.Fatalf("cached_tokens %d, want %d of %d", u.Usage.PromptTokensDetails.CachedTokens, want, prompt)
	}
	ttft := time.Duration(float64(100*time.Millisecond) * (1 - 0.5*float64(want)/float64(prompt)))
	if rec := impLastRecord(t, e); rec.TTFTMS != ttft.Milliseconds() {
		t.Fatalf("ttft %d ms, want %d", rec.TTFTMS, ttft.Milliseconds())
	}

	// OpenRouter is another provider: its own miss, then the hit billed at
	// the cache-read price.
	call(t, e, "openrouter", "/api/v1/chat/completions", impAuth, body)
	or := oaiCall(t, call(t, e, "openrouter", "/api/v1/chat/completions", impAuth, body))
	c := cachedOf(or.Usage.PromptTokens, 64, 32)
	if or.Usage.PromptTokensDetails.CachedTokens != c {
		t.Fatalf("openrouter cached %d, want %d", or.Usage.PromptTokensDetails.CachedTokens, c)
	}
	if want := float64((or.Usage.PromptTokens-c)*2+c) / 1e6; math.Abs(or.Usage.Cost-want) > 1e-9 {
		t.Fatalf("cost %g, want %g (cached at the cache-read price)", or.Usage.Cost, want)
	}
}
