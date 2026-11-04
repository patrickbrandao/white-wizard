package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 250: the OpenRouter key routes read the key's spend and the
// account's credits from the store; the abuse ladder climbs one rung per
// escalation and its 403 carries the provider's text.

const keyInfoCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {mode: random, output_tokens: {dist: uniform, min: 100, max: 100}}
models:
  - {id: openai/gpt-4o-mini, protocols: [openai], latency: fast, generation: fixed, pricing: {input_per_m: 1, output_per_m: 1000}}
accounts:
  - {id: acme, limits: {credits_usd: 28}}
keys:
  - {key: sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef, label: capped, account: acme, limits: {credits_usd: 2}}
  - {key: k-free, label: free, account: poor}
  - {key: k-monthly, label: monthly, account: acme, limits: {spend_monthly_usd: 3}}
`

type keyInfoData struct {
	Data struct {
		Label          string   `json:"label"`
		Limit          *float64 `json:"limit"`
		LimitReset     *string  `json:"limit_reset"`
		LimitRemaining *float64 `json:"limit_remaining"`
		Usage          float64  `json:"usage"`
		UsageMonthly   float64  `json:"usage_monthly"`
		IsFreeTier     bool     `json:"is_free_tier"`
		FreeDaily      struct {
			Limit int `json:"limit"`
		} `json:"free_model_daily_requests"`
		RateLimit struct {
			Requests int    `json:"requests"`
			Interval string `json:"interval"`
		} `json:"rate_limit"`
	} `json:"data"`
}

func getKeyInfo(t *testing.T, e *engine.Engine, key, path string) keyInfoData {
	t.Helper()
	w := call(t, e, "openrouter", path, map[string]string{"Authorization": "Bearer " + key}, "")
	if w.StatusCode() != 200 {
		t.Fatalf("%s: status %d: %s", path, w.StatusCode(), w.Body())
	}
	if cc := w.Headers()["Cache-Control"]; cc != "private, no-store" {
		t.Errorf("%s: cache-control %q", path, cc)
	}
	var out keyInfoData
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatalf("%s: %s: %v", path, w.Body(), err)
	}
	return out
}

func credits(t *testing.T, e *engine.Engine, key string) (total, used float64) {
	t.Helper()
	w := call(t, e, "openrouter", "/api/v1/credits", map[string]string{"Authorization": "Bearer " + key}, "")
	if w.StatusCode() != 200 {
		t.Fatalf("credits: status %d: %s", w.StatusCode(), w.Body())
	}
	var out struct {
		Data struct {
			Total float64 `json:"total_credits"`
			Usage float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Data.Total, out.Data.Usage
}

func TestKeyInfoReadsStoreState(t *testing.T) {
	e := newEngine(t, keyInfoCfg)
	const key = "sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	before := getKeyInfo(t, e, key, "/api/v1/key")
	d := before.Data
	if d.Label != "sk-or-v1-012...def" {
		t.Errorf("label %q, want the masked key", d.Label)
	}
	if d.Limit == nil || *d.Limit != 2 || d.LimitRemaining == nil || *d.LimitRemaining != 2 || d.LimitReset != nil {
		t.Errorf("limit %v remaining %v reset %v, want the key's own 2, 2, null", d.Limit, d.LimitRemaining, d.LimitReset)
	}
	if d.Usage != 0 || d.IsFreeTier || d.FreeDaily.Limit != 1000 {
		t.Errorf("usage %v free tier %v free daily %d, want 0 false 1000", d.Usage, d.IsFreeTier, d.FreeDaily.Limit)
	}
	if d.RateLimit.Requests != -1 || d.RateLimit.Interval != "10s" {
		t.Errorf("rate_limit %+v, want the real deprecated {-1, 10s}", d.RateLimit)
	}
	if total, used := credits(t, e, key); total != 28 || used != 0 {
		t.Errorf("credits before: total %v used %v, want 28 0", total, used)
	}

	w := call(t, e, "openrouter", "/api/v1/chat/completions", map[string]string{"Authorization": "Bearer " + key},
		`{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 200 {
		t.Fatalf("chat: %d %s", w.StatusCode(), w.Body())
	}

	after := getKeyInfo(t, e, key, "/api/v1/auth/key").Data
	if after.Usage <= 0 || after.Usage != after.UsageMonthly {
		t.Errorf("usage %v monthly %v after a paid call, want > 0 and equal", after.Usage, after.UsageMonthly)
	}
	if after.LimitRemaining == nil || *after.LimitRemaining >= 2 || *after.Limit != 2 {
		t.Errorf("limit %v remaining %v after a paid call, want 2 and < 2", after.Limit, after.LimitRemaining)
	}
	total, used := credits(t, e, key)
	if total != 28 || used <= 0 || used > 28 {
		t.Errorf("credits after: total %v used %v, want 28 and the cost of the call", total, used)
	}

	// The monthly ceiling is the limit with limit_reset "monthly".
	m := getKeyInfo(t, e, "k-monthly", "/api/v1/key").Data
	if m.Limit == nil || *m.Limit != 3 || m.LimitReset == nil || *m.LimitReset != "monthly" || m.Label != "k-m..." {
		t.Errorf("monthly key: limit %v reset %v label %q", m.Limit, m.LimitReset, m.Label)
	}

	// No credits anywhere: free tier, no limit, 50 free requests a day.
	f := getKeyInfo(t, e, "k-free", "/api/v1/key").Data
	if !f.IsFreeTier || f.Limit != nil || f.LimitRemaining != nil || f.FreeDaily.Limit != 50 {
		t.Errorf("free key: free tier %v limit %v remaining %v free daily %d", f.IsFreeTier, f.Limit, f.LimitRemaining, f.FreeDaily.Limit)
	}
	if total, used := credits(t, e, "k-free"); total != 0 || used != 0 {
		t.Errorf("free credits: total %v used %v", total, used)
	}
}

