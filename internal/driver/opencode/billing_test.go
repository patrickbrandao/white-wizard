package opencode_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 422, unit level: the catalogs of the bases, the billing decision
// (interface (d)), the credential and the declaration (ADR 0072).

func billing(t *testing.T) driver.BillingDecider {
	t.Helper()
	bd, ok := driver.As[driver.BillingDecider](newDriver(t))
	if !ok {
		t.Fatal("opencode has no BillingDecider")
	}
	return bd
}

// TestCatalogOf: /inference and /zen are opencode, their /go the
// opencode-go catalog; elsewhere none.
func TestCatalogOf(t *testing.T) {
	d := newDriver(t)
	for path, want := range map[string]string{
		"/inference/openai/v1/chat/completions":         "opencode",
		"/zen/v1/messages":                              "opencode",
		"/inference/go/google/v1beta/models/x:generate": "opencode-go",
		"/zen/go/v1/usage":                              "opencode-go",
		"/console/api/user":                             "",
	} {
		if got := driver.RouteCatalog(d, driver.Route{Path: path}); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

func model(pricing config.Pricing, cat map[string]config.CatalogEntry) *config.ModelConfig {
	return &config.ModelConfig{ID: "m", Pricing: pricing, Catalogs: cat}
}

// TestBillingDecision: the source, the quota owner, Extra Usage and the
// allowance factor by base, plan, key and model; the refusals.
func TestBillingDecision(t *testing.T) {
	bd := billing(t)
	paid := model(config.Pricing{InputPerM: 1, OutputPerM: 2}, nil)
	one := 1.0
	no, yes := false, true
	key := &config.KeyConfig{Key: "k"}
	svc := &config.KeyConfig{Key: "s", ServiceAccount: true}
	oc := &core.Request{Headers: map[string]string{"user-agent": "opencode/latest/2.2.5/x"}}
	goRoute := driver.Route{Path: "/inference/go/openai/v1/chat/completions"}
	payg := driver.Route{Path: "/zen/openai/v1/chat/completions"}
	cases := []struct {
		name string
		q    driver.BillingQuery
		want driver.Billing
	}{
		{"go by default", driver.BillingQuery{Route: goRoute, Catalog: "opencode-go", Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 0.6}},
		{"catalog from the route", driver.BillingQuery{Route: goRoute, Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 0.6}},
		{"gemini", driver.BillingQuery{Catalog: "opencode-go", Key: key, Model: &config.ModelConfig{ID: "gemini-4-argon", Pricing: config.Pricing{InputPerM: 1}}, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 2}},
		{"catalog factor", driver.BillingQuery{Catalog: "opencode-go", Key: key, Model: model(config.Pricing{InputPerM: 1}, map[string]config.CatalogEntry{"opencode-go": {AllowanceFactor: &one}}), Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 1}},
		{"go-plus", driver.BillingQuery{Catalog: "opencode-go", Plan: "go-plus", Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go-plus", AllowanceFactor: 0.6}},
		{"go-teams", driver.BillingQuery{Catalog: "opencode-go", Plan: "go-teams", Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember, ExtraUsage: true}},
		{"go-teams without extra usage", driver.BillingQuery{Catalog: "opencode-go", Plan: "go-teams", Account: &config.AccountConfig{ExtraUsage: &no}, Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember}},
		{"go with extra usage", driver.BillingQuery{Catalog: "opencode-go", Account: &config.AccountConfig{ExtraUsage: &yes}, Key: key, Model: paid, Request: oc}, driver.Billing{Source: "go", AllowanceFactor: 0.6, ExtraUsage: true}},
		{"service account", driver.BillingQuery{Catalog: "opencode-go", Key: svc, Model: paid, Request: oc}, driver.Billing{Source: "balance"}},
		{"pay-as-you-go", driver.BillingQuery{Route: payg, Catalog: "opencode", Key: key, Model: paid}, driver.Billing{Source: "balance"}},
		{"free on pay-as-you-go", driver.BillingQuery{Catalog: "opencode", Credential: driver.Credential{Anonymous: true}, Model: model(config.Pricing{}.Declare(), nil), Request: oc}, driver.Billing{Source: "free", FreeDailyRequests: 200}},
		{"free by the catalog's price", driver.BillingQuery{Catalog: "opencode-go", Key: key, Model: model(config.Pricing{InputPerM: 1}, map[string]config.CatalogEntry{"opencode-go": {Pricing: ptr(config.Pricing{}.Declare())}}), Request: oc}, driver.Billing{Source: "free"}},
		{"auxiliary route, Go", driver.BillingQuery{Route: goRoute, Catalog: "opencode-go", Plan: "go-plus", Key: key}, driver.Billing{Source: "go-plus"}},
		{"auxiliary route, pay-as-you-go", driver.BillingQuery{Catalog: "opencode", Key: key}, driver.Billing{Source: "balance"}},
	}
	for _, c := range cases {
		got, err := bd.Billing(c.q)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.name, got, err, c.want)
		}
	}
	// Refusals: the error the driver renders.
	for _, c := range []struct {
		name string
		q    driver.BillingQuery
		kind core.ErrorKind
	}{
		{"public on Go", driver.BillingQuery{Catalog: "opencode-go", Credential: driver.Credential{Anonymous: true}, Model: model(config.Pricing{}.Declare(), nil), Request: oc}, core.ErrInvalidAPIKey},
		{"public with a paid model", driver.BillingQuery{Catalog: "opencode", Credential: driver.Credential{Anonymous: true}, Model: paid, Request: oc}, core.ErrInvalidAPIKey},
		{"plan none", driver.BillingQuery{Catalog: "opencode-go", Plan: "none", Key: key, Model: paid, Request: oc}, core.ErrPermission},
		{"generic client", driver.BillingQuery{Catalog: "opencode", Key: key, Model: model(config.Pricing{}.Declare(), nil), Request: &core.Request{}}, core.ErrRateLimit},
	} {
		if _, err := bd.Billing(c.q); err == nil || err.Kind != c.kind {
			t.Errorf("%s: %+v, want %s", c.name, err, c.kind)
		}
	}
	// Plan none on the pay-as-you-go base is billed as usual.
	if b, err := bd.Billing(driver.BillingQuery{Catalog: "opencode", Plan: "none", Key: key, Model: paid}); err != nil || b.Source != "balance" {
		t.Errorf("plan none on pay-as-you-go: %+v %v", b, err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestAuthenticatePublic: public is Anonymous in a bearer, in x-api-key
// on Messages and in x-goog-api-key on Google; x-api-key counts only on
// Messages.
func TestAuthenticatePublic(t *testing.T) {
	d := newDriver(t)
	for _, c := range []struct {
		name   string
		h      driver.MapHeaders
		key    string
		scheme string
		anon   bool
	}{
		{"bearer public", driver.MapHeaders{"authorization": "Bearer public"}, "public", "bearer", true},
		{"bearer key", driver.MapHeaders{"authorization": "Bearer oc_sk_1"}, "oc_sk_1", "bearer", false},
		{"x-api-key on Messages", driver.MapHeaders{"x-api-key": "public", "x-wwz-path": "/inference/anthropic/v1/messages"}, "public", "x-api-key", true},
		{"x-api-key on plain Messages", driver.MapHeaders{"x-api-key": "oc_sk_1", "x-wwz-path": "/zen/go/v1/messages"}, "oc_sk_1", "x-api-key", false},
		{"bearer beats x-api-key", driver.MapHeaders{"authorization": "Bearer oc_sk_2", "x-api-key": "oc_sk_1", "x-wwz-path": "/zen/v1/messages"}, "oc_sk_2", "bearer", false},
		{"x-api-key on Chat", driver.MapHeaders{"x-api-key": "oc_sk_1", "x-wwz-path": "/zen/v1/chat/completions"}, "", "none", false},
		{"x-goog-api-key public", driver.MapHeaders{"x-goog-api-key": "public", "x-wwz-path": "/inference/google/v1beta/models/m:generateContent"}, "public", "x-goog-api-key", true},
	} {
		c.h["x-opencode-session"] = "s"
		got := d.Authenticate(c.h)
		if got.Key != c.key || got.Scheme != c.scheme || got.Anonymous != c.anon {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// TestDeclarationBilling: catalogs, plans, the reserved key, the option
// and the member entity.
func TestDeclarationBilling(t *testing.T) {
	d := declaration(t, "opencode")
	if !slices.Equal(d.Plans, []string{"go", "go-plus", "go-teams", "none"}) || !slices.Equal(d.ReservedKeys, []string{"public"}) {
		t.Errorf("plans %v, reserved %v", d.Plans, d.ReservedKeys)
	}
	if len(d.Catalogs) != 2 || d.Catalogs[0] != (config.CatalogDecl{Name: "opencode"}) || d.Catalogs[1] != (config.CatalogDecl{Name: "opencode-go", Allowance: true}) {
		t.Errorf("catalogs %+v", d.Catalogs)
	}
	i := slices.IndexFunc(d.Options, func(o config.DriverOption) bool { return o.Name == "free_requests_per_day" })
	if i < 0 || d.Options[i].Default != "200" || d.Options[i].Check == nil || d.Options[i].Check("0", nil) == nil || d.Options[i].Check("3", nil) != nil {
		t.Errorf("free_requests_per_day: %+v", d.Options)
	}
	if got := newDriver(t).LimitPolicy().Entities; !slices.Equal(got, []string{"ip", "key", "user", "project", "account"}) {
		t.Errorf("entities %v", got)
	}
	for _, v := range []string{"abc", "-1"} {
		if _, err := driver.New("opencode", map[string]string{"free_requests_per_day": v}); err == nil || !strings.Contains(err.Error(), "free_requests_per_day") {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// TestGatewayErrorsBilling: the errors rendered without the engine: the
// workspace of a key without an account, the coverage of a budget raised
// without an entity (chaos), the suspension, the daily count on Messages.
func TestGatewayErrorsBilling(t *testing.T) {
	d := newDriver(t)
	write := func(req *core.Request, e core.Error) string {
		w := core.NewMemWriter()
		d.WriteError(w, req, &e, nil)
		return string(w.Body())
	}
	h := driver.MapHeaders{}
	driver.SetAccount(h, driver.Account{Key: "oc_sk_solo"})
	a := write(&core.Request{Endpoint: core.EndpointChat, Headers: h}, core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "quota_weekly"}})
	driver.SetAccount(h, driver.Account{Key: "oc_sk_other", Account: "acme"})
	b := write(&core.Request{Endpoint: core.EndpointChat, Headers: h}, core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "quota_weekly"}})
	if !reWorkspace.MatchString(a) || !reWorkspace.MatchString(b) || reWorkspace.FindString(a) == reWorkspace.FindString(b) {
		t.Errorf("workspaces: %s / %s", a, b)
	}
	if got := write(&core.Request{Endpoint: core.EndpointChat}, core.Error{Kind: core.ErrBudget}); !strings.Contains(got, `"coverage":"models"`) {
		t.Errorf("chaos budget: %s", got)
	}
	if got := write(&core.Request{Endpoint: core.EndpointChat}, core.Error{Kind: core.ErrSpendLimit, Extra: map[string]any{"entity_kind": "project"}}); !strings.Contains(got, `"coverage":"all"`) {
		t.Errorf("project budget: %s", got)
	}
	if got := write(&core.Request{Endpoint: core.EndpointMessages}, core.Error{Kind: core.ErrAccountSuspended}); got != `{"type":"error","error":{"type":"permission_error","message":"This workspace has been suspended"}}` {
		t.Errorf("suspended on Messages: %s", got)
	}
	if got := write(&core.Request{Endpoint: core.EndpointMessages}, core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"free_models": true}}); got != `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}` {
		t.Errorf("daily count on Messages: %s", got)
	}
}

// TestUsageRateLimitedView: the billing view's windows, without the
// engine: the largest of the owner and the model quotas, rate-limited when
// used up, and the owner without the store's view reads 0.
func TestUsageRateLimitedView(t *testing.T) {
	q := config.QuotaUSD{MonthlyUSD: 60, Rolling5hPct: 20, WeeklyPct: 50}
	xc := driver.ExtraContext{
		Key:        &config.KeyConfig{Key: "k"},
		QuotaOwner: &driver.QuotaView{Kind: "account", ID: "a", Quota: q, State: &driver.Balance{Quota5hUSD: 6, QuotaWeeklyUSD: 6, QuotaMonthlyUSD: 6}},
		ModelQuotas: []driver.ModelQuotaView{
			{Model: "m", Quota: config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 20, WeeklyPct: 50}, State: &driver.Balance{Quota5hUSD: 1, QuotaWeeklyUSD: 5, QuotaMonthlyUSD: 5}},
			{Model: "unread", Quota: q},
		},
	}
	u := usageOf(t, xc)
	if u.Usage["rolling"].Percent != 50 || u.Usage["rolling"].Status != "ok" {
		t.Errorf("rolling %+v", u.Usage["rolling"])
	}
	if u.Usage["weekly"].Percent != 100 || u.Usage["weekly"].Status != "rate-limited" {
		t.Errorf("weekly %+v", u.Usage["weekly"])
	}
	if u.Usage["monthly"].Percent != 50 || u.Usage["monthly"].Status != "ok" {
		t.Errorf("monthly %+v", u.Usage["monthly"])
	}
	xc.QuotaOwner.State, xc.ModelQuotas = nil, nil
	for name, w := range usageOf(t, xc).Usage {
		if w.Percent != 0 || w.Status != "ok" {
			t.Errorf("unread %s: %+v", name, w)
		}
	}
}
