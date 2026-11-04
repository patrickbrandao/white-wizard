package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// ADR 0048: protocols is the wire dialect, drivers the catalog.

func TestCatalogValidation(t *testing.T) {
	for _, c := range []struct{ yaml, err string }{
		{"models: [{id: m, protocols: [openai, anthropic, gemini]}]", ""},
		{"models: [{id: m, protocols: ['*'], drivers: [gemini, azure]}]", ""},
		{"models: [{id: m, protocols: [anthropic], drivers: [openrouter]}]", ""},
		{"models: [{id: m}]", ""},
		{"models: [{id: m, protocols: [bedrock]}]", `models[0].protocols: unknown value "bedrock"`},
		{"models: [{id: a}, {id: m, protocols: [openai], drivers: [vertex]}]", `models[1].drivers: unknown driver "vertex"`},
		{"models: [{id: m, protocols: [openai], drivers: [anthropic]}]", `models[0].drivers: driver "anthropic" speaks none of protocols [openai]`},
		{"models: [{id: m, protocols: [gemini], drivers: [azure]}]", `driver "azure" speaks none`},
	} {
		_, err := Parse([]byte(c.yaml))
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.yaml, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err %v, want %q", c.yaml, err, c.err)
		}
	}
}

func TestLegacyProtocolsTranslated(t *testing.T) {
	c, err := Parse([]byte(`models:
  - {id: a, protocols: [openai, anthropic, openrouter, opencode]}
  - {id: b, protocols: [openrouter]}
  - {id: c, protocols: [anthropic, opencode], drivers: [anthropic]}
  - {id: d, protocols: ['*', openrouter]}
  - {id: e, protocols: [openai]}
`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][2][]string{
		{{"openai", "anthropic"}, {"openai", "anthropic", "openrouter", "opencode"}},
		{{"openai"}, {"openrouter"}},
		{{"anthropic"}, {"anthropic", "opencode"}},
		{{"*"}, nil},
		{{"openai"}, nil},
	} {
		m := c.Models[i]
		if !slices.Equal(m.Protocols, want[0]) || !slices.Equal(m.Drivers, want[1]) {
			t.Errorf("models[%d]: protocols %v drivers %v, want %v %v", i, m.Protocols, m.Drivers, want[0], want[1])
		}
	}
	w := c.Warnings()
	if len(w) != 4 || !strings.Contains(w[1], "models[1].protocols: driver names [openrouter] are deprecated; read as protocols [openai] and drivers [openrouter]") {
		t.Errorf("warnings: %q", w)
	}
	// Validating again (serve re-validates after flag overrides) is a no-op.
	if err := c.Validate(); err != nil || len(c.Warnings()) != 4 {
		t.Errorf("revalidate: %v %q", err, c.Warnings())
	}
	// The built-in models are in the new form: no warning.
	d, err := Parse(nil)
	if err != nil || len(d.Warnings()) != 0 {
		t.Errorf("defaults: %v %q", err, d.Warnings())
	}
	for _, m := range d.Models {
		if !slices.Equal(m.Protocols, []string{"openai", "anthropic"}) || len(m.Drivers) != 0 {
			t.Errorf("default %s: %v %v", m.ID, m.Protocols, m.Drivers)
		}
	}
}

func TestServedOn(t *testing.T) {
	oa := &ModelConfig{Protocols: []string{"openai"}}
	an := &ModelConfig{Protocols: []string{"anthropic"}, Drivers: []string{"anthropic", "openrouter"}}
	all := &ModelConfig{}
	for _, c := range []struct {
		m          *ModelConfig
		drv, dia   string
		in, served bool
	}{
		{oa, "openai", "openai", true, true},
		{oa, "azure", "openai", true, true},
		{oa, "openrouter", "anthropic", true, true}, // OpenRouter: every route
		{oa, "opencode", "openai", true, true},
		{oa, "opencode", "anthropic", true, true}, // OpenCode: every route (ADR 0062)
		{oa, "opencode", "gemini", true, true},
		{oa, "anthropic", "anthropic", false, false},
		{oa, "gemini", "gemini", false, false},
		{an, "anthropic", "anthropic", true, true},
		{an, "opencode", "anthropic", false, false}, // outside the catalog
		{an, "openrouter", "openai", true, true},
		{all, "gemini", "gemini", true, true},
		{all, "custom-test-driver", "openai", true, true},
	} {
		if got := c.m.ModelSupports(c.drv); got != c.in {
			t.Errorf("%+v ModelSupports(%s) = %v", c.m, c.drv, got)
		}
		if got := c.m.ServedOn(c.drv, c.dia); got != c.served {
			t.Errorf("%+v ServedOn(%s, %s) = %v", c.m, c.drv, c.dia, got)
		}
	}
}

func TestAddToCatalog(t *testing.T) {
	// Implicit catalog: kept, plus the new driver.
	m := &ModelConfig{Protocols: []string{"openai"}}
	m.AddToCatalog("openai") // already in: no change
	if m.Drivers != nil {
		t.Fatalf("drivers %v", m.Drivers)
	}
	m.AddToCatalog("anthropic")
	if !slices.Equal(m.Protocols, []string{"openai", "anthropic"}) || !slices.Equal(m.Drivers, []string{"azure", "openai", "opencode", "openrouter", "anthropic"}) {
		t.Errorf("implicit: %v %v", m.Protocols, m.Drivers)
	}
	if !m.ServedOn("anthropic", "anthropic") || m.ModelSupports("gemini") {
		t.Errorf("implicit served: %+v", m)
	}
	// Explicit catalog; OpenRouter needs no dialect.
	g := &ModelConfig{Protocols: []string{"gemini"}, Drivers: []string{"gemini"}}
	g.AddToCatalog("openrouter")
	if !slices.Equal(g.Protocols, []string{"gemini"}) || !slices.Equal(g.Drivers, []string{"gemini", "openrouter"}) || !g.ServedOn("openrouter", "openai") {
		t.Errorf("explicit: %v %v", g.Protocols, g.Drivers)
	}
}

// withCatalogDriver registers, for the test, a driver that serves every
// route and declares two catalogs, one with an allowance (ADR 0072).
func withCatalogDriver(t *testing.T) {
	t.Helper()
	saved := decls.Load()
	t.Cleanup(func() { decls.Store(saved) })
	RegisterDriver("wwz-catalogs", func() (DriverDecl, bool) {
		return DriverDecl{
			Dialects: []string{DialectOpenAI}, AllRoutes: true,
			Catalogs:   []CatalogDecl{{Name: "paygo"}, {Name: "sub", Allowance: true}},
			ModelExtra: []string{"family", "api", "capabilities", "name", "knowledge"},
		}, true
	})
}

func TestModelCatalogs(t *testing.T) {
	withCatalogDriver(t)
	c, err := Parse([]byte(`models:
  - id: both
    drivers: [wwz-catalogs]
    pricing: {input_per_m: 0.3, output_per_m: 1.2}
    catalogs:
      paygo: {}
      sub:
        pricing:
          input_per_m: 0.15
          multipliers: [{when: {utc_days: {in: [mon]}}, factor: 2}]
        allowance_factor: 0.6
  - id: free-paygo
    drivers: [wwz-catalogs]
    catalogs: {paygo: {pricing: {input_per_m: 0}}, sub: {}}
  - id: plain
    drivers: [wwz-catalogs]
  - id: implicit
    catalogs: {sub: {allowance_factor: 0}}
`))
	if err != nil {
		t.Fatal(err)
	}
	both, free, plain, implicit := c.FindModel("both"), c.FindModel("free-paygo"), c.FindModel("plain"), c.FindModel("implicit")
	for _, tc := range []struct {
		m       *ModelConfig
		catalog string
		in      bool
		input   float64
		free    bool
	}{
		{both, "paygo", true, 0.3, false},
		{both, "sub", true, 0.15, false},
		{both, "", true, 0.3, false},
		{both, "other", false, 0.3, false},
		{free, "paygo", true, 0, true},
		{free, "sub", true, 0, false}, // no pricing anywhere: paid at US$ 0
		{plain, "paygo", true, 0, false},
		{plain, "sub", true, 0, false},
		{implicit, "paygo", false, 0, false},
	} {
		if got := tc.m.InCatalog(tc.catalog); got != tc.in {
			t.Errorf("%s InCatalog(%q) = %v", tc.m.ID, tc.catalog, got)
		}
		p := tc.m.PricingFor(tc.catalog)
		if p.InputPerM != tc.input || p.Free() != tc.free {
			t.Errorf("%s PricingFor(%q) = %+v free %v", tc.m.ID, tc.catalog, p, p.Free())
		}
	}
	// The catalog's pricing replaces the whole block, conditions included.
	if e := both.PricingFor("sub").Effective(at("2026-11-02 12:00:00"), 0); !near(e.InputPerM, 0.3) || e.OutputPerM != 0 {
		t.Errorf("sub effective: %+v", e)
	}
	if f, ok := both.AllowanceFactor("sub"); !ok || f != 0.6 {
		t.Errorf("allowance sub: %v %v", f, ok)
	}
	if f, ok := implicit.AllowanceFactor("sub"); !ok || f != 0 {
		t.Errorf("allowance 0: %v %v", f, ok)
	}
	for _, cat := range []string{"paygo", "other"} {
		if _, ok := both.AllowanceFactor(cat); ok {
			t.Errorf("allowance %s declared", cat)
		}
	}
	// Catalogs survive GET/POST /admin/config.
	back := roundTrip(t, c)
	if b := back.FindModel("both"); !b.InCatalog("paygo") || b.PricingFor("sub").InputPerM != 0.15 || len(b.PricingFor("sub").Multipliers) != 1 {
		t.Errorf("round trip both: %+v", b.Catalogs)
	}
	if b := back.FindModel("free-paygo"); !b.PricingFor("paygo").Free() || b.PricingFor("sub").Declared() {
		t.Errorf("round trip free-paygo: %+v", b.Catalogs)
	}
	if b := back.FindModel("implicit"); b.InCatalog("paygo") {
		t.Error("round trip implicit")
	}
	if f, ok := back.FindModel("implicit").AllowanceFactor("sub"); !ok || f != 0 {
		t.Errorf("round trip allowance 0: %v %v", f, ok)
	}
}

func TestModelCatalogsValidation(t *testing.T) {
	withCatalogDriver(t)
	for _, tc := range []struct{ yaml, err string }{
		{"models: [{id: m, catalogs: {}}]", ""},
		{"models: [{id: a}, {id: m, drivers: [wwz-catalogs], catalogs: {gold: {}}}]", `config: models[1].catalogs: unknown catalog "gold" (want paygo, sub)`},
		{"models: [{id: m, drivers: [openai], catalogs: {paygo: {}}}]", `config: models[0].catalogs: no driver serving the model has catalogs`},
		{"models: [{id: m, protocols: [anthropic], drivers: [anthropic], catalogs: {sub: {}}}]", `no driver serving the model has catalogs`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {paygo: {allowance_factor: 1}}}]", `config: models[0].catalogs.paygo.allowance_factor: catalog "paygo" has no allowance`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {allowance_factor: -0.1}}}]", `config: models[0].catalogs.sub.allowance_factor must be >= 0 and finite, got -0.1`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {allowance_factor: .nan}}}]", `allowance_factor must be >= 0`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {pricing: {multipliers: [{when: {x: 1}, factor: 2}]}}}}]", `config: models[0].catalogs.sub.pricing.multipliers[0].when: unknown condition "x"`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {bogus: 1}}}]", `unknown field "bogus"`},
		// The fields of the effective model (revision of task 434).
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {context_window: 1, max_output: 1, capabilities: {tools: false}, extra: {family: x, api: {messages: null}}}}}]", ""},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {context_window: 0}}}]", `config: models[0].catalogs.sub.context_window must be > 0, got 0`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {paygo: {max_output: -5}}}]", `config: models[0].catalogs.paygo.max_output must be > 0, got -5`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {extra: {benchmarks: {}}}}}]", `config: models[0].catalogs.sub.extra: key "benchmarks" unknown to dialects [openai]`},
		{"models: [{id: m, drivers: [wwz-catalogs], extra: {benchmarks: 1}}]", `config: models[0].extra: key "benchmarks" unknown to dialects [openai]`},
		{"models: [{id: m, drivers: [wwz-catalogs], catalogs: {sub: {capabilities: {telepathy: true}}}}]", `unknown field "telepathy"`},
	} {
		_, err := Parse([]byte(tc.yaml))
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.yaml, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: err %v, want %q", tc.yaml, err, tc.err)
		}
	}
}

