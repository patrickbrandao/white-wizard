package engine

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Billing origin and subjects (task 421; ADR 0072, items 4, 6, 7 and
// 10–14; spec 010 "Origem da cobrança no opencode"; spec 005 interface (d)).

// billDriverName is the fake driver with interface (d): routes under /go
// serve the "sub" catalog (the subscription base), the others "paygo".
const billDriverName = "wwz-billing-test"

var registerBillDriver sync.Once

// billAllowance is the fake driver's allowance factor.
const billAllowance = 0.5

// billingDriver is OpenAI's driver under another name with interfaces (c)
// and (d), a toy of OpenCode's rules: "public" is anonymous; on the
// subscription base public is refused (401), plan none is refused (403),
// a free model is free without a daily count, a service account pays from
// the balance and the others are go (go-plus on that plan, the member's
// quota on go-teams); on the pay-as-you-go base a free model outside the
// "opencode/" client is refused (429), a free one is free with a daily
// count of 2, and the rest (public refused, 401) is balance. It reports
// the billing and the denial in headers.
type billingDriver struct{ driver.Driver }

func (billingDriver) Name() string     { return billDriverName }
func (billingDriver) Protocol() string { return billDriverName }

func (billingDriver) CatalogOf(r driver.Route) string {
	if strings.HasPrefix(r.Path, "/go/") {
		return "sub"
	}
	return "paygo"
}

func (d billingDriver) Authenticate(h driver.Headers) driver.Credential {
	c := d.Driver.Authenticate(h)
	c.Anonymous = c.Key == "public"
	return c
}

func (billingDriver) Billing(q driver.BillingQuery) (driver.Billing, *core.Error) {
	b := driver.Billing{AllowanceFactor: billAllowance}
	if q.Plan == "go-teams" {
		b.QuotaOwner = driver.OwnerMember
	}
	b.ExtraUsage = q.Plan == "go-teams"
	if q.Account != nil && q.Account.ExtraUsage != nil {
		b.ExtraUsage = *q.Account.ExtraUsage
	}
	if q.Model == nil {
		return b, nil
	}
	free := q.Model.PricingFor(q.Catalog).Free()
	if q.Catalog == "sub" {
		switch {
		case q.Key == nil:
			return b, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "Go requires a credential"}
		case q.Plan == "none":
			return b, &core.Error{Kind: core.ErrPermission, Message: "no Go"}
		case free:
			b.Source = driver.BillingFree
		case q.Key.ServiceAccount:
			b.Source = driver.BillingBalance
		case q.Plan == "go-plus":
			b.Source = driver.BillingGoPlus
		default:
			b.Source = driver.BillingGo
		}
		return b, nil
	}
	switch {
	case free && !strings.HasPrefix(q.Request.Header("user-agent"), "opencode/"):
		return b, &core.Error{Kind: core.ErrRateLimit, Message: "Free usage limit reached."}
	case free:
		b.Source, b.FreeDailyRequests = driver.BillingFree, 2
	case q.Key == nil:
		return b, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "Missing API key."}
	default:
		b.Source = driver.BillingBalance
	}
	return b, nil
}

func (d billingDriver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if b, ok := driver.BillingOf(req); ok {
		w.Header("x-test-billing", b.Source)
	}
	return d.Driver.WriteResponse(w, req, resp, rl)
}

func (d billingDriver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if e.Extra != nil {
		w.Header("x-test-limit", anyString(e.Extra["limit"]))
		w.Header("x-test-entity", anyString(e.Extra["entity_kind"]))
	}
	if b, ok := driver.BillingOf(req); ok {
		w.Header("x-test-billing", b.Source)
	}
	d.Driver.WriteError(w, req, e, rl)
}

// Extra answers the usage route with the billing view of the context.
func (billingDriver) Extra(_ driver.Kind, w core.ResponseWriter, _ driver.Headers, _ []byte, xc driver.ExtraContext) {
	w.Status(200)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"account": xc.AccountID, "plan": xc.Plan, "service_account": xc.ServiceAccount,
		"owner": xc.QuotaOwner, "model_quotas": xc.ModelQuotas, "budgets": xc.Budgets,
		"account_balance": xc.AccountBalance,
	})
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}

