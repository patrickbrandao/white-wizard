package config

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// withOpenCodeDecl registers, for the test, the opencode declaration of the
// fixture with what tasks 422–424 add to it (ADR 0072): the two catalogs,
// the plans, the public key and the open options. A second test driver
// declares other plans, so the accepted plans are the union.
// withFixtureDecl registers the declaration of driver name from
// testdata/driver_decls.json (the golden of TestDeclarationsMatchConfigFixture)
// for the test, with checks giving back the DriverOption.Check the JSON
// leaves out; the previous registry comes back on cleanup.
func withFixtureDecl(t *testing.T, name string, checks map[string]func(string, *Config) error) {
	t.Helper()
	saved := decls.Load()
	t.Cleanup(func() { decls.Store(saved) })
	b, err := os.ReadFile("testdata/driver_decls.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]DriverDecl
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	d, ok := fixture[name]
	if !ok {
		t.Fatalf("driver %q not in testdata/driver_decls.json", name)
	}
	d.Options = slices.Clone(d.Options)
	for i := range d.Options {
		d.Options[i].Check = checks[d.Options[i].Name]
	}
	RegisterDriver(name, func() (DriverDecl, bool) { return d, true })
}

func withOpenCodeDecl(t *testing.T) {
	t.Helper()
	saved := decls.Load()
	t.Cleanup(func() { decls.Store(saved) })
	b, err := os.ReadFile("testdata/driver_decls.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]DriverDecl
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	oc := fixture["opencode"]
	oc.Catalogs = []CatalogDecl{{Name: "opencode"}, {Name: "opencode-go", Allowance: true}}
	oc.Plans = []string{"go", "go-plus", "go-teams", "none"}
	oc.ReservedKeys = []string{"public"}
	oc.Options = append(slices.Clone(oc.Options),
		DriverOption{Name: "free_requests_per_day", Default: "200", Check: OptionPositiveInt},
		DriverOption{Name: "console_key", Check: OptionKeyLabel},
	)
	RegisterDriver("opencode", func() (DriverDecl, bool) { return oc, true })
	RegisterDriver("wwz-plans", func() (DriverDecl, bool) {
		return DriverDecl{Dialects: []string{DialectOpenAI}, Plans: []string{"go", "pro"}}, true
	})
}

// openCodeFieldsYAML exercises every field of ADR 0072 items 6 and 9–14 at
// every level.
const openCodeFieldsYAML = `
drivers:
  - name: opencode
    options: {free_requests_per_day: "200", console_key: ci}
  - name: openai
keys:
  - {key: oc_sk_ana, account: acme, project: ana, limits: {spend_monthly_models_usd: {m1: 1}}}
  - {key: oc_sk_ci, label: ci, account: acme, service_account: true}
accounts:
  - id: acme
    tier: go
    extra_usage: false
    limits:
      spend_monthly_models_usd: {opencode/m1: 10, opencode-go/m1: 4, opencode/paygo-only: 2}
projects:
  - id: ana
    limits: {quota_usd: {models: {m1: 3}}}
tiers:
  go:
    plan: go-teams
    limits:
      quota_usd: {rolling_5h_pct: 20, weekly_pct: 50, models: {m1: 100, opencode-go/m1: 60}}
  pro:
    plan: pro
  bare: {}
models:
  - id: m1
    drivers: [opencode]
  - id: paygo-only
    drivers: [opencode]
    catalogs: {opencode: {}}
  - id: direct
    drivers: [openai]
  - id: vendor/slash
    drivers: [openai]
`

func TestOpenCodeFieldsParse(t *testing.T) {
	withOpenCodeDecl(t)
	c, err := Parse([]byte(openCodeFieldsYAML))
	if err != nil {
		t.Fatal(err)
	}
	for round, cfg := range []*Config{c, roundTrip(t, c), roundTrip(t, roundTrip(t, c))} {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if p := cfg.Tiers["go"].Plan; p != "go-teams" {
			t.Errorf("round %d: plan %q", round, p)
		}
		if cfg.Tiers["bare"].Plan != "" {
			t.Errorf("round %d: bare plan %q", round, cfg.Tiers["bare"].Plan)
		}
		if e := cfg.Accounts[0].ExtraUsage; e == nil || *e {
			t.Errorf("round %d: extra_usage false lost: %v", round, e)
		}
		if !cfg.Keys[1].ServiceAccount || cfg.Keys[0].ServiceAccount {
			t.Errorf("round %d: service_account %v %v", round, cfg.Keys[0].ServiceAccount, cfg.Keys[1].ServiceAccount)
		}
		e := cfg.LimitsFor(&cfg.Keys[0], "")
		if q := e.Account.Limits.QuotaUSD; q == nil || !reflect.DeepEqual(q.Models, map[string]float64{"m1": 100, "opencode-go/m1": 60}) || q.Rolling5hPct != 20 {
			t.Errorf("round %d: account quota %+v", round, q)
		}
		if got := e.Account.Limits.SpendMonthlyModelsUSD; !reflect.DeepEqual(got, map[string]float64{"opencode/m1": 10, "opencode-go/m1": 4, "opencode/paygo-only": 2}) {
			t.Errorf("round %d: account budgets %v", round, got)
		}
		if q := e.Project.Limits.QuotaUSD; q == nil || q.Models["m1"] != 3 {
			t.Errorf("round %d: project quota %+v", round, q)
		}
		if got := e.Key.Limits.SpendMonthlyModelsUSD; got["m1"] != 1 {
			t.Errorf("round %d: key budgets %v", round, got)
		}
	}
	// An extra_usage left out is nil: the driver decides by plan.
	c.Accounts[0].ExtraUsage = nil
	if b, err := yaml.Marshal(c); err != nil || strings.Contains(string(b), "extra_usage") || strings.Contains(string(b), "service_account: false") {
		t.Errorf("serialized: %v\n%s", err, b)
	}
	// The public key is free while no driver that reserves it is mounted.
	if _, err := Parse([]byte("drivers: [{name: openai}]\nkeys: [{key: public}]\n")); err != nil {
		t.Errorf("public without opencode: %v", err)
	}
}

// TestOpenCodeFieldsRejected: every refusal of spec 003 names its path, at
// boot (Parse), on reload (the YAML of the configuration parsed again, as
// SIGHUP and POST /admin/config do) and in wwz validate (Validate).
func TestOpenCodeFieldsRejected(t *testing.T) {
	withOpenCodeDecl(t)
	for _, tc := range []struct {
		name string
		edit func(c *Config)
		want string
	}{
		{"public key", func(c *Config) { c.Keys[0].Key = "public" }, `config: keys[0].key "public" is reserved by the opencode driver`},
		{"plan", func(c *Config) { c.Tiers["bare"] = Tier{Plan: "gold"} }, `config: tiers.bare.plan: unknown plan "gold" (want go, go-plus, go-teams, none, pro)`},
		{"quota unknown model", func(c *Config) { setQuotaModel(c, "nope", 1) }, `config: tiers.go.limits.quota_usd.models: unknown model "nope"`},
		{"quota other prefix", func(c *Config) { setQuotaModel(c, "zen/m1", 1) }, `config: tiers.go.limits.quota_usd.models: unknown model "zen/m1"`},
		{"quota outside catalog", func(c *Config) { setQuotaModel(c, "opencode-go/paygo-only", 1) }, `config: tiers.go.limits.quota_usd.models: unknown model "opencode-go/paygo-only"`},
		{"quota model not on the catalog driver", func(c *Config) { setQuotaModel(c, "opencode/direct", 1) }, `config: tiers.go.limits.quota_usd.models: unknown model "opencode/direct"`},
		{"quota alias is not an id", func(c *Config) { c.Models[0].Aliases = []string{"m1-alias"}; setQuotaModel(c, "m1-alias", 1) }, `config: tiers.go.limits.quota_usd.models: unknown model "m1-alias"`},
		{"quota negative", func(c *Config) { setQuotaModel(c, "m1", -1) }, `config: tiers.go.limits.quota_usd.models["m1"] must not be negative, got -1`},
		{"account budget unknown", func(c *Config) { c.Accounts[0].Limits.SpendMonthlyModelsUSD["opencode/nope"] = 1 }, `config: accounts[0].limits.spend_monthly_models_usd: unknown model "opencode/nope"`},
		{"account budget negative", func(c *Config) { c.Accounts[0].Limits.SpendMonthlyModelsUSD["opencode/m1"] = -2 }, `config: accounts[0].limits.spend_monthly_models_usd["opencode/m1"] must not be negative, got -2`},
		{"project quota", func(c *Config) { c.Projects[0].Limits.QuotaUSD.Models = map[string]float64{"x": 1} }, `config: projects[0].limits.quota_usd.models: unknown model "x"`},
		{"key budget", func(c *Config) { c.Keys[0].Limits.SpendMonthlyModelsUSD = map[string]float64{"opencode-go/x": 1} }, `config: keys[0].limits.spend_monthly_models_usd: unknown model "opencode-go/x"`},
		{"tier budget", func(c *Config) {
			l := c.Tiers["bare"]
			l.Limits.SpendMonthlyModelsUSD = map[string]float64{"m1": -0.5}
			c.Tiers["bare"] = l
		}, `config: tiers.bare.limits.spend_monthly_models_usd["m1"] must not be negative, got -0.5`},
		{"free requests zero", func(c *Config) { c.Drivers[0].Options["free_requests_per_day"] = "0" }, `config: drivers[0] (opencode): option free_requests_per_day="0" is invalid: want an integer >= 1`},
		{"free requests text", func(c *Config) { c.Drivers[0].Options["free_requests_per_day"] = "many" }, `config: drivers[0] (opencode): option free_requests_per_day="many" is invalid: want an integer >= 1`},
		{"console key", func(c *Config) { c.Drivers[0].Options["console_key"] = "nobody" }, `config: drivers[0] (opencode): option console_key="nobody" is invalid: not the label of a keys[] entry`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(openCodeFieldsYAML))
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(c)
			if err := c.Validate(); err == nil || err.Error() != tc.want {
				t.Errorf("validate: got %v, want %s", err, tc.want)
			}
			b, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(b); err == nil || err.Error() != tc.want {
				t.Errorf("boot/reload: got %v, want %s", err, tc.want)
			}
		})
	}
}

