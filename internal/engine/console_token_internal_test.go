package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 425 (ADR 0072, item 17; spec 005): a token that stands for a keys[]
// entry (driver.KeyResolver, OpenCode's Console access token) and a route
// answered without a credential (driver.PublicRouter, the Console's device
// flow), with the billing driver of billing_subjects_test.go; and the
// billing view of an auxiliary route when the store cannot be read.

// tokenDriver is the billing driver with a resolver of "tok-<label>"
// tokens and the public auxiliary route /v1/open.
type tokenDriver struct{ billingDriver }

func (tokenDriver) ResolveKey(token string, keys driver.KeyLookup, _ time.Time) (string, bool) {
	label, ok := strings.CutPrefix(token, "tok-")
	if !ok {
		return "", false
	}
	if label == "ghost" || keys(label) != nil {
		// "ghost" names no keys[] entry: a token of a label the
		// configuration lost, as after a reload.
		return label, true
	}
	return "", false
}

func (tokenDriver) PublicRoute(r driver.Route) bool { return r.Path == "/v1/open" }

func newTokenDriver(t *testing.T) driver.Driver {
	t.Helper()
	return tokenDriver{newBillingDriver(t).(billingDriver)}
}

func TestResolvedTokenAuthenticates(t *testing.T) {
	drv := newTokenDriver(t)
	e := newPricingEngine(t, billingCfg)
	for _, c := range []struct {
		key    string
		status int
		source string
	}{
		{"tok-acme-key", 200, driver.BillingBalance}, // the token stands for k-acme
		{"k-acme", 200, driver.BillingBalance},       // the key itself, unresolved
		{"tok-ghost", 401, ""},                       // a label keys[] does not have
		{"tok-nobody", 401, ""},                      // the resolver refuses it
		{"other", 401, ""},                           // not a token at all
	} {
		w := billCall(e, drv, c.key, "", paygoPath, "paid")
		if w.StatusCode() != c.status {
			t.Errorf("%s: %d %s, want %d", c.key, w.StatusCode(), w.Body(), c.status)
			continue
		}
		if got := w.Headers()["x-test-billing"]; c.source != "" && got != c.source {
			t.Errorf("%s: billing %q, want %q", c.key, got, c.source)
		}
	}
	// The token is the key's account: its quota usage is acme's.
	w := core.NewMemWriter()
	e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindUsage, Method: "GET", Path: "/go/v1/usage", Headers: driver.MapHeaders{"authorization": "Bearer tok-acme-key"}}, w)
	var out map[string]any
	if err := json.Unmarshal(w.Body(), &out); err != nil || out["account"] != "acme" {
		t.Errorf("usage with the token: %d %s", w.StatusCode(), w.Body())
	}
}

func TestPublicAuxiliaryRoute(t *testing.T) {
	drv := newTokenDriver(t)
	e := newPricingEngine(t, billingCfg)
	call := func(path, key string) *core.MemWriter {
		h := driver.MapHeaders{}
		if key != "" {
			h["authorization"] = "Bearer " + key
		}
		w := core.NewMemWriter()
		e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindUsage, Method: "POST", Path: path, Headers: h}, w)
		return w
	}
	for _, key := range []string{"", "nope", "k-acme"} {
		if w := call("/v1/open", key); w.StatusCode() != 200 {
			t.Errorf("public route with %q: %d %s", key, w.StatusCode(), w.Body())
		}
		if w := call("/v1/closed", key); key != "k-acme" && w.StatusCode() != 401 {
			t.Errorf("closed route with %q: %d %s", key, w.StatusCode(), w.Body())
		}
	}
}

// TestBillingViewStoreDown: the usage route still answers when the store
// cannot be read; the quota and budget states are absent and the spend 0.
func TestBillingViewStoreDown(t *testing.T) {
	drv := newBillingDriver(t)
	e := newPricingEngine(t, strings.Replace(billingCfg, "  - key: k-budget\n", "  - key: k-budget\n    project: alice\n", 1)+`
projects:
  - {id: alice, limits: {spend_monthly_usd: 5, spend_monthly_models_usd: {paid: 0, paygo/paid2: 2}}}
`)
	e.Store = failingGet{e.Store}
	w := core.NewMemWriter()
	e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindUsage, Method: "GET", Path: "/go/v1/usage", Headers: driver.MapHeaders{"authorization": "Bearer k-budget"}}, w)
	var out struct {
		Owner struct {
			State any
		} `json:"owner"`
		ModelQuotas []struct{ Model string } `json:"model_quotas"`
		Budgets     []struct {
			Level, Model string
			SpentUSD     float64
		} `json:"budgets"`
	}
	if w.StatusCode() != 200 || json.Unmarshal(w.Body(), &out) != nil {
		t.Fatalf("usage: %d %s", w.StatusCode(), w.Body())
	}
	if out.Owner.State != nil || len(out.ModelQuotas) != 1 {
		t.Errorf("owner %+v, model quotas %+v", out.Owner, out.ModelQuotas)
	}
	var models []string
	for _, b := range out.Budgets {
		if b.SpentUSD != 0 {
			t.Errorf("budget %+v spent with the store down", b)
		}
		if b.Level == "project" {
			models = append(models, b.Model)
		}
	}
	// A budget of 0 is no budget.
	if strings.Join(models, ",") != ",paygo/paid2" {
		t.Errorf("project budgets %v, want the whole one and paygo/paid2", models)
	}
	// A key that is neither an id nor <catalog>/<id> names itself (the
	// configuration refuses it; the engine does not depend on that).
	if got := modelIDOfKey(e.Config(), "gone"); got != "gone" {
		t.Errorf("modelIDOfKey(gone) = %q", got)
	}
}