func newBillingDriver(t *testing.T) driver.Driver {
	t.Helper()
	registerBillDriver.Do(func() {
		config.RegisterDriver(billDriverName, func() (config.DriverDecl, bool) {
			return config.DriverDecl{
				Dialects: []string{config.DialectOpenAI}, AllRoutes: true,
				Catalogs: []config.CatalogDecl{{Name: "paygo"}, {Name: "sub", Allowance: true}},
				Plans:    []string{"go", "go-plus", "go-teams", "none"},
			}, true
		})
	})
	inner, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	return billingDriver{inner}
}

// billCall sends a chat request for model with the key on path.
func billCall(e *Engine, drv driver.Driver, key, ua, path, model string) *core.MemWriter {
	w := core.NewMemWriter()
	h := driver.MapHeaders{"authorization": "Bearer " + key}
	if ua != "" {
		h["user-agent"] = ua
	}
	e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindChat, Method: "POST", Path: path, Headers: h, Body: []byte(chatOf(model)), ClientIP: "10.0.0.7"}, w)
	return w
}

const (
	paygoPath = "/v1/chat/completions"
	goPath    = "/go/v1/chat/completions"
	clientUA  = "opencode/latest/2.2.5/cli"
)

// billingCfg prices "paid" at 1000/2000 USD per million tokens, so a
// request costs whole cents; "paid2" is the same model under another id;
// "gratis" is free.
const billingCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
tiers:
  go:
    limits:
      credits_usd: 10
      spend_monthly_usd: 100
      quota_usd: {monthly_usd: 100}
  plus:
    plan: go-plus
    limits:
      credits_usd: 10
      quota_usd: {monthly_usd: 100}
  teams:
    plan: go-teams
    limits:
      credits_usd: 10
      quota_usd: {monthly_usd: 100}
  nogo:
    plan: none
    limits: {credits_usd: 10}
  tiny:
    limits:
      rpm: 3
      credits_usd: 10
      quota_usd: {monthly_usd: 0.000001}
  tinybroke:
    limits:
      credits_usd: 0
      quota_usd: {monthly_usd: 0.000001}
  permodel:
    limits:
      rpm: 2
      credits_usd: 10
      quota_usd: {monthly_usd: 100, models: {paid: 0.000001}}
accounts:
  - {id: acme, tier: go}
  - {id: plusco, tier: plus}
  - {id: team, tier: teams}
  - {id: nogo, tier: nogo}
  - {id: tiny-extra, tier: tiny, extra_usage: true}
  - {id: tiny-strict, tier: tiny}
  - {id: tiny-broke, tier: tinybroke, extra_usage: true}
  - {id: permodel, tier: permodel}
keys:
  - {key: k-acme, label: acme-key, account: acme}
  - {key: k-plus, label: plus-key, account: plusco}
  - {key: k-member, label: member-key, account: team, project: alice}
  - {key: k-member-solo, label: solo-key, account: team}
  - {key: k-svc, label: svc-key, account: acme, service_account: true}
  - {key: k-nogo, label: nogo-key, account: nogo}
  - {key: k-tiny-extra, label: tiny-extra-key, account: tiny-extra}
  - {key: k-tiny-strict, label: tiny-strict-key, account: tiny-strict}
  - {key: k-tiny-broke, label: tiny-broke-key, account: tiny-broke}
  - {key: k-permodel, label: permodel-key, account: permodel}
  - key: k-budget
    label: budget-key
    account: permodel
    limits:
      spend_monthly_models_usd: {paygo/paid: 0.000001}
models:
  - id: paid
    drivers: [wwz-billing-test]
    latency: fast
    pricing: {input_per_m: 1000, output_per_m: 2000}
  - id: paid2
    drivers: [wwz-billing-test]
    latency: fast
    pricing: {input_per_m: 1000, output_per_m: 2000}
  - id: gratis
    drivers: [wwz-billing-test]
    latency: fast
    pricing: {input_per_m: 0, output_per_m: 0}