func setQuotaModel(c *Config, k string, v float64) {
	c.Tiers["go"].Limits.QuotaUSD.Models[k] = v
}

// TestPlanDeclared: the opencode driver declares the plans (task 422): an
// empty plan and a declared one are valid, any other is refused.
func TestPlanDeclared(t *testing.T) {
	for _, p := range []string{"", "go", "go-plus", "go-teams", "none"} {
		if err := validatePlan("t", p); err != nil {
			t.Fatalf("%q: %v", p, err)
		}
	}
	if err := validatePlan("t", "gold"); err == nil || err.Error() != `config: tiers.t.plan: unknown plan "gold" (want go, go-plus, go-teams, none)` {
		t.Fatal(err)
	}
}

// TestModelKeyKnown: an id with "/" is matched whole before the catalog
// prefix is tried.
func TestModelKeyKnown(t *testing.T) {
	withOpenCodeDecl(t)
	c, err := Parse([]byte(openCodeFieldsYAML))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{
		"m1": true, "opencode/m1": true, "opencode-go/m1": true, "vendor/slash": true,
		"opencode/paygo-only": true, "paygo-only": true, "direct": true,
		"opencode-go/paygo-only": false, "opencode/direct": false, "vendor/m1": false, "opencode/": false, "": false,
	} {
		if got := c.ModelKeyKnown(k); got != want {
			t.Errorf("ModelKeyKnown(%q) = %v", k, got)
		}
	}
}

