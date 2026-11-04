package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// fixtureCosts reads cost of each model of the real OpenCode Go catalog
// (testdata/real/opencode/shape/models.json).
func fixtureCosts(t *testing.T) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../testdata/real/opencode/shape/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Response struct {
			Body json.RawMessage `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	body := fx.Response.Body
	var s string
	if json.Unmarshal(body, &s) == nil {
		body = json.RawMessage(s)
	}
	var list struct {
		Data []struct {
			ID   string         `json:"id"`
			Cost map[string]any `json:"cost"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, m := range list.Data {
		out[m.ID] = m.Cost
	}
	if len(out) == 0 {
		t.Fatal("no model in the fixture")
	}
	return out
}

// jsonOf renders v as JSON, which is YAML flow.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// path walks a decoded JSON value.
func path(v any, keys ...any) any {
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// fixtureConfig declares the fixture's DeepSeek, Gemini 4 Argon and Grok 4.7
// with the real conditions copied verbatim.
func fixtureConfig(t *testing.T) *Config {
	t.Helper()
	fx := fixtureCosts(t)
	ds := path(fx["deepseek-v4.1-flash"], "multipliers", 0, "when")
	ar := path(fx["gemini-4-argon"], "multipliers", 0, "when")
	gk := path(fx["grok-4.7"], "text", "input", "overrides", 0, "when")
	if ds == nil || ar == nil || gk == nil {
		t.Fatal("fixture conditions missing")
	}
	y := `models:
  - id: deepseek
    pricing:
      input_per_m: 0.3
      output_per_m: 1.2
      cache_read_per_m: 0.006
      multipliers: [{when: ` + jsonOf(t, ds) + `, factor: 2}]
  - id: argon
    pricing:
      input_per_m: 2
      output_per_m: 10
      cache_read_per_m: 0.1
      multipliers: [{when: ` + jsonOf(t, ar) + `, factor: 2}]
  - id: grok
    pricing:
      input_per_m: 2
      output_per_m: 6
      cache_read_per_m: 0.5
      overrides: [{when: ` + jsonOf(t, gk) + `, input_per_m: 4, output_per_m: 12, cache_read_per_m: 1}]
  - id: paid
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// at reads "YYYY-MM-DD HH:MM:SS" in UTC.
func at(s string) time.Time {
	t, err := time.Parse(time.DateTime, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestWhenFixtureConditions: the conditions of the real catalog match like
// the real one (2026-11-02 is a Monday).
func TestWhenFixtureConditions(t *testing.T) {
	c := fixtureConfig(t)
	ds := c.FindModel("deepseek").Pricing.Multipliers[0].When
	ar := c.FindModel("argon").Pricing.Multipliers[0].When
	gk := c.FindModel("grok").Pricing.Overrides[0].When
	for _, tc := range []struct {
		name string
		w    When
		at   string
		ctx  int
		want bool
	}{
		{"deepseek off-peak 00:59", ds, "2026-11-02 00:59:59", 0, false},
		{"deepseek peak 01:00", ds, "2026-11-02 01:00:00", 0, true},
		{"deepseek peak 03:59", ds, "2026-11-02 03:59:59", 0, true},
		{"deepseek 04:00 off-peak", ds, "2026-11-02 04:00:00", 0, false},
		{"deepseek 05:30 off-peak", ds, "2026-11-04 05:30:00", 0, false},
		{"deepseek second window 06:00", ds, "2026-11-06 06:00:00", 0, true},
		{"deepseek 10:00 off-peak", ds, "2026-11-06 10:00:00", 0, false},
		{"deepseek saturday 02:00", ds, "2026-11-07 02:00:00", 0, false},
		{"deepseek sunday 07:00", ds, "2026-11-08 07:00:00", 0, false},
		{"argon before the date", ar, "2026-11-30 23:59:59", 0, false},
		{"argon on the date", ar, "2026-12-01 00:00:00", 0, true},
		{"argon after the date", ar, "2027-01-15 12:00:00", 0, true},
		{"grok 200000", gk, "2026-11-02 12:00:00", 200000, false},
		{"grok 200001", gk, "2026-11-02 12:00:00", 200001, true},
	} {
		if got := tc.w.Matches(at(tc.at), tc.ctx); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// A time in another zone is read in UTC: 22:30 at UTC-3 is Tuesday
	// 01:30 UTC, the peak.
	sp := time.FixedZone("BRT", -3*3600)
	if !ds.Matches(time.Date(2026, 11, 2, 22, 30, 0, 0, sp), 0) {
		t.Error("deepseek must be read in UTC")
	}
	// The declared map is kept for the listing.
	if !reflect.DeepEqual(jsonRoundTrip(t, ds.Raw()), path(fixtureCosts(t)["deepseek-v4.1-flash"], "multipliers", 0, "when")) {
		t.Errorf("raw: %v", ds.Raw())
	}
}

// jsonRoundTrip normalizes a YAML-decoded value to its JSON decoding.
func jsonRoundTrip(t *testing.T, v any) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(jsonOf(t, v)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestPricingEffective(t *testing.T) {
	c := fixtureConfig(t)
	type prices [4]float64
	of := func(p Pricing) prices { return prices{p.InputPerM, p.OutputPerM, p.CacheReadPerM, p.CacheWritePerM} }
	for _, tc := range []struct {
		model string
		at    string
		ctx   int
		want  prices
	}{
		{"deepseek", "2026-11-02 00:30:00", 0, prices{0.3, 1.2, 0.006, 0}},
		{"deepseek", "2026-11-02 02:00:00", 0, prices{0.6, 2.4, 0.012, 0}},
		{"argon", "2026-11-30 12:00:00", 0, prices{2, 10, 0.1, 0}},
		{"argon", "2026-12-01 12:00:00", 0, prices{4, 20, 0.2, 0}},
		{"grok", "2026-11-02 12:00:00", 200000, prices{2, 6, 0.5, 0}},
		{"grok", "2026-11-02 12:00:00", 200001, prices{4, 12, 1, 0}},
		{"paid", "2026-11-02 12:00:00", 1 << 20, prices{}},
	} {
		got := c.FindModel(tc.model).Pricing.Effective(at(tc.at), tc.ctx)
		g := of(got)
		for i := range g {
			if !near(g[i], tc.want[i]) {
				t.Errorf("%s at %s ctx %d: %v, want %v", tc.model, tc.at, tc.ctx, g, tc.want)
				break
			}
		}
		if got.Multipliers != nil || got.Overrides != nil {
			t.Errorf("%s: effective keeps conditions", tc.model)
		}
	}
	// The first override that matches wins and replaces only the prices it
	// declares; then every matching multiplier scales the result.
	p, err := Parse([]byte(`models:
  - id: m
    pricing:
      input_per_m: 1
      output_per_m: 2
      cache_read_per_m: 0.1
      cache_write_per_m: 1.25
      overrides:
        - {when: {context: {gte: 1000}}, input_per_m: 3}
        - {when: {context: {gte: 10}}, input_per_m: 5, output_per_m: 7, cache_write_per_m: 2}
      multipliers:
        - {when: {utc_days: {in: [sat, sun]}}, factor: 2}
        - {when: {date: {lt: "2027-01-01"}}, factor: 1.5}
        - {when: {utc_hours: {gt: "23:00"}}, factor: 10}
`))
	if err != nil {
		t.Fatal(err)
	}
	pr := p.Models[0].Pricing
	for _, tc := range []struct {
		at   string
		ctx  int
		want prices
	}{
		{"2027-01-04 12:00:00", 5, prices{1, 2, 0.1, 1.25}},     // Monday, nothing matches
		{"2027-01-04 12:00:00", 50, prices{5, 7, 0.1, 2}},       // second override
		{"2027-01-04 12:00:00", 5000, prices{3, 2, 0.1, 1.25}},  // first override wins
		{"2026-11-07 12:00:00", 5000, prices{9, 6, 0.3, 3.75}},  // ×2 ×1.5
		{"2027-01-02 23:00:00", 0, prices{2, 4, 0.2, 2.5}},      // gt 23:00 is false at 23:00
		{"2027-01-02 23:00:01", 0, prices{20, 40, 2, 25}},       // ×2 ×10
		{"2026-12-31 23:30:00", 0, prices{15, 30, 1.5, 18.75}},  // Thursday: ×1.5 ×10
		{"2026-12-31 22:00:00", 0, prices{1.5, 3, 0.15, 1.875}}, // ×1.5
	} {
		g := of(pr.Effective(at(tc.at), tc.ctx))
		for i := range g {
			if !near(g[i], tc.want[i]) {
				t.Errorf("at %s ctx %d: %v, want %v", tc.at, tc.ctx, g, tc.want)
				break
			}
		}
	}
	// lte includes the bound.
	p, err = Parse([]byte(`models:
  - id: m
    pricing:
      input_per_m: 1
      overrides:
        - {when: {context: {lte: 100}}, input_per_m: 9}
`))
	if err != nil {
		t.Fatal(err)
	}
	pr = p.Models[0].Pricing
	for ctx, want := range map[int]float64{100: 9, 101: 1} {
		if got := pr.Effective(at("2027-01-04 12:00:00"), ctx).InputPerM; !near(got, want) {
			t.Errorf("lte 100 at ctx %d: input %v, want %v", ctx, got, want)
		}
	}
}

func TestPricingFree(t *testing.T) {
	c, err := Parse([]byte(`models:
  - {id: absent}
  - {id: empty, pricing: {}}
  - {id: zero, pricing: {input_per_m: 0, output_per_m: 0}}
  - {id: out-only, pricing: {output_per_m: 1}}
  - {id: paid, pricing: {input_per_m: 0.5}}
  - {id: "null", pricing: null}
`))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][2]bool{
		"absent": {false, false}, "empty": {true, true}, "zero": {true, true},
		"out-only": {true, true}, "paid": {true, false}, "null": {false, false},
	} {
		p := c.FindModel(id).Pricing
		if p.Declared() != want[0] || p.Free() != want[1] {
			t.Errorf("%s: declared %v free %v, want %v", id, p.Declared(), p.Free(), want)
		}
		if e := p.Effective(time.Now(), 0); e.Declared() != want[0] || e.Free() != want[1] {
			t.Errorf("%s effective: declared %v free %v", id, e.Declared(), e.Free())
		}
	}
	// Built in code: set fields declare; Declare marks a zero block.
	if (Pricing{}).Free() || !(Pricing{}).Declare().Free() || !(Pricing{InputPerM: 1}).Declared() {
		t.Error("code-built pricing")
	}
}

// roundTrip serializes c as GET /admin/config does (YAML → generic
// document → JSON) and parses it back as POST /admin/config does.
func roundTrip(t *testing.T, c *Config) *Config {
	t.Helper()
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatal(err)
	}
	y, err := yaml.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(y)
	if err != nil {
		t.Fatalf("reparse: %v\n%s", err, y)
	}
	return out
}

// TestPricingRoundTrip: the presence of pricing survives GET/POST
// /admin/config, a revalidation and a reload; a model without pricing never
// becomes free.
func TestPricingRoundTrip(t *testing.T) {
	src := []byte(`models:
  - {id: absent}
  - {id: free, pricing: {}}
  - id: peak
    pricing:
      input_per_m: 0.3
      multipliers: [{when: {utc_days: {in: [mon]}, any: [{utc_hours: {gte: "01:00", lt: "04:00"}}]}, factor: 2}]
      overrides: [{when: {context: {gt: 200000}}, output_per_m: 9}]
`)
	c, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "id: absent\n    pricing") || !strings.Contains(string(b), "multipliers:") {
		t.Errorf("serialized:\n%s", b)
	}
	for round, cfg := range []*Config{c, roundTrip(t, c), roundTrip(t, roundTrip(t, c))} {
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if p := cfg.FindModel("absent").Pricing; p.Declared() || p.Free() {
			t.Errorf("round %d: absent pricing became declared", round)
		}
		if !cfg.FindModel("free").Pricing.Free() {
			t.Errorf("round %d: free model lost its pricing", round)
		}
		p := cfg.FindModel("peak").Pricing
		if e := p.Effective(at("2026-11-02 02:00:00"), 300000); !near(e.InputPerM, 0.6) || !near(e.OutputPerM, 18) {
			t.Errorf("round %d: effective %+v", round, e)
		}
		if e := p.Effective(at("2026-11-03 02:00:00"), 0); !near(e.InputPerM, 0.3) {
			t.Errorf("round %d: tuesday %+v", round, e)
		}
	}
}

// TestNoNewFieldsSameMeaning: configurations without the ADR 0072 fields
// validate as before and keep no pricing where none was declared. The
// examples use the opencode options (examples/opencode-completo sets
// console_key) and the anthropic ones (examples/claude-code-assinatura sets
// oauth_key, oauth_plan and client_verification), so both declarations are
// registered.
func TestNoNewFieldsSameMeaning(t *testing.T) {
	withOpenCodeDecl(t)
	withFixtureDecl(t, "anthropic", map[string]func(string, *Config) error{"oauth_key": OptionKeyLabel})
	files, err := filepath.Glob("../../examples/*/config.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("examples: %v %v", files, err)
	}
	def, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range def.Models {
		if !m.Pricing.Declared() || m.Pricing.Free() || len(m.Catalogs) != 0 || !m.InCatalog("x") {
			t.Errorf("default %s: %+v", m.ID, m.Pricing)
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Parse(data)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		back := roundTrip(t, c)
		for i, m := range c.Models {
			b := back.Models[i]
			if m.Pricing.Declared() != b.Pricing.Declared() || m.Pricing.Free() != b.Pricing.Free() ||
				m.Pricing.InputPerM != b.Pricing.InputPerM || m.Pricing.OutputPerM != b.Pricing.OutputPerM {
				t.Errorf("%s %s: pricing %+v became %+v", f, m.ID, m.Pricing, b.Pricing)
			}
			if !strings.Contains(string(data), "pricing") && m.Pricing.Declared() && len(c.Raw()) > 0 {
				t.Errorf("%s %s: pricing declared without a pricing block", f, m.ID)
			}
		}
	}
}

func TestPricingValidation(t *testing.T) {
	model := func(pricing string) string { return "models: [{id: a}, {id: m, pricing: " + pricing + "}]" }
	for _, tc := range []struct{ yaml, err string }{
		{model(`{multipliers: [{when: {x: 1}, factor: 2}]}`), `config: models[1].pricing.multipliers[0].when: unknown condition "x"`},
		{model(`{multipliers: [{when: {any: [{all: [{utc_hours: {gte: "01:00"}}, {weekday: 1}]}]}, factor: 2}]}`), `config: models[1].pricing.multipliers[0].when.any[0].all[1]: unknown condition "weekday"`},
		{model(`{multipliers: [{factor: 2}]}`), `config: models[1].pricing.multipliers[0]: missing when`},
		{model(`{multipliers: [{when: {}, factor: 2}]}`), `multipliers[0]: missing when`},
		{model(`{multipliers: [{when: {utc_days: {in: [mon]}}}]}`), `multipliers[0].factor must be > 0 and finite, got 0`},
		{model(`{multipliers: [{when: {utc_days: {in: [mon]}}, factor: -1}]}`), `factor must be > 0 and finite, got -1`},
		{model(`{multipliers: [{when: {utc_days: {in: [mon]}}, factor: .inf}]}`), `factor must be > 0 and finite`},
		{model(`{multipliers: [{when: {context: {gt: 10}}, factor: 2}]}`), `multipliers[0].when: condition "context" is only allowed in overrides`},
		{model(`{multipliers: [{when: {utc_days: {in: [monday]}}, factor: 2}]}`), `when.utc_days.in: unknown day monday`},
		{model(`{multipliers: [{when: {utc_days: {in: []}}, factor: 2}]}`), `when.utc_days.in: want a non-empty list`},
		{model(`{multipliers: [{when: {utc_days: {not_in: [mon]}}, factor: 2}]}`), `when.utc_days: unknown operator "not_in"`},
		{model(`{multipliers: [{when: {utc_days: [mon]}, factor: 2}]}`), `when.utc_days: want {in: [days]}`},
		{model(`{multipliers: [{when: {utc_hours: {eq: "01:00"}}, factor: 2}]}`), `when.utc_hours: unknown operator "eq"`},
		{model(`{multipliers: [{when: {utc_hours: {gte: "25:00"}}, factor: 2}]}`), `when.utc_hours.gte: invalid time "25:00"`},
		{model(`{multipliers: [{when: {utc_hours: {gte: "1:00"}}, factor: 2}]}`), `when.utc_hours.gte: invalid time 1:00`},
		{model(`{multipliers: [{when: {utc_hours: {gte: "01:6a"}}, factor: 2}]}`), `invalid time 01:6a`},
		{model(`{multipliers: [{when: {utc_hours: {}}, factor: 2}]}`), `when.utc_hours: want {gte|gt|lte|lt: value}`},
		{model(`{multipliers: [{when: {date: {gte: "2026-13-01"}}, factor: 2}]}`), `when.date.gte: invalid date 2026-13-01`},
		{model(`{multipliers: [{when: {date: {gte: 20261201}}, factor: 2}]}`), `when.date.gte: invalid date`},
		{model(`{multipliers: [{when: {any: []}, factor: 2}]}`), `when.any: want a non-empty list of conditions`},
		{model(`{multipliers: [{when: {all: [mon]}, factor: 2}]}`), `when.all[0]: want a condition`},
		{model(`{multipliers: [{when: {all: [{}]}, factor: 2}]}`), `when.all[0]: empty condition`},
		{model(`{overrides: [{when: {context: {gt: 10}}}]}`), `config: models[1].pricing.overrides[0]: no price`},
		{model(`{overrides: [{input_per_m: 1}]}`), `config: models[1].pricing.overrides[0]: missing when`},
		{model(`{overrides: [{when: {context: {gt: -1}}, input_per_m: 1}]}`), `when.context.gt: invalid token count -1`},
		{model(`{overrides: [{when: {context: {gt: many}}, input_per_m: 1}]}`), `when.context.gt: invalid token count many`},
		{model(`{overrides: [{when: {any: [{context: {lte: 5}}, {x: 1}]}, input_per_m: 1}]}`), `overrides[0].when.any[1]: unknown condition "x"`},
		{model(`{bogus: 1}`), `unknown field "bogus"`},
		{model(`{multipliers: [{when: [utc_days], factor: 2}]}`), `config:`},
		{model(`{overrides: [{when: {context: {gt: 1}}, input_per_m: 1, bogus: 2}]}`), `unknown field "bogus"`},
		// Valid forms.
		{model(`{overrides: [{when: {context: {gte: 1.5e5}}, cache_write_per_m: 1}]}`), ""},
		{model(`{multipliers: [{when: {date: {gte: 2026-12-01, lt: "2027-01-01"}, utc_hours: {gte: "00:00", lte: "23:59"}}, factor: 0.5}]}`), ""},
		{model(`null`), ""},
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

// TestWhenUncompiled: a condition built without Validate compiles when it
// is first matched; an invalid one never matches.
func TestWhenUncompiled(t *testing.T) {
	ok := When{raw: map[string]any{"context": map[string]any{"gt": 10}}}
	bad := When{raw: map[string]any{"x": 1}}
	if !ok.Matches(time.Now(), 11) || ok.Matches(time.Now(), 10) || bad.Matches(time.Now(), 11) {
		t.Error("uncompiled conditions")
	}
	p := Pricing{InputPerM: 1, Overrides: []PriceOverride{{When: bad, InputPerM: new(float64)}}}
	if p.Effective(time.Now(), 0).InputPerM != 1 {
		t.Error("an invalid condition matched")
	}
	// Validating twice keeps the compiled condition.
	c, err := Parse([]byte(`models: [{id: m, pricing: {multipliers: [{when: {utc_days: {in: [mon]}}, factor: 2}]}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil || c.Models[0].Pricing.Multipliers[0].When.match == nil {
		t.Fatalf("revalidate: %v", err)
	}
	// A null block is no block.
	q := Pricing{InputPerM: 1}.Declare()
	if err := q.UnmarshalYAML([]byte(" null ")); err != nil || q.Declared() {
		t.Errorf("null pricing: %v %+v", err, q)
	}
	if (When{}).Raw() != nil {
		t.Error("absent raw")
	}
}