`

// listCost is the list cost of a served chat reply of "paid", in USD.
func listCost(t *testing.T, w *core.MemWriter) float64 {
	t.Helper()
	p, c := usageOf(t, w)
	return float64(pico(p, c, 1000, 2000)) / 1e12
}

func stateOf(t *testing.T, e *Engine, kind limits.EntityKind, id string) limits.EntityState {
	t.Helper()
	st, err := e.Store.Get(context.Background(), limits.Entity{Kind: kind, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func credits(st limits.EntityState) float64 {
	if st.Credits == nil {
		return math.NaN()
	}
	return *st.Credits
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// lastEvent is the newest event of the store.
func lastEvent(t *testing.T, e *Engine) limits.Event {
	t.Helper()
	evs, err := e.Store.Events(context.Background(), 1)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events: %v %v", evs, err)
	}
	return evs[0]
}

func mustStatus(t *testing.T, w *core.MemWriter, status int, what string) {
	t.Helper()
	if w.StatusCode() != status {
		t.Fatalf("%s: %d %s %v, want %d", what, w.StatusCode(), w.Body(), w.Headers(), status)
	}
}

func TestBillingGoDebitsOnlyTheQuotaByTheAllowance(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	for _, c := range []struct{ key, acct, src string }{{"k-acme", "acme", "go"}, {"k-plus", "plusco", "go-plus"}} {
		w := billCall(e, drv, c.key, "", goPath, "paid")
		mustStatus(t, w, 200, c.key)
		if got := w.Headers()["x-test-billing"]; got != c.src {
			t.Errorf("%s: billing %q, want %q", c.key, got, c.src)
		}
		cost := listCost(t, w)
		st := stateOf(t, e, limits.KindAccount, c.acct)
		if !near(st.Quota5hUSD, cost*billAllowance) || st.SpendUSD != 0 || st.SpentTotalUSD != 0 {
			t.Errorf("%s: quota %v spend %v spent %v, want quota %v and no spend", c.key, st.Quota5hUSD, st.SpendUSD, st.SpentTotalUSD, cost*billAllowance)
		}
		if cr := credits(st); !math.IsNaN(cr) && cr != 10 {
			t.Errorf("%s: credits %v, want untouched", c.key, cr)
		}
		ev := lastEvent(t, e)
		if ev.Fields["billing_source"] != c.src || !near(anyFloat(ev.Fields["allowance_usd"]), cost*billAllowance) || anyFloat(ev.Fields["cost_usd"]) != 0 {
			t.Errorf("%s: event %v", c.key, ev.Fields)
		}
	}
}

func anyFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

func TestBillingPayAsYouGoDebitsOnlyTheBalance(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	w := billCall(e, drv, "k-acme", "", paygoPath, "paid")
	mustStatus(t, w, 200, "paygo")
	if got := w.Headers()["x-test-billing"]; got != "balance" {
		t.Errorf("billing %q, want balance", got)
	}
	cost := listCost(t, w)
	st := stateOf(t, e, limits.KindAccount, "acme")
	if st.Quota5hUSD != 0 || !near(st.SpendUSD, cost) || !near(credits(st), 10-cost) {
		t.Errorf("quota %v spend %v credits %v, want no quota, spend %v and credits %v", st.Quota5hUSD, st.SpendUSD, credits(st), cost, 10-cost)
	}
	if ev := lastEvent(t, e); ev.Fields["billing_source"] != "balance" || ev.Fields["allowance_usd"] != nil {
		t.Errorf("event %v", ev.Fields)
	}
}

func TestBillingFreeCountsDailyAndDebitsNothing(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)

	// public: the daily count is the IP's, the event key is "public".
	for i := range 2 {
		w := billCall(e, drv, "public", clientUA, paygoPath, "gratis")
		mustStatus(t, w, 200, "public free")
		if got := w.Headers()["x-test-billing"]; got != "free" {
			t.Errorf("request %d: billing %q, want free", i, got)
		}
	}
	ev := lastEvent(t, e)
	if ev.Key != "public" || ev.Account != "" || ev.Fields["billing_source"] != "free" {
		t.Errorf("public event: key %q account %q fields %v", ev.Key, ev.Account, ev.Fields)
	}
	w := billCall(e, drv, "public", clientUA, paygoPath, "gratis")
	mustStatus(t, w, 429, "third public free")
	if w.Headers()["x-test-limit"] != "rpd" || w.Headers()["x-test-entity"] != string(limits.KindFreeModels) {
		t.Errorf("third public free: headers %v", w.Headers())
	}
	if st := stateOf(t, e, limits.KindFreeModels, "ip:10.0.0.7"); st.Requests != 2 {
		t.Errorf("free_models ip:10.0.0.7: %d requests, want 2", st.Requests)
	}

	// With a key the count is the account's, apart from the IP's, and the
	// free model debits nothing.
	for range 2 {
		mustStatus(t, billCall(e, drv, "k-acme", clientUA, paygoPath, "gratis"), 200, "keyed free")
	}
	mustStatus(t, billCall(e, drv, "k-acme", clientUA, paygoPath, "gratis"), 429, "third keyed free")
	if st := stateOf(t, e, limits.KindFreeModels, "acme"); st.Requests != 2 {
		t.Errorf("free_models acme: %d requests, want 2", st.Requests)
	}
	if st := stateOf(t, e, limits.KindAccount, "acme"); st.SpendUSD != 0 || st.Quota5hUSD != 0 || !math.IsNaN(credits(st)) && credits(st) != 10 {
		t.Errorf("acme after free: spend %v quota %v credits %v", st.SpendUSD, st.Quota5hUSD, credits(st))
	}

	// On the subscription base a free model has no daily count.
	for range 3 {
		w := billCall(e, drv, "k-plus", "", goPath, "gratis")
		mustStatus(t, w, 200, "free on go")
		if w.Headers()["x-test-billing"] != "free" {
			t.Errorf("free on go: billing %q", w.Headers()["x-test-billing"])
		}
	}
	if st := stateOf(t, e, limits.KindAccount, "plusco"); st.Quota5hUSD != 0 {
		t.Errorf("free on go debited the quota: %v", st.Quota5hUSD)
	}
}

func TestBillingRefusalBeforeCheckCountsNothing(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	for _, c := range []struct {
		key, ua, path, model string
		status               int
	}{
		{"public", "curl/8.7.1", paygoPath, "gratis", 429}, // free outside the client
		{"k-acme", "curl/8.7.1", paygoPath, "gratis", 429},
		{"public", clientUA, paygoPath, "paid", 401}, // public with a paid model
		{"public", clientUA, goPath, "gratis", 401},  // public on the Go base
		{"k-nogo", "", goPath, "paid", 403},          // plan none
	} {
		w := billCall(e, drv, c.key, c.ua, c.path, c.model)
		mustStatus(t, w, c.status, c.key+" "+c.path+" "+c.model)
		if w.Headers()["x-test-billing"] != "" {
			t.Errorf("%s %s: a refusal carries the billing %q", c.key, c.model, w.Headers()["x-test-billing"])
		}
	}
	if evs, _ := e.Store.Events(context.Background(), 10); len(evs) != 0 {
		t.Errorf("refusals reached the store: %v", evs)
	}
	for _, ent := range []limits.Entity{{Kind: limits.KindIP, ID: "10.0.0.7"}, {Kind: limits.KindFreeModels, ID: "ip:10.0.0.7"}, {Kind: limits.KindFreeModels, ID: "acme"}, {Kind: limits.KindAccount, ID: "acme"}, {Kind: limits.KindAccount, ID: "nogo"}} {
		if st := stateOf(t, e, ent.Kind, ent.ID); st.Requests != 0 {
			t.Errorf("%v: %d requests after refusals", ent, st.Requests)
		}
	}
	// The daily count is whole: both free requests of the client pass.
	for range 2 {
		mustStatus(t, billCall(e, drv, "public", clientUA, paygoPath, "gratis"), 200, "free after refusals")
	}
}

func TestBillingGoTeamsCountsOnTheMember(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	w := billCall(e, drv, "k-member", "", goPath, "paid")
	mustStatus(t, w, 200, "member with project")
	cost := listCost(t, w)
	if st := stateOf(t, e, limits.KindProject, "alice"); !near(st.Quota5hUSD, cost*billAllowance) {
		t.Errorf("member alice: quota %v, want %v", st.Quota5hUSD, cost*billAllowance)
	}
	w = billCall(e, drv, "k-member-solo", "", goPath, "paid")
	mustStatus(t, w, 200, "member without project")
	if st := stateOf(t, e, limits.KindKey, "solo-key"); !near(st.Quota5hUSD, listCost(t, w)*billAllowance) {
		t.Errorf("member solo-key: quota %v", st.Quota5hUSD)
	}
	if st := stateOf(t, e, limits.KindAccount, "team"); st.Quota5hUSD != 0 || st.SpendUSD != 0 {
		t.Errorf("team account: quota %v spend %v, want none", st.Quota5hUSD, st.SpendUSD)
	}
}

func TestBillingServiceAccountOnGoPaysFromTheBalance(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	w := billCall(e, drv, "k-svc", "", goPath, "paid")
	mustStatus(t, w, 200, "service account")
	if got := w.Headers()["x-test-billing"]; got != "balance" {
		t.Errorf("billing %q, want balance", got)
	}
	cost := listCost(t, w)
	if st := stateOf(t, e, limits.KindAccount, "acme"); st.Quota5hUSD != 0 || !near(st.SpendUSD, cost) || !near(credits(st), 10-cost) {
		t.Errorf("acme: quota %v spend %v credits %v", st.Quota5hUSD, st.SpendUSD, credits(st))
	}
}

func TestBillingExtraUsage(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	// The first request fits the tiny quota and overflows it.
	for _, k := range []string{"k-tiny-extra", "k-tiny-strict", "k-tiny-broke"} {
		mustStatus(t, billCall(e, drv, k, "", goPath, "paid"), 200, k+" first")
	}

	// Without Extra Usage the quota answers.
	w := billCall(e, drv, "k-tiny-strict", "", goPath, "paid")
	mustStatus(t, w, 429, "strict")
	if w.Headers()["x-test-limit"] != "quota_monthly" || w.Headers()["x-test-billing"] != "go" {
		t.Errorf("strict: headers %v", w.Headers())
	}

	// With it the request is billed to the balance.
	before := stateOf(t, e, limits.KindAccount, "tiny-extra")
	w = billCall(e, drv, "k-tiny-extra", "", goPath, "paid")
	mustStatus(t, w, 200, "extra usage")
	if got := w.Headers()["x-test-billing"]; got != "balance" {
		t.Errorf("extra usage: billing %q, want balance", got)
	}
	cost := listCost(t, w)
	after := stateOf(t, e, limits.KindAccount, "tiny-extra")
	if !near(after.SpendUSD, cost) || !near(credits(after), 10-cost) || after.Quota5hUSD != before.Quota5hUSD || after.Requests != before.Requests+1 {
		t.Errorf("extra usage: before %+v after %+v (cost %v)", before, after, cost)
	}
	if ev := lastEvent(t, e); ev.Fields["billing_source"] != "balance" {
		t.Errorf("extra usage event: %v", ev.Fields)
	}

	// With a zero balance the credits answer (the driver's 402).
	w = billCall(e, drv, "k-tiny-broke", "", goPath, "paid")
	if w.StatusCode() < 400 || w.Headers()["x-test-limit"] != "credits" || w.Headers()["x-test-billing"] != "balance" {
		t.Errorf("broke: %d %v", w.StatusCode(), w.Headers())
	}
	if ev := lastEvent(t, e); ev.Fields["denied"] != true || ev.Fields["limit"] != "credits" {
		t.Errorf("broke event: %v", ev.Fields)
	}
}

func TestBillingModelQuotaAndBudgetDenyAlone(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)

	// model_quota "permodel/paid" (rpm 2 on the account).
	mustStatus(t, billCall(e, drv, "k-permodel", "", goPath, "paid"), 200, "first paid")
	if st := stateOf(t, e, limits.KindModelQuota, "permodel/paid"); st.Quota5hUSD <= 0 {
		t.Errorf("model_quota permodel/paid: quota %v", st.Quota5hUSD)
	}
	w := billCall(e, drv, "k-permodel", "", goPath, "paid")
	mustStatus(t, w, 429, "second paid")
	if w.Headers()["x-test-limit"] != "quota_monthly" || w.Headers()["x-test-entity"] != string(limits.KindModelQuota) {
		t.Errorf("second paid: headers %v", w.Headers())
	}
	// The denial consumed nothing: the rpm still has room for paid2.
	mustStatus(t, billCall(e, drv, "k-permodel", "", goPath, "paid2"), 200, "paid2 after the model quota")
	if st := stateOf(t, e, limits.KindAccount, "permodel"); st.Requests != 2 {
		t.Errorf("permodel: %d requests, want 2", st.Requests)
	}

	// model_spend "key:budget-key/paid", only on the pay-as-you-go catalog.
	e = newPricingEngine(t, billingCfg)
	mustStatus(t, billCall(e, drv, "k-budget", "", paygoPath, "paid"), 200, "first budget")
	w = billCall(e, drv, "k-budget", "", paygoPath, "paid")
	mustStatus(t, w, 429, "second budget")
	if w.Headers()["x-test-limit"] != "spend_monthly" || w.Headers()["x-test-entity"] != string(limits.KindModelSpend) {
		t.Errorf("second budget: headers %v", w.Headers())
	}
	mustStatus(t, billCall(e, drv, "k-budget", "", paygoPath, "paid2"), 200, "paid2 after the budget")
	if st := stateOf(t, e, limits.KindAccount, "permodel"); st.Requests != 2 {
		t.Errorf("permodel: %d requests, want 2", st.Requests)
	}
	if st := stateOf(t, e, limits.KindModelSpend, "key:budget-key/paid"); st.SpendUSD <= 0 {
		t.Errorf("model_spend key:budget-key/paid: spend %v", st.SpendUSD)
	}
}

func TestBillingPublicWithAuthNone(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, "server: {auth: none}\n"+strings.TrimPrefix(billingCfg, "\nserver: {seed: 42}\n"))
	w := billCall(e, drv, "public", clientUA, paygoPath, "gratis")
	mustStatus(t, w, 200, "public, auth none")
	ev := lastEvent(t, e)
	if ev.Key != "public" || ev.Account != "" {
		t.Errorf("event key %q account %q", ev.Key, ev.Account)
	}
	mustStatus(t, billCall(e, drv, "public", clientUA, paygoPath, "paid"), 401, "public paid, auth none")
}

func TestBillingSubjectsByOrigin(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, billingCfg)
	cfg := e.Config()
	key := cfg.FindKey("k-acme")
	req := &core.Request{ClientIP: "10.0.0.7"}
	paid := cfg.FindModel("paid")
	kinds := func(subs []limits.Subject) map[limits.EntityKind]config.Limits {
		out := map[limits.EntityKind]config.Limits{}
		for _, s := range subs {
			out[s.Entity.Kind] = s.Limits
		}
		return out
	}

	got := kinds(e.billedSubjects(cfg, drv, key, req, paid, "paygo", &driver.Billing{Source: driver.BillingBalance}))
	if a := got[limits.KindAccount]; a.QuotaUSD != nil || a.CreditsUSD == nil || a.SpendMonthlyUSD != 100 {
		t.Errorf("balance account limits %+v", a)
	}
	got = kinds(e.billedSubjects(cfg, drv, key, req, paid, "sub", &driver.Billing{Source: driver.BillingGo}))
	if a := got[limits.KindAccount]; a.QuotaUSD == nil || a.CreditsUSD != nil || a.SpendMonthlyUSD != 0 {
		t.Errorf("go account limits %+v", a)
	}
	got = kinds(e.billedSubjects(cfg, drv, key, req, paid, "paygo", &driver.Billing{Source: driver.BillingFree}))
	if a := got[limits.KindAccount]; a.QuotaUSD != nil || a.CreditsUSD != nil || a.SpendMonthlyUSD != 0 {
		t.Errorf("free account limits %+v", a)
	}
	if _, ok := got[limits.KindFreeModels]; ok {
		t.Error("free without a daily count has a free_models subject")
	}
	// Without interface (d) every limit stays, as before.
	got = kinds(e.billedSubjects(cfg, drv, key, req, paid, "paygo", nil))
	if a := got[limits.KindAccount]; a.QuotaUSD == nil || a.CreditsUSD == nil || a.SpendMonthlyUSD != 100 {
		t.Errorf("no billing account limits %+v", a)
	}
	// public: only ip (and user) and free_models.
	subs := e.billedSubjects(cfg, drv, nil, req, cfg.FindModel("gratis"), "paygo", &driver.Billing{Source: driver.BillingFree, FreeDailyRequests: 5})
	if len(subs) != 2 || subs[0].Entity != (limits.Entity{Kind: limits.KindIP, ID: "10.0.0.7"}) || subs[1].Entity != (limits.Entity{Kind: limits.KindFreeModels, ID: "ip:10.0.0.7"}) || subs[1].Limits.RPD != 5 {
		t.Errorf("public subjects %+v", subs)
	}
}

func TestBillingViewOfTheUsageRoute(t *testing.T) {
	drv := newBillingDriver(t)
	cfg := strings.Replace(billingCfg, "keys:\n", `projects:
  - id: alice
    limits: {quota_usd: {monthly_usd: 50, rolling_5h_pct: 10, models: {paid: 5, sub/paid2: 7}}}
