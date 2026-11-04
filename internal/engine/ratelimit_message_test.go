package engine_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// errMessage decodes error.message from an OpenAI or Anthropic envelope.
func errMessage(t *testing.T, b []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return env.Error.Message
}

// TestRateLimitMessageProviderText (task 063): a store denial is rendered
// with the provider's 429 text quoting the real limit, not the store's
// internal "rate limit of N requests per minute exceeded".
func TestRateLimitMessageProviderText(t *testing.T) {
	cfg := fastCfg + `
keys:
  - {key: k-rpm, label: rpm, limits: {rpm: 1}}
  - {key: k-tpm, label: tpm, limits: {tpm: 50}}
  - {key: k-anth, label: anth, limits: {rpm: 1}}
  - {key: k-ban, label: ban}
`
	e := newEngine(t, cfg)
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`

	h := map[string]string{"Authorization": "Bearer k-rpm"}
	if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 200 {
		t.Fatalf("first: %d %s", w.StatusCode(), w.Body())
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, body)
	msg := errMessage(t, w.Body())
	// The organization id derives from the key's account (task 201).
	want := regexp.MustCompile(`^Rate limit reached for wwz-fast in organization org-[A-Za-z0-9]{24} on requests per min \(RPM\): Limit 1, Used 1, Requested 1\. Please try again in `)
	if w.StatusCode() != 429 || !want.MatchString(msg) || !strings.HasSuffix(msg, ". Visit https://platform.openai.com/account/rate-limits to learn more.") {
		t.Fatalf("openai rpm message: %d %q", w.StatusCode(), msg)
	}

	tb := `{"model":"wwz-fast","max_tokens":200,"messages":[{"role":"user","content":"x"}]}`
	w = call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-tpm"}, tb)
	msg = errMessage(t, w.Body())
	if w.StatusCode() != 429 || !strings.Contains(msg, "on tokens per min (TPM): Limit 50, Used 0, Requested ") || strings.Contains(msg, "exceeded") {
		t.Fatalf("openai tpm message: %d %q", w.StatusCode(), msg)
	}

	ah := map[string]string{"x-api-key": "k-anth", "anthropic-version": "2023-06-01"}
	ab := `{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`
	if w := call(t, e, "anthropic", "/v1/messages", ah, ab); w.StatusCode() != 200 {
		t.Fatalf("anthropic first: %d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "anthropic", "/v1/messages", ah, ab)
	msg = errMessage(t, w.Body())
	if w.StatusCode() != 429 || !strings.HasPrefix(msg, "This request would exceed the rate limit for your organization (") || !strings.Contains(msg, ") of 1 requests per minute. For details, refer to: https://docs.claude.com/en/api/rate-limits.") {
		t.Fatalf("anthropic rpm message: %d %q", w.StatusCode(), msg)
	}

	// Ban reasons are operator text and pass through unchanged.
	if err := e.Store.Ban(context.Background(), limits.Entity{Kind: limits.KindKey, ID: "ban"}, "custom ban reason", "admin"); err != nil {
		t.Fatal(err)
	}
	w = call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-ban"}, body)
	if msg := errMessage(t, w.Body()); w.StatusCode() != 403 || msg != "custom ban reason" {
		t.Fatalf("ban message: %d %q", w.StatusCode(), msg)
	}
}
