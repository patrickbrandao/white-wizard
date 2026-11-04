package opencode_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// Task 422 (ADR 0072, items 3–14; spec 006 16.3): the public credential,
// the gate of the free models, the billing source and the gateway's
// errors, through the engine. No test sends User-Agent: opencode/... to
// the real provider; here it goes to the wwz only.

// ocClient is the User-Agent of the official client (only ever sent to
// the wwz).
const ocClient = "opencode/latest/2.2.5/wwz-test"

// billCfg: big-pickle is free on the pay-as-you-go catalog, beam-free on
// the Go one; deepseek-v4.1-flash is paid, with the Go catalog's price;
// gemini-4-argon is paid on both; glm-one keeps the list price as the
// allowance (allowance_factor 1). Every answer has 10 output tokens.
const billCfg = `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {mode: random, output_tokens: {dist: uniform, min: 10, max: 10}}
tiers:
  go:
    limits: {credits_usd: 10, quota_usd: {monthly_usd: 60, rolling_5h_pct: 20, weekly_pct: 50}}
  plus:
    plan: go-plus
    limits: {credits_usd: 10, quota_usd: {monthly_usd: 60}}
  teams:
    plan: go-teams
    limits: {credits_usd: 10, quota_usd: {monthly_usd: 60, rolling_5h_pct: 20, weekly_pct: 50, models: {glm-one: 0.000001}}}
  nogo:
    plan: none
    limits: {credits_usd: 10}
  tiny:
    limits: {credits_usd: 10, quota_usd: {monthly_usd: 0.000001}}
  budget:
    limits: {credits_usd: 10, spend_monthly_usd: 0.000001}
  mbudget:
    limits: {credits_usd: 10, spend_monthly_models_usd: {opencode/glm-one: 0.000001}}
accounts:
  - {id: acme, tier: go}
  - {id: plusco, tier: plus}
  - {id: team, tier: teams}
  - {id: nogo, tier: nogo}
  - {id: tiny-strict, tier: tiny}
  - {id: tiny-extra, tier: tiny, extra_usage: true}
  - {id: budget, tier: budget}
  - {id: mbudget, tier: mbudget}
keys:
  - {key: oc_sk_go, label: go-key, account: acme}
  - {key: oc_sk_plus, label: plus-key, account: plusco}
  - {key: oc_sk_member, label: member-key, account: team, project: alice}
  - {key: oc_sk_nogo, label: nogo-key, account: nogo}
  - {key: oc_sk_svc, label: svc-key, account: acme, service_account: true}
  - {key: oc_sk_tiny, label: tiny-key, account: tiny-strict}
  - {key: oc_sk_tinyx, label: tinyx-key, account: tiny-extra}
  - {key: oc_sk_budget, label: budget-key, account: budget}
  - {key: oc_sk_mbudget, label: mbudget-key, account: mbudget}
  - {key: oc_sk_susp, label: susp-key, account: acme, status: suspended}
models:
  - id: big-pickle
    protocols: [openai]
    drivers: [opencode]
    latency: fast
    generation: fixed
    pricing: {input_per_m: 0, output_per_m: 0}
    catalogs: {opencode: {}}
    capabilities: {reasoning: false}
  - id: beam-free
    protocols: [openai]
    drivers: [opencode]
    latency: fast
    generation: fixed
    pricing: {input_per_m: 0, output_per_m: 0}
    catalogs: {opencode-go: {}}
    capabilities: {reasoning: false}
  - id: deepseek-v4.1-flash
    protocols: [openai]
    drivers: [opencode]
    latency: fast
    generation: fixed
    pricing: {input_per_m: 0.3, output_per_m: 1.2}
    catalogs: {opencode: {}, opencode-go: {pricing: {input_per_m: 0.15, output_per_m: 0.6}}}
    capabilities: {reasoning: false}
  - id: gemini-4-argon
    protocols: [openai]
    drivers: [opencode]
    latency: fast
    generation: fixed
    pricing: {input_per_m: 1, output_per_m: 2}
    capabilities: {reasoning: false}
  - id: glm-one
    protocols: [openai]
    drivers: [opencode]
    latency: fast
    generation: fixed
    pricing: {input_per_m: 1, output_per_m: 2}
    catalogs: {opencode: {}, opencode-go: {allowance_factor: 1}}
    capabilities: {reasoning: false}
`