keys:
`, 1)
	cfg = strings.Replace(cfg, "  teams:\n    plan: go-teams\n    limits:\n", "  teams:\n    plan: go-teams\n    limits:\n      spend_monthly_usd: 30\n      spend_monthly_models_usd: {paid: 3}\n", 1)
	e := newPricingEngine(t, cfg)
	usage := func(key string) map[string]any {
		t.Helper()
		w := core.NewMemWriter()
		e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindUsage, Method: "GET", Path: "/go/v1/usage", Headers: driver.MapHeaders{"authorization": "Bearer " + key}}, w)
		var out map[string]any
		if err := json.Unmarshal(w.Body(), &out); err != nil {
			t.Fatalf("usage %s: %v %s", key, err, w.Body())
		}
		return out
	}

	got := usage("k-member")
	if got["account"] != "team" || got["plan"] != "go-teams" || got["service_account"] != false {
		t.Errorf("member usage header fields %v", got)
	}
	owner, _ := got["owner"].(map[string]any)
	if owner["Kind"] != "project" || owner["ID"] != "alice" {
		t.Errorf("member owner %v", owner)
	}
	if q, _ := owner["Quota"].(map[string]any); q["MonthlyUSD"] != 50.0 || q["Models"] != nil {
		t.Errorf("member owner quota %v", owner["Quota"])
	}
	mq, _ := got["model_quotas"].([]any)
	if len(mq) != 2 || mq[0].(map[string]any)["Model"] != "paid" || mq[1].(map[string]any)["Model"] != "sub/paid2" {
		t.Errorf("member model quotas %v", mq)
	}
	budgets, _ := got["budgets"].([]any)
	if len(budgets) != 2 || budgets[0].(map[string]any)["Model"] != "" || budgets[1].(map[string]any)["Model"] != "paid" || budgets[1].(map[string]any)["LimitUSD"] != 3.0 {
		t.Errorf("member budgets %v", budgets)
	}

	// The model quota of the view is the entity the requests debit.
	mustStatus(t, billCall(e, drv, "k-member", "", goPath, "paid2"), 200, "member paid2")
	got = usage("k-member")
	state, _ := got["model_quotas"].([]any)[1].(map[string]any)["State"].(map[string]any)
	if anyFloat(state["Quota5hUSD"]) <= 0 {
		t.Errorf("model quota sub/paid2 state %v", state)
	}

	if got := usage("k-svc"); got["service_account"] != true || got["plan"] != "" {
		t.Errorf("service account usage %v", got)
	}
}

// The usage route sees the quota debit of the week and of the month for
// the account and for the quota owner, apart from the spend, which a
// go/go-plus/go-teams request leaves at 0 (task 432).
func TestBillingViewQuotaWeeklyAndMonthly(t *testing.T) {
	drv := newBillingDriver(t)
	cfg := strings.Replace(billingCfg, "keys:\n", `projects:
  - id: alice
    limits: {quota_usd: {monthly_usd: 50}}