// TestForCatalog: the model effective in a catalog (ADR 0072, item 16,
// revision of task 434). An entry with nothing besides allowance_factor,
// "" and an unknown catalog give the model itself; otherwise a copy where
// pricing, context_window and max_output replace, capabilities replace
// field by field and extra merges deeply, its nulls kept. The model is
// never changed, and the effective model is the same again in its catalog.
func TestForCatalog(t *testing.T) {
	withCatalogDriver(t)
	c, err := Parse([]byte(`models:
  - id: m
    drivers: [wwz-catalogs]
    context_window: 512000
    max_output: 128000
    capabilities: {tools: true, reasoning: true}
    pricing: {input_per_m: 0.3}
    extra:
      family: minimax
      capabilities: {cache: {implicit: true}, reasoning: {toggle: true, effort: [high, max]}}
      api: {chat-completions: {sdk: {aisdk: x}}}
    catalogs:
      paygo: {}
      sub:
        allowance_factor: 0.6
        context_window: 1000000
        max_output: 131072
        capabilities: {tools: false, vision: true}
        pricing: {input_per_m: 0.6}
        extra:
          family: minimax-m3
          knowledge: "2025-01"
          capabilities: {cache: null, reasoning: {toggle: null, effort: [low]}}
          api: {chat-completions: null, messages: {}}
  - id: only-allowance
    drivers: [wwz-catalogs]
    catalogs: {sub: {allowance_factor: 1}}
  - id: only-extra
    drivers: [wwz-catalogs]
    catalogs: {sub: {extra: {name: N}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := c.FindModel("m")
	before, _ := json.Marshal(m)
	for _, name := range []string{"", "paygo", "other"} {
		if m.ForCatalog(name) != m {
			t.Errorf("ForCatalog(%q) copied the model", name)
		}
	}
	if oa := c.FindModel("only-allowance"); oa.ForCatalog("sub") != oa {
		t.Error("allowance_factor alone copied the model")
	}
	var nilModel *ModelConfig
	if nilModel.ForCatalog("sub") != nil {
		t.Error("nil model")
	}

	e := m.ForCatalog("sub")
	if e == m || e.ContextWindow != 1000000 || e.MaxOutput != 131072 || e.Pricing.InputPerM != 0.6 {
		t.Fatalf("effective sub: %+v", e)
	}
	caps := e.Capabilities
	if ok := caps.Tools != nil && !*caps.Tools && caps.Reasoning != nil && *caps.Reasoning && caps.Vision != nil && *caps.Vision && caps.JSONSchema == nil; !ok {
		t.Errorf("capabilities field by field: %+v", caps)
	}
	want := `{"api":{"chat-completions":null,"messages":{}},` +
		`"capabilities":{"cache":null,"reasoning":{"effort":["low"],"toggle":null}},` +
		`"family":"minimax-m3","knowledge":"2025-01"}`
	if got, _ := json.Marshal(e.Extra); string(got) != want {
		t.Errorf("extra merge\n got  %s\n want %s", got, want)
	}
	if !e.InCatalog("sub") || e.InCatalog("other") || e.PricingFor("sub").InputPerM != 0.6 {
		t.Error("catalogs lost on the effective model")
	}
	if f, ok := e.AllowanceFactor("sub"); !ok || f != 0.6 {
		t.Errorf("allowance on the effective model: %v %v", f, ok)
	}
	again, _ := json.Marshal(e.ForCatalog("sub"))
	if eff, _ := json.Marshal(e); string(again) != string(eff) {
		t.Errorf("ForCatalog is not idempotent:\n%s\n%s", eff, again)
	}
	if after, _ := json.Marshal(m); string(after) != string(before) {
		t.Errorf("the model changed:\n%s\n%s", before, after)
	}

	// extra over a model without extra; the other fields stay.
	oe := c.FindModel("only-extra")
	if e := oe.ForCatalog("sub"); e == oe || e.Extra["name"] != "N" || e.ContextWindow != oe.ContextWindow || e.MaxOutput != oe.MaxOutput {
		t.Errorf("only-extra: %+v", e)
	}

	// The new fields survive GET/POST /admin/config, nulls included.
	back := roundTrip(t, c).FindModel("m").ForCatalog("sub")
	if got, _ := json.Marshal(back.Extra); string(got) != want || back.ContextWindow != 1000000 || back.MaxOutput != 131072 || *back.Capabilities.Tools {
		t.Errorf("round trip: %s %+v", got, back)
	}
}
