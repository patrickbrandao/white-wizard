package engine_test

import (
	"strconv"
	"testing"
)

// mustHeaders checks that every name is present and, when want has a value
// for it, equal to it.
func mustHeaders(t *testing.T, what string, h map[string]string, want map[string]string, names ...string) {
	t.Helper()
	for _, n := range names {
		v, ok := h[n]
		if !ok {
			t.Errorf("%s: header %s missing (%v)", what, n, h)
			continue
		}
		if w, ok := want[n]; ok && v != w {
			t.Errorf("%s: %s = %q, want %q", what, n, v, w)
		}
	}
}

// TestRateLimit429TokenHeadersOpenAI (task 064): a 429 by rpm still carries
// x-ratelimit-*-tokens, as the real OpenAI 429 does.
func TestRateLimit429TokenHeadersOpenAI(t *testing.T) {
	e := newEngine(t, fastCfg+`
keys:
  - {key: k-rpm, label: rpm, limits: {rpm: 1, tpm: 100000}}
`)
	h := map[string]string{"Authorization": "Bearer k-rpm"}
	body := `{"model":"wwz-fast","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`
	if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 200 {
		t.Fatalf("first: %d %s", w.StatusCode(), w.Body())
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, body)
	if w.StatusCode() != 429 {
		t.Fatalf("second: %d %s", w.StatusCode(), w.Body())
	}
	hs := w.Headers()
	mustHeaders(t, "openai 429", hs, map[string]string{"x-ratelimit-limit-requests": "1", "x-ratelimit-remaining-requests": "0", "x-ratelimit-limit-tokens": "100000"},
		"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens", "retry-after")
	if rem, err := strconv.Atoi(hs["x-ratelimit-remaining-tokens"]); err != nil || rem <= 0 || rem >= 100000 {
		t.Errorf("x-ratelimit-remaining-tokens = %q, want the tokens left after the first request", hs["x-ratelimit-remaining-tokens"])
	}
}

// TestRateLimit429TokenHeadersAnthropic (task 064): a 429 by rpm carries
// anthropic-ratelimit-{tokens,input-tokens,output-tokens}-*, and tokens-*
// is input + output when only itpm/otpm are configured (spec 006).
func TestRateLimit429TokenHeadersAnthropic(t *testing.T) {
	e := newEngine(t, fastCfg+`
keys:
  - {key: k-anth, label: anth, limits: {rpm: 1, itpm: 20000, otpm: 8000}}
`)
	h := map[string]string{"x-api-key": "k-anth", "anthropic-version": "2023-06-01"}
	body := `{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`
	w := call(t, e, "anthropic", "/v1/messages", h, body)
	if w.StatusCode() != 200 {
		t.Fatalf("first: %d %s", w.StatusCode(), w.Body())
	}
	names := []string{
		"anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-limit", "anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-limit", "anthropic-ratelimit-output-tokens-remaining", "anthropic-ratelimit-output-tokens-reset",
	}
	want := map[string]string{"anthropic-ratelimit-tokens-limit": "28000", "anthropic-ratelimit-input-tokens-limit": "20000", "anthropic-ratelimit-output-tokens-limit": "8000"}
	mustHeaders(t, "anthropic 200", w.Headers(), want, names...)

	w = call(t, e, "anthropic", "/v1/messages", h, body)
	if w.StatusCode() != 429 {
		t.Fatalf("second: %d %s", w.StatusCode(), w.Body())
	}
	want["anthropic-ratelimit-requests-limit"], want["anthropic-ratelimit-requests-remaining"] = "1", "0"
	mustHeaders(t, "anthropic 429", w.Headers(), want, append(names, "anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining", "retry-after")...)
}