keys:
`, 1)
	e := newPricingEngine(t, cfg)
	usage := func(key string) map[string]any {
		t.Helper()
		w := core.NewMemWriter()
		e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindUsage, Method: "GET", Path: "/go/v1/usage", Headers: driver.MapHeaders{"authorization": "Bearer " + key}}, w)
		var out map[string]any
		if err := json.Unmarshal(w.Body(), &out); err != nil {
			t.Fatalf("usage %s: %v %s", key, err, w.Body())
		}
		return out
	}
	quotaOf := func(what string, b any, want float64) {
		t.Helper()
		m, _ := b.(map[string]any)
		if m == nil || !near(anyFloat(m["QuotaWeeklyUSD"]), want) || !near(anyFloat(m["QuotaMonthlyUSD"]), want) || anyFloat(m["SpendWeeklyUSD"]) != 0 || anyFloat(m["SpendUSD"]) != 0 {
			t.Errorf("%s: %v, want quota week and month %v and no spend", what, m, want)
		}
	}

	// go: the account is the quota owner.
	w := billCall(e, drv, "k-acme", "", goPath, "paid")
	mustStatus(t, w, 200, "acme")
	debit := listCost(t, w) * billAllowance
	got := usage("k-acme")
	quotaOf("acme account", got["account_balance"], debit)
	owner, _ := got["owner"].(map[string]any)
	if owner["Kind"] != "account" || owner["ID"] != "acme" {
		t.Fatalf("acme owner %v", owner)
	}
	quotaOf("acme owner", owner["State"], debit)

	// go-teams: the member owns the quota and the account sees none of it.
	w = billCall(e, drv, "k-member", "", goPath, "paid")
	mustStatus(t, w, 200, "member")
	debit = listCost(t, w) * billAllowance
	got = usage("k-member")
	quotaOf("team account", got["account_balance"], 0)
	owner, _ = got["owner"].(map[string]any)
	if owner["Kind"] != "project" || owner["ID"] != "alice" {
		t.Fatalf("member owner %v", owner)
	}
	quotaOf("member owner", owner["State"], debit)
}

func TestBillingSlotAndAllowance(t *testing.T) {
	b := driver.Billing{Source: driver.BillingGoPlus, AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember, ExtraUsage: true, FreeDailyRequests: 200}
	h := driver.MapHeaders{}
	driver.SetBilling(h, b)
	if got, ok := driver.BillingFrom(h); !ok || got != b {
		t.Errorf("BillingFrom: %+v %v, want %+v", got, ok, b)
	}
	if got, ok := driver.BillingOf(&core.Request{Headers: h}); !ok || got != b {
		t.Errorf("BillingOf: %+v %v", got, ok)
	}
	if _, ok := driver.BillingOf(nil); ok {
		t.Error("BillingOf(nil) found a billing")
	}
	if _, ok := driver.BillingFrom(driver.MapHeaders{}); ok {
		t.Error("BillingFrom of empty headers found a billing")
	}
	driver.SetBilling(nil, b) // no panic
	if got := b.Allowance(1000); got != 600 {
		t.Errorf("Allowance(1000) = %d, want 600", got)
	}
	if got := (driver.Billing{Source: driver.BillingBalance, AllowanceFactor: 1}).Allowance(1000); got != 0 {
		t.Errorf("balance allowance %d", got)
	}
}

// A driver without interface (d) keeps billing as before, and an Anonymous
// credential means nothing to it; spend_monthly_models_usd still applies.
func TestBillingWithoutInterfaceD(t *testing.T) {
	drv := anonOpenAI{newCatalogDriver(t)}
	e := newPricingEngine(t, `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