// billEngine is an engine over billCfg and an opencode driver with opts.
type billEngine struct {
	e   *engine.Engine
	drv driver.Driver
}

func newBillEngine(t *testing.T, cfgYAML string, opts map[string]string) *billEngine {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), log)
	drv, err := driver.New("opencode", opts)
	if err != nil {
		t.Fatal(err)
	}
	return &billEngine{e: e, drv: drv}
}

// do sends one request as the server hands it over: lower-case headers
// and x-wwz-path, the path relative to the mount prefix.
func (b *billEngine) do(t *testing.T, method, path string, hdr map[string]string, body string) *core.MemWriter {
	t.Helper()
	var kind driver.Kind
	for _, rt := range b.drv.Routes() {
		if rt.Method == method && (rt.Path == path || strings.HasSuffix(rt.Path, "/*") && strings.HasPrefix(path, strings.TrimSuffix(rt.Path, "*"))) {
			kind = rt.Kind
			break
		}
	}
	if kind == "" {
		t.Fatalf("no route for %s %s", method, path)
	}
	h := driver.MapHeaders{"x-wwz-path": path}
	for k, v := range hdr {
		h[strings.ToLower(k)] = v
	}
	w := core.NewMemWriter()
	b.e.Handle(context.Background(), engine.Input{Driver: b.drv, Kind: kind, Method: method, Path: path, Headers: h, Body: []byte(body), ClientIP: "203.0.113.7"}, w)
	return w
}

// chat posts a Chat Completions body for model with key (a bearer; ""
// sends none) on base, from the OpenCode client when oc is set.
func (b *billEngine) chat(t *testing.T, base, key, model string, oc bool) *core.MemWriter {
	t.Helper()
	hdr := map[string]string{"x-opencode-session": "ses_1"}
	if key != "" {
		hdr["authorization"] = "Bearer " + key
	}
	if oc {
		hdr["user-agent"] = ocClient
	}
	return b.do(t, "POST", base+"/openai/v1/chat/completions", hdr, `{"model":"`+model+`","messages":[{"role":"user","content":"oi"}]}`)
}