// TestDriverOptionCheck: Check runs after the closed values, and an open
// option (Check without Values) skips the closed-value test.
func TestDriverOptionCheck(t *testing.T) {
	saved := decls.Load()
	t.Cleanup(func() { decls.Store(saved) })
	errOdd := errors.New("odd")
	RegisterDriver("wwz-check", func() (DriverDecl, bool) {
		return DriverDecl{Options: []DriverOption{
			{Name: "open", Check: OptionPositiveInt},
			{Name: "both", Values: []string{"1", "2"}, Check: func(v string, _ *Config) error {
				if v == "1" {
					return errOdd
				}
				return nil
			}},
		}}, true
	})
	c := &Config{Models: []ModelConfig{{ID: "m"}}}
	for opts, want := range map[string]string{
		"open=7":  "",
		"open=-1": `config: drivers[0] (wwz-check): option open="-1" is invalid: want an integer >= 1`,
		"both=2":  "",
		"both=3":  `config: drivers[0] (wwz-check): option both="3" is invalid (allowed: [1 2])`,
		"both=1":  `config: drivers[0] (wwz-check): option both="1" is invalid: odd`,
	} {
		k, v, _ := strings.Cut(opts, "=")
		c.Drivers = []DriverMount{{Name: "wwz-check", Options: map[string]string{k: v}}}
		err := c.Validate()
		if (want == "" && err != nil) || (want != "" && (err == nil || err.Error() != want)) {
			t.Errorf("%s: got %v, want %q", opts, err, want)
		}
	}
	if vals, ok := driverOptionValues("wwz-check"); !ok || vals["open"] != nil || len(vals["both"]) != 2 {
		t.Errorf("values: %v %v", vals, ok)
	}
	if vals, ok := driverOptionValues("wwz-unregistered"); ok || vals != nil {
		t.Errorf("unregistered: %v %v", vals, ok)
	}
	if err := OptionKeyLabel("x", nil); err == nil {
		t.Error("label without configuration accepted")
	}
}