tiers:
  default:
    limits: {quota_usd: {monthly_usd: 100}, credits_usd: 10}
keys:
  - key: k-cat
    label: cat
    account: acct-cat
    limits: {spend_monthly_models_usd: {plain: 0.000001}}
models:
  - id: plain
    drivers: [wwz-catalog-test]
    latency: fast
    pricing: {input_per_m: 1000, output_per_m: 1000}
`)
	w := catCall(e, drv, driver.KindChat, "/v1/chat/completions", chatOf("plain"))
	mustStatus(t, w, 200, "first")
	st := stateOf(t, e, limits.KindAccount, "acct-cat")
	if st.SpendUSD <= 0 || st.Quota5hUSD != st.SpendUSD || credits(st) >= 10 {
		t.Errorf("no (d): account %+v, want cost on spend, credits and quota", st)
	}
	if ev := lastEvent(t, e); ev.Fields["billing_source"] != nil {
		t.Errorf("no (d): billing_source %v", ev.Fields["billing_source"])
	}
	w = catCall(e, drv, driver.KindChat, "/v1/chat/completions", chatOf("plain"))
	mustStatus(t, w, 429, "second, over the model budget")
	if st := stateOf(t, e, limits.KindModelSpend, "key:cat/plain"); st.SpendUSD <= 0 {
		t.Errorf("no (d): model_spend key:cat/plain spend %v", st.SpendUSD)
	}

	w = core.NewMemWriter()
	e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindChat, Method: "POST", Path: "/v1/chat/completions", Headers: driver.MapHeaders{"authorization": "Bearer public"}, Body: []byte(chatOf("plain")), ClientIP: "10.0.0.7"}, w)
	mustStatus(t, w, 401, "anonymous on a driver without (d)")
}

// anonOpenAI marks every credential Anonymous, without interface (d).
type anonOpenAI struct{ driver.Driver }

func (d anonOpenAI) Authenticate(h driver.Headers) driver.Credential {
	c := d.Driver.Authenticate(h)
	c.Anonymous = true
	return c
}

func (d anonOpenAI) Unwrap() driver.Driver { return d.Driver }