func TestKeyManagementRefusesInferenceKey(t *testing.T) {
	e := newEngine(t, keyInfoCfg)
	w := call(t, e, "openrouter", "/api/v1/keys", map[string]string{"Authorization": "Bearer k-free"}, "")
	if w.StatusCode() != 401 || string(w.Body()) != `{"error":{"message":"Invalid API key","code":401}}` {
		t.Fatalf("keys: %d %s", w.StatusCode(), w.Body())
	}
	// Without a key the route answers the 401 of the key routes, with their
	// Cache-Control.
	w = call(t, e, "openrouter", "/api/v1/credits", nil, "")
	if w.StatusCode() != 401 || w.Headers()["Cache-Control"] != "private, no-store" {
		t.Fatalf("credits without key: %d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
}

const ladderCfg = `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gpt-4o-mini, latency: fast}
keys:
  - {key: k-abuse, label: abuse, account: a, limits: {rpm: 1}}
abuse:
  enabled: true
  strike_429: {count: 2, window: 1m}
  ladder:
    - {action: warn}
    - {action: suspend, duration: 10m}
`

func TestAbuseLadderClimbsOneRungPerEscalation(t *testing.T) {
	e := newEngine(t, ladderCfg)
	status := func() (int, string) {
		w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-abuse"}, chatBody)
		return w.StatusCode(), string(w.Body())
	}
	// 1: 200; 2: 429 (strike 1); 3: 429 (strike 2 = count: warn, rung 0);
	// 4: 429 (strike 3: suspend, rung 1); 5: 403.
	want := []int{200, 429, 429, 429, 403}
	var last string
	for i, w := range want {
		got, body := status()
		if got != w {
			t.Fatalf("request %d: status %d, want %d: %s", i+1, got, w, body)
		}
		last = body
	}
	if strings.Contains(last, "abuse ladder") || !strings.Contains(last, "account_suspended") {
		t.Errorf("403 of the ladder: %s, want the provider's account_suspended text", last)
	}
	ent := limits.Entity{Kind: limits.KindKey, ID: "abuse"}
	st, err := e.Store.Get(context.Background(), ent)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != limits.StatusSuspended || st.By != "abuse-ladder" || st.Reason != "abuse ladder: suspend" || st.Strikes != 3 {
		t.Errorf("state %+v, want suspended by the ladder with 3 strikes", st)
	}

	// Unblock keeps the strikes (reset with scope strikes forgives them):
	// the next 429 of the window escalates again, at the last rung.
	if err := e.Store.Unblock(context.Background(), ent); err != nil {
		t.Fatal(err)
	}
	if got, body := status(); got != 429 {
		t.Fatalf("after unblock: %d %s, want 429", got, body)
	}
	if got, body := status(); got != 403 {
		t.Fatalf("after the next strike: %d %s, want 403", got, body)
	}
}

func TestAbuseLadderOpenRouterSuspendedText(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: openai/gpt-4o-mini, protocols: [openai], latency: fast}
keys:
  - {key: k-or, label: or, account: a, limits: {rpm: 1}}
abuse:
  enabled: true
  strike_429: {count: 1, window: 1m}
  ladder:
    - {action: suspend, duration: 10m}
`)
	body := `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	var w interface {
		StatusCode() int
		Body() []byte
	}
	for range 3 {
		w = call(t, e, "openrouter", "/api/v1/chat/completions", map[string]string{"Authorization": "Bearer k-or"}, body)
	}
	if w.StatusCode() != 403 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Code != 403 || out.Error.Message != "Your account has been suspended." {
		t.Errorf("403 %s, want OpenRouter's suspension text", w.Body())
	}
}