// freeFixture is a recording of testdata/real/opencode/free/ (task 416).
type freeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    *string           `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

func loadFree(t *testing.T, name string) freeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "free", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx freeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

// checkRetryAfter: retry-after runs to the next 00:00 UTC, in seconds.
func checkRetryAfter(t *testing.T, name string, got map[string]string) {
	t.Helper()
	v := ""
	for k, x := range got {
		if strings.EqualFold(k, "retry-after") {
			v = x
		}
	}
	n, err := strconv.Atoi(v)
	now := time.Now().UTC()
	want := int(math.Ceil(time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()))
	if err != nil || n < want-2 || n > want+2 {
		t.Errorf("%s: retry-after %q, want ~%d (to 00:00 UTC)", name, v, want)
	}
}

// TestRealFreeFixtures: the recordings of task 416 (generic client, no
// User-Agent opencode/) give the same status, body and headers through
// the engine.
func TestRealFreeFixtures(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	for _, name := range []string{"public_generic_client", "public_messages", "public_paid_model", "public_go_base", "messages_x_api_key", "public_usage"} {
		fx := loadFree(t, name)
		body := ""
		if fx.Request.Body != nil {
			body = *fx.Request.Body
		}
		w := b.do(t, fx.Request.Method, fx.Request.Path, fx.Request.Headers, body)
		if w.StatusCode() != fx.Response.Status || string(w.Body()) != fx.Response.Body {
			t.Errorf("%s: %d %s, real %d %s", name, w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
		}
		checkHeaders(t, shapeFixture{Name: name, Response: struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}{Headers: withoutRetryAfter(fx.Response.Headers)}}, w.Headers())
		if _, ok := fx.Response.Headers["retry-after"]; ok {
			checkRetryAfter(t, name, w.Headers())
		}
	}
}

func withoutRetryAfter(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if k != "retry-after" {
			out[k] = v
		}
	}
	return out
}

// TestRefusalOrder: session → body → model → credential → client → daily
// count (ADR 0072, item 5).
func TestRefusalOrder(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	const missingSession = "MissingSessionID"
	cases := []struct {
		name, path string
		hdr        map[string]string
		body       string
		status     int
		want       string
	}{
		{"session before public", "/inference/go/openai/v1/chat/completions", map[string]string{"authorization": "Bearer public"}, `{"model":"beam-free","messages":[{"role":"user","content":"oi"}]}`, 400, missingSession},
		{"body before public", "/inference/openai/v1/chat/completions", map[string]string{"authorization": "Bearer public", "x-opencode-session": "s"}, `{`, 400, ""},
		{"model before public", "/inference/openai/v1/chat/completions", map[string]string{"authorization": "Bearer public", "x-opencode-session": "s"}, `{"model":"nope","messages":[{"role":"user","content":"oi"}]}`, 400, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`},
		{"model out of the base's catalog", "/inference/go/openai/v1/chat/completions", map[string]string{"authorization": "Bearer oc_sk_go", "x-opencode-session": "s"}, `{"model":"big-pickle","messages":[{"role":"user","content":"oi"}]}`, 400, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`},
		{"credential before client", "/inference/go/openai/v1/chat/completions", map[string]string{"authorization": "Bearer public", "x-opencode-session": "s"}, `{"model":"beam-free","messages":[{"role":"user","content":"oi"}]}`, 401, `{"error":{"type":"server_error","message":"Upstream request failed: Go requires a credential"}}`},
		{"plan before client", "/inference/go/openai/v1/chat/completions", map[string]string{"authorization": "Bearer oc_sk_nogo", "x-opencode-session": "s"}, `{"model":"beam-free","messages":[{"role":"user","content":"oi"}]}`, 403, `{"type":"error","error":{"type":"ForbiddenError","message":"This workspace does not have an active OpenCode Go subscription"}}`},
		{"client with a key on the Go base", "/inference/go/openai/v1/chat/completions", map[string]string{"authorization": "Bearer oc_sk_go", "x-opencode-session": "s"}, `{"model":"beam-free","messages":[{"role":"user","content":"oi"}]}`, 429, `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`},
		{"public on Messages of the Go base", "/inference/go/anthropic/v1/messages", map[string]string{"x-api-key": "public", "x-opencode-session": "s"}, `{"model":"deepseek-v4.1-flash","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}`, 401, `{"type":"error","error":{"type":"api_error","message":"Upstream request failed: Go requires a credential"}}`},
		{"no credential on a pay-as-you-go base", "/zen/openai/v1/chat/completions", map[string]string{"x-opencode-session": "s"}, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"oi"}]}`, 401, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`},
		{"no credential on Messages of a pay-as-you-go base", "/inference/anthropic/v1/messages", map[string]string{"x-opencode-session": "s"}, `{"model":"deepseek-v4.1-flash","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}`, 401, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`},
		{"no credential on a Go base", "/zen/go/openai/v1/chat/completions", map[string]string{"x-opencode-session": "s"}, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"oi"}]}`, 401, `{"error":{"type":"server_error","message":"Upstream request failed: Go requires a credential"}}`},
		{"suspended workspace", "/inference/openai/v1/chat/completions", map[string]string{"authorization": "Bearer oc_sk_susp", "x-opencode-session": "s"}, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"oi"}]}`, 403, `{"error":{"type":"permission_error","message":"This workspace has been suspended"}}`},
	}
	for _, c := range cases {
		w := b.do(t, "POST", c.path, c.hdr, c.body)
		got := string(w.Body())
		if w.StatusCode() != c.status || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %d %s, want %d %s", c.name, w.StatusCode(), got, c.status, c.want)
		}
	}
	// Google route: the FreeUsageLimitError in Google's envelope.
	w := b.do(t, "POST", "/inference/google/v1beta/models/big-pickle:generateContent", map[string]string{"x-goog-api-key": "public", "x-opencode-session": "s"}, `{"contents":[{"role":"user","parts":[{"text":"oi"}]}]}`)
	if w.StatusCode() != 429 || !strings.Contains(string(w.Body()), `"status":"RESOURCE_EXHAUSTED"`) || !strings.Contains(string(w.Body()), `"reason":"FreeUsageLimitError"`) {
		t.Errorf("google: %d %s", w.StatusCode(), w.Body())
	}
}

// usageOfBody is usage.cost and usage.opencode of a 200.
type ocUsage struct {
	Cost     float64 `json:"cost"`
	OpenCode struct {
		BillingSource string   `json:"billing_source"`
		ListCost      float64  `json:"list_cost"`
		Allowance     *float64 `json:"allowance"`
	} `json:"opencode"`
}

func usageFrom(t *testing.T, w *core.MemWriter) ocUsage {
	t.Helper()
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	var out struct {
		Usage ocUsage `json:"usage"`
	}
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatalf("%s: %v", w.Body(), err)
	}
	return out.Usage
}

// TestBillingSource: billing_source, cost and allowance follow the
// request's billing (free, balance, go, go-plus), not the path.
func TestBillingSource(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	cases := []struct {
		name, base, key, model string
		source                 string
		cost                   bool    // cost == list_cost (else 0)
		factor                 float64 // allowance / list_cost; 0 = no allowance
	}{
		{"public free", "/inference", "public", "big-pickle", "free", false, 0},
		{"key free on pay-as-you-go", "/zen", "oc_sk_go", "big-pickle", "free", false, 0},
		{"free on Go", "/inference/go", "oc_sk_go", "beam-free", "free", false, 0},
		{"pay-as-you-go", "/inference", "oc_sk_go", "deepseek-v4.1-flash", "balance", true, 0},
		{"Go", "/inference/go", "oc_sk_go", "deepseek-v4.1-flash", "go", false, 0.6},
		{"Go, native Gemini", "/zen/go", "oc_sk_go", "gemini-4-argon", "go", false, 2},
		{"Go, catalog allowance_factor", "/inference/go", "oc_sk_go", "glm-one", "go", false, 1},
		{"Go Plus", "/inference/go", "oc_sk_plus", "deepseek-v4.1-flash", "go-plus", false, 0.6},
		{"Go Teams", "/inference/go", "oc_sk_member", "deepseek-v4.1-flash", "go", false, 0.6},
		{"service account on Go", "/inference/go", "oc_sk_svc", "deepseek-v4.1-flash", "balance", true, 0},
		{"plan none on pay-as-you-go", "/inference", "oc_sk_nogo", "deepseek-v4.1-flash", "balance", true, 0},
	}
	for _, c := range cases {
		u := usageFrom(t, b.chat(t, c.base, c.key, c.model, true))
		if u.OpenCode.BillingSource != c.source {
			t.Errorf("%s: billing_source %q, want %q", c.name, u.OpenCode.BillingSource, c.source)
		}
		if wantCost := map[bool]float64{true: u.OpenCode.ListCost, false: 0}[c.cost]; !near(u.Cost, wantCost) {
			t.Errorf("%s: cost %v, want %v (list %v)", c.name, u.Cost, wantCost, u.OpenCode.ListCost)
		}
		switch {
		case c.factor == 0 && u.OpenCode.Allowance != nil:
			t.Errorf("%s: allowance %v outside go/go-plus", c.name, *u.OpenCode.Allowance)
		case c.factor > 0 && (u.OpenCode.Allowance == nil || !near(*u.OpenCode.Allowance, math.Round(u.OpenCode.ListCost*c.factor*1e8)/1e8)):
			t.Errorf("%s: allowance %v, want list %v × %v", c.name, u.OpenCode.Allowance, u.OpenCode.ListCost, c.factor)
		}
	}
	// The Go catalog's price: deepseek-v4.1-flash costs half on the Go base.
	payg := usageFrom(t, b.chat(t, "/inference", "oc_sk_go", "deepseek-v4.1-flash", true)).OpenCode.ListCost
	gob := usageFrom(t, b.chat(t, "/inference/go", "oc_sk_go", "deepseek-v4.1-flash", true)).OpenCode.ListCost
	if !near(payg, 2*gob) {
		t.Errorf("list cost: pay-as-you-go %v, Go %v; want twice", payg, gob)
	}
}

// TestFreeDailyCount: free_requests_per_day counts the free models of the
// pay-as-you-go bases per IP (no key) or account; the Go bases do not
// count them. Past it, the 429 FreeUsageLimitError to 00:00 UTC.
func TestFreeDailyCount(t *testing.T) {
	b := newBillEngine(t, billCfg, map[string]string{"free_requests_per_day": "2"})
	for i := range 2 {
		if w := b.chat(t, "/inference", "public", "big-pickle", true); w.StatusCode() != 200 {
			t.Fatalf("public %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	w := b.chat(t, "/zen", "public", "big-pickle", true)
	if w.StatusCode() != 429 || string(w.Body()) != loadFree(t, "public_generic_client").Response.Body {
		t.Fatalf("third: %d %s", w.StatusCode(), w.Body())
	}
	checkRetryAfter(t, "daily count", w.Headers())
	// The account counts apart from the IP; the Go base never counts.
	for i := range 2 {
		if w := b.chat(t, "/inference", "oc_sk_go", "big-pickle", true); w.StatusCode() != 200 {
			t.Fatalf("key %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	if w := b.chat(t, "/inference", "oc_sk_go", "big-pickle", true); w.StatusCode() != 429 {
		t.Fatalf("key third: %d %s", w.StatusCode(), w.Body())
	}
	for i := range 3 {
		if w := b.chat(t, "/inference/go", "oc_sk_go", "beam-free", true); w.StatusCode() != 200 {
			t.Fatalf("Go free %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	// Messages: the gateway's envelope, not api_error.
	w = b.do(t, "POST", "/inference/anthropic/v1/messages", map[string]string{"x-api-key": "public", "x-opencode-session": "s", "user-agent": ocClient}, `{"model":"big-pickle","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}`)
	if w.StatusCode() != 429 || string(w.Body()) != loadFree(t, "public_messages").Response.Body {
		t.Fatalf("messages: %d %s", w.StatusCode(), w.Body())
	}
	if _, err := driver.New("opencode", map[string]string{"free_requests_per_day": "0"}); err == nil {
		t.Error("free_requests_per_day 0 accepted")
	}
}

