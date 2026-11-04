package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// Effective price and route catalog (task 420; ADR 0072, items 15 and 16;
// spec 005, optional interface (c)).

// catDriverName is the fake driver with two catalogs: routes under /sub
// serve "sub", the others "paygo".
const catDriverName = "wwz-catalog-test"

var registerCatDriver sync.Once

// catalogDriver is OpenAI's driver under another name that implements
// driver.CatalogRouter, lists "id catalog input_per_m" lines and reports
// the cost the engine set and the catalog the request carries.
type catalogDriver struct{ driver.Driver }

func (catalogDriver) Name() string     { return catDriverName }
func (catalogDriver) Protocol() string { return catDriverName }

func (catalogDriver) CatalogOf(r driver.Route) string {
	if strings.HasPrefix(r.Path, "/sub/") {
		return "sub"
	}
	return "paygo"
}

func (catalogDriver) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	w.Status(200)
	w.Header("x-test-catalog", driver.CatalogOfRequest(req))
	for _, m := range models {
		_, _ = fmt.Fprintf(w, "%s %s %v\n", m.Cfg.ID, m.Catalog, m.Pricing().InputPerM)
	}
}

func (d catalogDriver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	w.Header("x-test-cost-pico", strconv.FormatInt(resp.Usage.PromptCostPicoUSD+resp.Usage.CompletionCostPicoUSD, 10))
	w.Header("x-test-catalog", driver.CatalogOfRequest(req))
	return d.Driver.WriteResponse(w, req, resp, rl)
}

func newCatalogDriver(t *testing.T) driver.Driver {
	t.Helper()
	registerCatDriver.Do(func() {
		config.RegisterDriver(catDriverName, func() (config.DriverDecl, bool) {
			return config.DriverDecl{
				Dialects: []string{config.DialectOpenAI}, AllRoutes: true,
				Catalogs: []config.CatalogDecl{{Name: "paygo"}, {Name: "sub", Allowance: true}},
			}, true
		})
	})
	inner, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	return catalogDriver{inner}
}

