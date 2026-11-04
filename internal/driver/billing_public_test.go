package driver_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 425 (ADR 0072; spec 005, optional interfaces (c) and (d) and
// PublicRouter): the request slots of the catalog and the billing, the
// listing's pricing by catalog and IsPublicRoute, without a real driver.

// fakeDriver is a Driver whose optional interfaces are those of its
// embedded value; it routes nothing itself.
type fakeDriver struct{ driver.Driver }

// publicDriver answers the route /open without a credential.
type publicDriver struct{ fakeDriver }

func (publicDriver) PublicRoute(r driver.Route) bool { return r.Path == "/open" }

// wrapped is a decorator over another driver, found through Unwrap.
type wrapped struct {
	fakeDriver
	inner driver.Driver
}

func (w wrapped) Unwrap() driver.Driver { return w.inner }

func TestIsPublicRoute(t *testing.T) {
	open, closed := driver.Route{Method: "POST", Path: "/open"}, driver.Route{Method: "GET", Path: "/closed"}
	p := publicDriver{}
	if !driver.IsPublicRoute(p, open) || driver.IsPublicRoute(p, closed) {
		t.Error("PublicRouter not consulted")
	}
	if !driver.IsPublicRoute(wrapped{inner: p}, open) {
		t.Error("a decorated PublicRouter not found through Unwrap")
	}
	if driver.IsPublicRoute(fakeDriver{}, open) {
		t.Error("a driver without PublicRouter has a public route")
	}
}

func TestCatalogSlot(t *testing.T) {
	driver.SetCatalog(nil, "opencode") // no panic
	h := driver.MapHeaders{}
	driver.SetCatalog(h, "opencode-go")
	if got := driver.CatalogFrom(h); got != "opencode-go" {
		t.Errorf("CatalogFrom %q", got)
	}
	if got := driver.CatalogOfRequest(&core.Request{Headers: h}); got != "opencode-go" {
		t.Errorf("CatalogOfRequest %q", got)
	}
	driver.SetCatalog(h, "")
	if got := driver.CatalogFrom(h); got != "" || len(h) != 0 {
		t.Errorf("after removal %q %v", got, h)
	}
	if driver.CatalogFrom(nil) != "" || driver.CatalogOfRequest(nil) != "" {
		t.Error("nil headers or request carry a catalog")
	}
	// A client cannot send the slot: its name is not an HTTP header.
	if driver.CatalogFrom(driver.MapHeaders{"wwz-catalog": "opencode"}) != "" {
		t.Error("a plain header reached the slot")
	}
}

func TestBillingSlot(t *testing.T) {
	driver.SetBilling(nil, driver.Billing{Source: driver.BillingGo}) // no panic
	for _, b := range []driver.Billing{
		{Source: driver.BillingGo, AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember, ExtraUsage: true},
		{Source: driver.BillingFree, FreeDailyRequests: 200},
		{Source: driver.BillingBalance},
	} {
		h := driver.MapHeaders{}
		driver.SetBilling(h, b)
		got, ok := driver.BillingFrom(h)
		if !ok || got != b {
			t.Errorf("BillingFrom %+v %v, want %+v", got, ok, b)
		}
		if got, ok := driver.BillingOf(&core.Request{Headers: h}); !ok || got != b {
			t.Errorf("BillingOf %+v %v, want %+v", got, ok, b)
		}
	}
	if _, ok := driver.BillingFrom(nil); ok {
		t.Error("nil headers carry a billing")
	}
	if _, ok := driver.BillingFrom(driver.MapHeaders{}); ok {
		t.Error("empty headers carry a billing")
	}
	if _, ok := driver.BillingOf(nil); ok {
		t.Error("a nil request carries a billing")
	}
}

func TestModelInfoPricing(t *testing.T) {
	if p := (driver.ModelInfo{}).Pricing(); p.Declared() {
		t.Errorf("no model: %+v", p)
	}
	go_ := config.Pricing{InputPerM: 0.15, OutputPerM: 0.6}
	m := &config.ModelConfig{
		ID: "deepseek-v4.1-flash", Pricing: config.Pricing{InputPerM: 0.3, OutputPerM: 1.2},
		Catalogs: map[string]config.CatalogEntry{"opencode-go": {Pricing: &go_}},
	}
	if p := (driver.ModelInfo{Cfg: m, Catalog: "opencode-go"}).Pricing(); p.InputPerM != 0.15 || p.OutputPerM != 0.6 {
		t.Errorf("Go catalog: %+v", p)
	}
	if p := (driver.ModelInfo{Cfg: m, Catalog: "opencode"}).Pricing(); p.InputPerM != 0.3 {
		t.Errorf("a catalog without its own price: %+v", p)
	}
}