// reWorkspace is metadata.workspace of GoUsageLimitError.
var reWorkspace = regexp.MustCompile(`"workspace":"wrk_[0-9A-HJKMNP-TV-Z]{26}"`)

// TestQuotaAndBudgetErrors: GoUsageLimitError with the account's
// workspace (from the quota and from a model quota), Extra Usage to the
// balance, BudgetLimitError with the coverage of the budget and resetsAt
// on the 1st, 00:00 UTC.
func TestQuotaAndBudgetErrors(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	_ = b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true)
	w := b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true)
	if w.StatusCode() != 429 || !strings.Contains(string(w.Body()), `"type":"GoUsageLimitError"`) || !reWorkspace.Match(w.Body()) || !strings.Contains(string(w.Body()), `"limitName":"monthly"`) {
		t.Fatalf("quota: %d %s", w.StatusCode(), w.Body())
	}
	ws := reWorkspace.Find(w.Body())
	// Extra Usage: the same exhausted quota goes on as balance.
	_ = b.chat(t, "/inference/go", "oc_sk_tinyx", "deepseek-v4.1-flash", true)
	if u := usageFrom(t, b.chat(t, "/inference/go", "oc_sk_tinyx", "deepseek-v4.1-flash", true)); u.OpenCode.BillingSource != "balance" || u.Cost == 0 || u.OpenCode.Allowance != nil {
		t.Errorf("extra usage: %+v", u)
	}
	if bytesOf := reWorkspace.Find(b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true).Body()); string(bytesOf) != string(ws) {
		t.Errorf("workspace not stable: %s, %s", bytesOf, ws)
	}
	// A model quota of the member (Go Teams, Extra Usage on by default):
	// the first request fills it, the second goes on as balance.
	_ = b.chat(t, "/inference/go", "oc_sk_member", "glm-one", true)
	if u := usageFrom(t, b.chat(t, "/inference/go", "oc_sk_member", "glm-one", true)); u.OpenCode.BillingSource != "balance" {
		t.Errorf("model quota with extra usage: %+v", u)
	}
	next := func() string {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	for _, c := range []struct{ key, model, coverage string }{
		{"oc_sk_budget", "deepseek-v4.1-flash", "all"},
		{"oc_sk_mbudget", "glm-one", "models"},
	} {
		_ = b.chat(t, "/inference", c.key, c.model, true)
		w := b.chat(t, "/inference", c.key, c.model, true)
		want := `"metadata":{"coverage":"` + c.coverage + `","resetsAt":"` + next() + `"}`
		if w.StatusCode() != 429 || !strings.Contains(string(w.Body()), `"type":"BudgetLimitError"`) || !strings.Contains(string(w.Body()), want) {
			t.Errorf("%s: %d %s, want %s", c.key, w.StatusCode(), w.Body(), want)
		}
	}
	// A budget never counts the Go base inside the quota.
	if w := b.chat(t, "/inference/go", "oc_sk_budget", "deepseek-v4.1-flash", true); w.StatusCode() != 200 {
		t.Errorf("budget on Go: %d %s", w.StatusCode(), w.Body())
	}
}