func newPricingEngine(t *testing.T, yaml string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := NewMetrics(metrics.New(1000), nil)
	return New(cfg, limits.NewMemory(), scenario.NewRegistry(), m, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func catCall(e *Engine, drv driver.Driver, kind driver.Kind, path, body string) *core.MemWriter {
	w := core.NewMemWriter()
	h := driver.MapHeaders{"authorization": "Bearer k-cat"}
	e.Handle(context.Background(), Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
	return w
}

func chatOf(model string) string {
	return `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hello there"}]}`
}

// usageOf reads usage.prompt_tokens and completion_tokens of a chat reply.
func usageOf(t *testing.T, w *core.MemWriter) (prompt, completion int) {
	t.Helper()
	var r struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body(), &r); err != nil || r.Usage.Prompt == 0 {
		t.Fatalf("usage of %s: %v", w.Body(), err)
	}
	return r.Usage.Prompt, r.Usage.Completion
}

// pico is the cost of prompt and completion tokens at in and out USD per
// million tokens, in pico-dollars.
func pico(prompt, completion int, in, out float64) int64 {
	return int64(float64(prompt)*in*1e6 + float64(completion)*out*1e6)
}

func costHeader(t *testing.T, w *core.MemWriter) int64 {
	t.Helper()
	n, err := strconv.ParseInt(w.Headers()["x-test-cost-pico"], 10, 64)
	if err != nil {
		t.Fatalf("cost header of %d %s: %v", w.StatusCode(), w.Body(), err)
	}
	return n
}

const catalogPricingCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
keys:
  - {key: k-cat, label: cat, account: acct-cat}
models:
  - id: shared
    drivers: [wwz-catalog-test]
    latency: fast
    pricing: {input_per_m: 1, output_per_m: 2}
    catalogs:
      paygo: {}
      sub: {pricing: {input_per_m: 10, output_per_m: 20}}
  - id: paygo-only
    drivers: [wwz-catalog-test]
    latency: fast
    pricing: {input_per_m: 3, output_per_m: 3}
    catalogs: {paygo: {}}
  - id: plain
    drivers: [wwz-catalog-test]
    latency: fast
    pricing: {input_per_m: 4, output_per_m: 4}
`

func TestCatalogRouterListingAcceptanceAndCost(t *testing.T) {
	drv := newCatalogDriver(t)
	e := newPricingEngine(t, catalogPricingCfg)

	// Listing: a model outside the route's catalog is not listed, and each
	// one carries the catalog, whose price the driver shows.
	for path, want := range map[string]string{
		"/v1/models":     "shared paygo 1\npaygo-only paygo 3\nplain paygo 4\n",
		"/sub/v1/models": "shared sub 10\nplain sub 4\n",
	} {
		w := catCall(e, drv, driver.KindModels, path, "")
		if got := string(w.Body()); w.StatusCode() != 200 || got != want {
			t.Errorf("%s: %d %q, want %q", path, w.StatusCode(), got, want)
		}
		if got, cat := w.Headers()["x-test-catalog"], drv.(driver.CatalogRouter).CatalogOf(driver.Route{Path: path}); got != cat {
			t.Errorf("%s: request catalog %q, want %q", path, got, cat)
		}
	}
	// GET /models/{id} serves the same catalog.
	w := core.NewMemWriter()
	e.Handle(context.Background(), Input{Driver: drv, Kind: driver.KindModel, Method: "GET", Path: "/sub/v1/models/paygo-only", PathParam: "paygo-only", Headers: driver.MapHeaders{"authorization": "Bearer k-cat"}}, w)
	if w.StatusCode() != 404 {
		t.Errorf("GET /sub/v1/models/paygo-only: %d %s, want 404", w.StatusCode(), w.Body())
	}

	// Acceptance: outside the catalog is the nonexistent-model error.
	if w := catCall(e, drv, driver.KindChat, "/sub/v1/chat/completions", chatOf("paygo-only")); w.StatusCode() != 404 || !strings.Contains(string(w.Body()), "model_not_found") {
		t.Errorf("paygo-only on /sub: %d %s, want the nonexistent-model 404", w.StatusCode(), w.Body())
	}
	if w := catCall(e, drv, driver.KindChat, "/v1/chat/completions", chatOf("paygo-only")); w.StatusCode() != 200 {
		t.Errorf("paygo-only on /: %d %s", w.StatusCode(), w.Body())
	}

	// Cost: the same id costs its catalog's price.
	for _, c := range []struct {
		path    string
		cat     string
		in, out float64
	}{
		{"/v1/chat/completions", "paygo", 1, 2},
		{"/sub/v1/chat/completions", "sub", 10, 20},
	} {
		ctx := context.Background()
		acct := limits.Entity{Kind: limits.KindAccount, ID: "acct-cat"}
		before, _ := e.Store.Get(ctx, acct)
		w := catCall(e, drv, driver.KindChat, c.path, chatOf("shared"))
		if w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", c.path, w.StatusCode(), w.Body())
		}
		if got := w.Headers()["x-test-catalog"]; got != c.cat {
			t.Errorf("%s: request catalog %q, want %q", c.path, got, c.cat)
		}
		p, o := usageOf(t, w)
		want := pico(p, o, c.in, c.out)
		if got := costHeader(t, w); got != want {
			t.Errorf("%s: cost %d pUSD, want %d (%d+%d tokens at %v/%v)", c.path, got, want, p, o, c.in, c.out)
		}
		// The debit follows the same price.
		after, _ := e.Store.Get(ctx, acct)
		if d := after.SpendUSD - before.SpendUSD; d < float64(want/1e6)/1e6-1e-9 || d > float64(want/1e6)/1e6+1e-9 {
			t.Errorf("%s: spend moved %v USD, want %v", c.path, d, float64(want/1e6)/1e6)
		}
	}
}

func timedCfg(contextGTE int) string {
	return fmt.Sprintf(`
server: {seed: 42, auth: none}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - id: timed
    protocols: [openai]
    latency: fast
    pricing:
      input_per_m: 1
      output_per_m: 3
      multipliers: [{when: {utc_hours: {gte: "16:00", lt: "18:00"}}, factor: 2}]
      overrides: [{when: {context: {gte: %d}}, input_per_m: 5}]
`, contextGTE)
}

func TestEffectivePriceMultiplierAndContextOverride(t *testing.T) {
	drv, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	cost := func(e *Engine) (int64, int, int) {
		t.Helper()
		var got int64
		w := core.NewMemWriter()
		e.Handle(context.Background(), Input{Driver: costProbe{drv, &got}, Kind: driver.KindChat, Method: "POST", Path: "/v1/chat/completions", Headers: driver.MapHeaders{}, Body: []byte(chatOf("timed")), ClientIP: "127.0.0.1"}, w)
		if w.StatusCode() != 200 {
			t.Fatalf("%d %s", w.StatusCode(), w.Body())
		}
		p, o := usageOf(t, w)
		return got, p, o
	}
	// The prompt size, to put the override's border on it.
	_, prompt, _ := cost(newPricingEngine(t, timedCfg(1<<30)))
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		at      time.Duration
		border  int
		in, out float64
	}{
		{"before the window", 15*time.Hour + 59*time.Minute, prompt + 1, 1, 3},
		{"window opens (gte)", 16 * time.Hour, prompt + 1, 2, 6},
		{"inside the window", 17*time.Hour + 59*time.Minute, prompt + 1, 2, 6},
		{"window closed (lt)", 18 * time.Hour, prompt + 1, 1, 3},
		{"context at the border", 12 * time.Hour, prompt, 5, 3},
		{"context one below the border", 12 * time.Hour, prompt + 1, 1, 3},
		{"override and multiplier", 16*time.Hour + 30*time.Minute, prompt, 10, 6},
	} {
		e := newPricingEngine(t, timedCfg(c.border))
		at := day.Add(c.at)
		e.Now = func() time.Time { return at }
		got, p, o := cost(e)
		if want := pico(p, o, c.in, c.out); got != want {
			t.Errorf("%s: cost %d pUSD, want %d (%d+%d tokens at %v/%v)", c.name, got, want, p, o, c.in, c.out)
		}
	}
}

// costProbe reports the cost the engine set on the response.
type costProbe struct {
	driver.Driver
	got *int64
}

func (d costProbe) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	*d.got = resp.Usage.PromptCostPicoUSD + resp.Usage.CompletionCostPicoUSD
	return d.Driver.WriteResponse(w, req, resp, rl)
}

func TestPricedModelKeepsFlatPricing(t *testing.T) {
	flat := &config.ModelConfig{ID: "flat", Pricing: config.Pricing{InputPerM: 1}}
	if pricedModel(flat, "", time.Now(), 10) != flat || pricedModel(flat, "x", time.Now(), 10) != flat {
		t.Error("a flat pricing must not copy the model")
	}
	if pricedModel(nil, "", time.Now(), 0) != nil {
		t.Error("nil model")
	}
	sub := 7.0
	m := &config.ModelConfig{ID: "c", Pricing: config.Pricing{InputPerM: 1}, Catalogs: map[string]config.CatalogEntry{"sub": {Pricing: &config.Pricing{InputPerM: sub}}, "paygo": {}}}
	if got := pricedModel(m, "sub", time.Now(), 0); got == m || got.Pricing.InputPerM != sub || m.Pricing.InputPerM != 1 {
		t.Errorf("sub: %+v (model %+v)", got.Pricing, m.Pricing)
	}
	if pricedModel(m, "paygo", time.Now(), 0) != m {
		t.Error("a catalog without pricing keeps the model's")
	}
}

func TestCatalogHelpers(t *testing.T) {
	driver.SetCatalog(nil, "x")
	if driver.CatalogFrom(nil) != "" || driver.CatalogOfRequest(nil) != "" {
		t.Error("nil headers carry no catalog")
	}
	h := driver.MapHeaders{}
	driver.SetCatalog(h, "sub")
	if driver.CatalogFrom(h) != "sub" {
		t.Error("SetCatalog")
	}
	driver.SetCatalog(h, "")
	if len(h) != 0 {
		t.Errorf("SetCatalog(\"\") left %v", h)
	}
	if (driver.ModelInfo{}).Pricing().Declared() {
		t.Error("ModelInfo without Cfg has no pricing")
	}
	if driver.RouteCatalog(catalogDriver{}, driver.Route{Path: "/sub/v1/models"}) != "sub" {
		t.Error("RouteCatalog")
	}
}