// TestSpec003OpenCodeExample: the OpenCode block of spec 003 validates with
// test drivers that declare the catalogs and plans. The block only declares
// the models it prices; the ones it only names in budgets and quotas come
// from the rest of the catalog and are added here.
func TestSpec003OpenCodeExample(t *testing.T) {
	withOpenCodeDecl(t)
	data, err := os.ReadFile("../../specs/003-configuracao.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	start := strings.Index(doc, "```yaml\n# OpenCode (ADR 0072)")
	if start < 0 {
		t.Fatal("no OpenCode yaml block in spec 003")
	}
	body := doc[start+len("```yaml\n"):]
	body = body[:strings.Index(body, "\n```")]
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	models, _ := raw["models"].([]any)
	declared := map[string]bool{}
	for _, m := range models {
		declared[m.(map[string]any)["id"].(string)] = true
	}
	for _, id := range referencedModels(raw) {
		if !declared[id] {
			models = append(models, map[string]any{"id": id, "drivers": []any{"opencode"}})
			declared[id] = true
		}
	}
	raw["models"] = models
	b, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatalf("spec 003 OpenCode example: %v\n%s", err, b)
	}
	if c.Tiers["go-teams"].Plan != "go-teams" || c.Tiers["payg"].Plan != "none" || !*c.Accounts[0].ExtraUsage || !c.Keys[1].ServiceAccount {
		t.Errorf("example fields lost: %+v %+v", c.Tiers, c.Accounts[0])
	}
	if c.Drivers[0].Options["console_key"] != c.Keys[0].Label {
		t.Errorf("console_key %q", c.Drivers[0].Options["console_key"])
	}
}

// referencedModels are the model ids (catalog prefix removed) the budgets
// and quotas of tiers and accounts of a raw configuration name.
func referencedModels(raw map[string]any) []string {
	var limits []map[string]any
	if tiers, ok := raw["tiers"].(map[string]any); ok {
		for _, t := range tiers {
			if l, ok := t.(map[string]any)["limits"].(map[string]any); ok {
				limits = append(limits, l)
			}
		}
	}
	if accts, ok := raw["accounts"].([]any); ok {
		for _, a := range accts {
			if l, ok := a.(map[string]any)["limits"].(map[string]any); ok {
				limits = append(limits, l)
			}
		}
	}
	var out []string
	add := func(m any) {
		mm, _ := m.(map[string]any)
		for k := range mm {
			if _, id, ok := strings.Cut(k, "/"); ok {
				k = id
			}
			out = append(out, k)
		}
	}
	for _, l := range limits {
		add(l["spend_monthly_models_usd"])
		if q, ok := l["quota_usd"].(map[string]any); ok {
			add(q["models"])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// spec003Block is the yaml block of spec 003 whose first line is head.
func spec003Block(t *testing.T, head string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../specs/003-configuracao.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	start := strings.Index(doc, "```yaml\n"+head+"\n")
	if start < 0 {
		t.Fatalf("no %q yaml block in spec 003", head)
	}
	body := doc[start+len("```yaml\n"):]
	return []byte(body[:strings.Index(body, "\n```")])
}

// TestSpec003OpenCodeCatalogFields: the "campos por catálogo" block of spec
// 003 (revision of task 434) passes the strict Parse and gives, in each
// catalog, the effective model it describes.
func TestSpec003OpenCodeCatalogFields(t *testing.T) {
	withOpenCodeDecl(t)
	c, err := Parse(spec003Block(t, "# OpenCode: campos por catálogo (ADR 0072, revisão da task 434)"))
	if err != nil {
		t.Fatalf("spec 003 catalog fields: %v", err)
	}
	mm := c.FindModel("minimax-m3")
	payg, gov := mm.ForCatalog("opencode"), mm.ForCatalog("opencode-go")
	if payg != mm || payg.ContextWindow != 512000 || payg.MaxOutput != 128000 {
		t.Errorf("minimax-m3 pay-as-you-go: %d/%d", payg.ContextWindow, payg.MaxOutput)
	}
	if gov.ContextWindow != 1000000 || gov.MaxOutput != 131072 || len(gov.Pricing.Overrides) != 1 || gov.Extra["family"] != "minimax-m3" {
		t.Errorf("minimax-m3 Go: %+v", gov)
	}
	if caps, _ := gov.Extra["capabilities"].(map[string]any); caps == nil || caps["cache"] != nil {
		t.Errorf("minimax-m3 Go capabilities %v", gov.Extra["capabilities"])
	} else if _, kept := caps["cache"]; !kept {
		t.Error("minimax-m3 Go: the null of capabilities.cache was dropped")
	}
	ds := c.FindModel("deepseek-v4-pro").ForCatalog("opencode-go")
	if ds.Extra["name"] != "DeepSeek V4 Pro (New)" || ds.Pricing.InputPerM != 0.66 || len(ds.Pricing.Multipliers) != 1 {
		t.Errorf("deepseek-v4-pro Go: %+v", ds)
	}
	if r, _ := ds.Extra["capabilities"].(map[string]any)["reasoning"].(map[string]any); r == nil || r["toggle"] != nil || len(r["effort"].([]any)) != 2 {
		t.Errorf("deepseek-v4-pro Go reasoning %v", r)
	}
}