// TestUsageRoute: public → 401 AuthError; plan none → 403
// ForbiddenError; the windows of the quota owner (the member in Go Teams),
// the largest of the whole quota and the model quotas, rate-limited when
// the window denies.
func TestUsageRoute(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	get := func(key string) *core.MemWriter {
		return b.do(t, "GET", "/zen/go/v1/usage", map[string]string{"authorization": "Bearer " + key}, "")
	}
	if w := get("public"); w.StatusCode() != 401 || string(w.Body()) != loadFree(t, "public_usage").Response.Body {
		t.Errorf("public: %d %s", w.StatusCode(), w.Body())
	}
	if w := get("oc_sk_nogo"); w.StatusCode() != 403 || string(w.Body()) != `{"type":"error","error":{"type":"ForbiddenError","message":"This workspace does not have an active OpenCode Go subscription"}}` {
		t.Errorf("plan none: %d %s", w.StatusCode(), w.Body())
	}
	type win struct {
		Status  string `json:"status"`
		Percent int    `json:"percent"`
	}
	read := func(key string) map[string]win {
		w := get(key)
		var out struct {
			Usage map[string]win `json:"usage"`
		}
		if w.StatusCode() != 200 || json.Unmarshal(w.Body(), &out) != nil {
			t.Fatalf("%s: %d %s", key, w.StatusCode(), w.Body())
		}
		return out.Usage
	}
	for name, w := range read("oc_sk_member") {
		if w.Status != "ok" || w.Percent != 0 {
			t.Errorf("fresh member %s: %+v", name, w)
		}
	}
	// The member's glm-one quota is used up by one request: every window
	// shows it at 100 % and rate-limited.
	_ = b.chat(t, "/inference/go", "oc_sk_member", "glm-one", true)
	for name, w := range read("oc_sk_member") {
		if w.Status != "rate-limited" || w.Percent != 100 {
			t.Errorf("member after the model quota %s: %+v", name, w)
		}
	}
	// The quota of tiny-strict, used up: monthly rate-limited; the 5 h and
	// weekly windows have no limit there.
	_ = b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true)
	u := read("oc_sk_tiny")
	if u["monthly"].Status != "rate-limited" || u["monthly"].Percent != 100 || u["rolling"].Status != "ok" || u["weekly"].Status != "ok" {
		t.Errorf("tiny: %+v", u)
	}
}
