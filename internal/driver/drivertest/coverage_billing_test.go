package drivertest_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
)

// The billing decisions, the public routes and the request of an error
// case (task 425, ADR 0072): a profile that expects what the driver does
// not do fails the run.

func TestBillingCasesFailTheRun(t *testing.T) {
	paid := config.ModelConfig{ID: "deepseek-v4.1-flash", Pricing: config.Pricing{InputPerM: 1, OutputPerM: 2}}
	key := &config.KeyConfig{Key: "oc_sk_wwztest", Label: "wwztest", Account: "acme"}
	hdr := map[string]string{"Authorization": "Bearer oc_sk_wwztest", "X-Opencode-Session": "ses_wwztest"}
	cases := map[string]drivertest.BillingCase{
		"wrong source":            {Name: "x", Base: "/inference", Headers: hdr, Key: key, Model: paid, Want: driver.Billing{Source: driver.BillingGo}},
		"refused, want accepted":  {Name: "x", Base: "/inference/go", Headers: hdr, Key: key, Plan: "none", Model: paid, Want: driver.Billing{Source: driver.BillingGo}},
		"accepted, want refusal":  {Name: "x", Base: "/inference", Headers: hdr, Key: key, Model: paid, Status: 401},
		"no route under the base": {Name: "x", Base: "/nowhere", Headers: hdr, Key: key, Model: paid, Want: driver.Billing{Source: driver.BillingBalance}},
		"other body": {Name: "x", Base: "/inference/go", Headers: hdr, Key: key, Plan: "none", Model: paid, Status: 403, Bodies: map[driver.Kind]string{
			driver.KindChat: `{"error":{"type":"other","message":"other"}}`, driver.KindMessages: `{}`,
		}},
		"other status": {Name: "x", Base: "/inference/go", Headers: hdr, Key: key, Plan: "none", Model: paid, Status: 402},
	}
	for name, c := range cases {
		p, _ := drivertest.ProfileFor("opencode")
		p.Billing = []drivertest.BillingCase{c}
		if runSuite(mustNew(t, "opencode"), drivertest.WithProfile(p)) {
			t.Errorf("%s: the run passed", name)
		}
	}
	// A driver without interface (d) fails a profile with billing cases.
	p, _ := drivertest.ProfileFor("openai")
	p.Billing = []drivertest.BillingCase{{Name: "x", Base: "/v1", Model: paid, Want: driver.Billing{Source: driver.BillingBalance}}}
	if runSuite(mustNew(t, "openai"), drivertest.WithProfile(p)) {
		t.Error("openai passed billing cases without driver.BillingDecider")
	}
}

func TestPublicRoutesFailTheRun(t *testing.T) {
	p, _ := drivertest.ProfileFor("openai")
	p.PublicRoutes = []driver.Route{{Method: "GET", Path: "/v1/models", Kind: driver.KindModels}}
	if runSuite(mustNew(t, "openai"), drivertest.WithProfile(p)) {
		t.Error("a route listed public that needs a key passed")
	}
	oc, _ := drivertest.ProfileFor("opencode")
	oc.PublicRoutes = oc.PublicRoutes[:1]
	if runSuite(mustNew(t, "opencode"), drivertest.WithProfile(oc)) {
		t.Error("a public route the profile does not list passed")
	}
}

// TestErrorCaseRequest: the catalog, account and headers of an error case
// reach WriteError, and the body, absent, pattern, retry-after and resetsAt
// checks fail a wrong expectation.
func TestErrorCaseRequest(t *testing.T) {
	base := drivertest.ErrorCase{
		Name: "x", Err: core.Error{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."},
		Catalog: "opencode", ReqHeaders: map[string]string{"User-Agent": "curl/8.7.1"}, Status: 401,
		Body: `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`,
	}
	run := func(c drivertest.ErrorCase) bool {
		p, _ := drivertest.ProfileFor("opencode")
		sf := p.Surfaces[driver.KindChat]
		sf.Errors = []drivertest.ErrorCase{c}
		p.Surfaces[driver.KindChat] = sf
		return runSuite(mustNew(t, "opencode"), drivertest.WithProfile(p))
	}
	if !run(base) {
		t.Fatal("the case with headers failed")
	}
	wrong := map[string]func(*drivertest.ErrorCase){
		"body":       func(c *drivertest.ErrorCase) { c.Catalog = "opencode-go" },
		"absent":     func(c *drivertest.ErrorCase) { c.Absent = []string{"error.type"} },
		"pattern":    func(c *drivertest.ErrorCase) { c.Patterns = map[string]string{"error.type": `^wrk_`} },
		"midnight":   func(c *drivertest.ErrorCase) { c.RetryAfterMidnight = true },
		"next month": func(c *drivertest.ErrorCase) { c.NextMonth = []string{"error.message"} },
	}
	for name, f := range wrong {
		c := base
		f(&c)
		if run(c) {
			t.Errorf("%s: the wrong expectation passed", name)
		}
	}
}
