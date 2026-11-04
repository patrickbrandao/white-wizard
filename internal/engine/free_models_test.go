package engine_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 262 (ADR 0060, spec 006 16.4): the requests to OpenRouter's :free
// models count on the account as a unit of their own, 20 a minute and 50 a
// day (1000 once the account got US$ 10 of credits), apart from its paid
// requests and its tier limits.

const freeModelsCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {mode: random, output_tokens: {dist: uniform, min: 5, max: 5}}
models:
  - {id: "meta-llama/llama-3.3-70b-instruct:free", protocols: [openai], latency: fast, generation: fixed}
  - {id: meta-llama/llama-3.3-70b-instruct, protocols: [openai], latency: fast, generation: fixed}
  - {id: gpt-4o-mini, protocols: [openai], latency: fast, generation: fixed}
accounts:
  - {id: rich, limits: {credits_usd: 28}}
keys:
  - {key: k-poor, label: poor, account: poor}
  - {key: k-rich, label: rich, account: rich}
  - {key: k-buyer, label: buyer, account: buyer}
  - {key: k-poor2, label: poor2, account: poor}
`

func freeChat(t *testing.T, call2 func(model, key string) (int, string), model, key string) int {
	t.Helper()
	st, _ := call2(model, key)
	return st
}

func TestFreeModelsPerMinuteOnTheAccount(t *testing.T) {
	e := newEngine(t, freeModelsCfg)
	chat := func(drv, path, model, key string) (int, string) {
		w := call(t, e, drv, path, map[string]string{"Authorization": "Bearer " + key},
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return w.StatusCode(), string(w.Body())
	}
	or := func(model, key string) (int, string) {
		return chat("openrouter", "/api/v1/chat/completions", model, key)
	}
	const free = "meta-llama/llama-3.3-70b-instruct:free"
	// Two keys of the same account share the 20 a minute.
	for i := range 20 {
		key := "k-poor"
		if i%2 == 1 {
			key = "k-poor2"
		}
		if st := freeChat(t, or, free, key); st != 200 {
			t.Fatalf("free request %d: status %d", i+1, st)
		}
	}
	// The 21st answers the real form (conferred on 2026-10-31): the
	// X-RateLimit-* headers repeated in metadata, no Retry-After, the
	// reset on a minute boundary.
	w := call(t, e, "openrouter", "/api/v1/chat/completions", map[string]string{"Authorization": "Bearer k-poor2"},
		`{"model":"`+free+`","messages":[{"role":"user","content":"hi"}]}`)
	reset := w.Headers()["X-RateLimit-Reset"]
	want := `{"error":{"message":"Rate limit exceeded: free-models-per-min. ","code":429,"metadata":{"headers":{"X-RateLimit-Limit":"20","X-RateLimit-Remaining":"0","X-RateLimit-Reset":"` + reset +
		`"},"limit_source":"openrouter_free_tier_per_minute","remedy_hint":"Slow down requests to free models, or retry after the per-minute window resets.","provider_name":null}},"user_id":"user_wwz"}`
	if got := strings.TrimSpace(string(w.Body())); w.StatusCode() != 429 || got != want {
		t.Fatalf("21st free request: %d\n got  %s\n want %s", w.StatusCode(), got, want)
	}
	if ms, err := strconv.ParseInt(reset, 10, 64); err != nil || ms%60000 != 0 {
		t.Errorf("X-RateLimit-Reset %q is not a minute boundary", reset)
	}
	for k := range w.Headers() {
		if strings.EqualFold(k, "retry-after") {
			t.Errorf("unexpected Retry-After")
		}
	}
	if w.Headers()["X-RateLimit-Limit"] != "20" || w.Headers()["X-RateLimit-Remaining"] != "0" {
		t.Errorf("X-RateLimit headers %v", w.Headers())
	}
	// The paid variant and another account are not counted.
	if st, body := or("meta-llama/llama-3.3-70b-instruct", "k-poor"); st != 200 {
		t.Errorf("paid request after the free cap: %d %s", st, body)
	}
	if st := freeChat(t, or, free, "k-rich"); st != 200 {
		t.Errorf("free request of another account: %d", st)
	}
	// The other drivers have no free-models unit.
	for range 25 {
		if st, body := chat("openai", "/v1/chat/completions", "gpt-4o-mini", "k-buyer"); st != 200 {
			t.Fatalf("openai: %d %s", st, body)
		}
	}
	// The unit is its own entity, not the account.
	ctx := context.Background()
	if s, err := e.Store.Get(ctx, limits.Entity{Kind: limits.KindFreeModels, ID: "poor"}); err != nil || s.Requests != 20 {
		t.Errorf("free unit of poor: %+v %v", s, err)
	}
	if s, err := e.Store.Get(ctx, limits.Entity{Kind: limits.KindAccount, ID: "poor"}); err != nil || s.Requests != 21 || s.FreeRequestsToday != 20 {
		t.Errorf("account poor: requests %d free today %d, want 21 and 20 (%v)", s.Requests, s.FreeRequestsToday, err)
	}
	// free_model_daily_requests reports the day's :free requests.
	d := getKeyInfo(t, e, "k-poor", "/api/v1/key")
	if d.Data.FreeDaily.Limit != 50 {
		t.Errorf("free daily limit %d, want 50", d.Data.FreeDaily.Limit)
	}
}
