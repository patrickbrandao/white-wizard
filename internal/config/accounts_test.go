package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// accountsYAML is the accounts/projects/tiers part of the spec 003 example.
const accountsYAML = `
keys:
  - key: sk-test-fast
    label: provedor-rapido
    limits: { rpm: 60000, tpm: 5000000, concurrency: 5000 }
  - key: sk-test-limited
    label: provedor-com-cota
    limits: { rpm: 300, tpm: 40000, concurrency: 20 }
  - key: sk-acme-a
    label: acme-a
    account: acme
    project: acme-web
  - key: sk-acme-b
    label: acme-b
    account: acme

accounts:
  - id: acme
    tier: tier-1

projects:
  - id: acme-web
    limits: { tpm: 50000 }

tiers:
  tier-1:
    limits: { rpm: 500, tpm: 200000, spend_monthly_usd: 100 }
    promote_when: { purchased_usd: 50, age_days: 7 }
    next: tier-2
  tier-2:
    limits: { rpm: 5000, tpm: 2000000, spend_monthly_usd: 500 }
`

func TestAccountsResolver(t *testing.T) {
	c, err := Parse([]byte(accountsYAML))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	// Keys without tier take the tier of their account.
	a, b := c.FindKey("sk-acme-a"), c.FindKey("sk-acme-b")
	if a.Tier != "tier-1" || b.Tier != "tier-1" || c.FindKey("sk-test-fast").Tier != "default" {
		t.Fatalf("key tiers: %q %q", a.Tier, b.Tier)
	}
	e := c.LimitsFor(a, "")
	if e.Account.ID != "acme" || e.Account.Tier != "tier-1" || e.Account.Limits.RPM != 500 || e.Account.Limits.SpendMonthlyUSD != 100 {
		t.Fatalf("account: %+v", e.Account)
	}
	if e.Project.ID != "acme-web" || e.Project.Limits.TPM != 50000 || e.Project.Limits.RPM != 0 {
		t.Fatalf("project: %+v", e.Project)
	}
	if e.Key.ID != "acme-a" || !e.Key.Limits.IsZero() {
		t.Fatalf("key: %+v", e.Key)
	}
	// Both keys of acme resolve to the same account level.
	if eb := c.LimitsFor(b, ""); !reflect.DeepEqual(eb.Account, e.Account) || eb.Project.ID != "" || !eb.Project.Limits.IsZero() {
		t.Fatalf("acme-b: %+v", eb)
	}
	// A tier assigned by the cluster (cfg:policies, promotion) wins over
	// the configured one.
	if e := c.LimitsFor(a, "tier-2"); e.Account.Tier != "tier-2" || e.Account.Limits.RPM != 5000 {
		t.Fatalf("assigned tier: %+v", e.Account)
	}
	// keys[].limits is a ceiling of the key only; the implicit account of
	// the key gets the default tier (no limits).
	f := c.LimitsFor(c.FindKey("sk-test-fast"), "")
	if f.Key.Limits.RPM != 60000 || f.Account.ID != "acct-provedor-rapido" || !f.Account.Limits.IsZero() || f.Account.Tier != "default" {
		t.Fatalf("fast: %+v", f)
	}
	if e := c.LimitsFor(nil, "x"); !reflect.DeepEqual(e, EffectiveLimits{}) {
		t.Fatalf("nil key: %+v", e)
	}
	if e := c.LimitsFor(&KeyConfig{Label: "bare"}, ""); e.Account.ID != "" || e.Key.ID != "bare" {
		t.Fatalf("key without account: %+v", e)
	}
	if !c.ProjectLimits("nope").IsZero() || c.FindProject("nope") != nil || c.FindAccount("nope") != nil {
		t.Fatal("unknown project/account")
	}
	if !KeyLimits(nil).IsZero() {
		t.Fatal("nil key limits")
	}
}

func TestAccountLimitsPrecedence(t *testing.T) {
	c, err := Parse([]byte(`
keys:
  - {key: sk-1, label: k1, account: a1, tier: pro}
  - {key: sk-2, label: k2, account: a1}
  - {key: sk-3, label: k3, account: a2}
accounts:
  - {id: a2, limits: {rpm: 7, window: {every: 1m, duration: 30s}, mode: window}}
  - {id: a3}
projects:
  - {id: p9, limits: {rpm: 1}}
  - {id: p8}
tiers:
  pro: {limits: {rpm: 100, tpm: 1000}}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Tier from the first key that declares one; the other key follows.
	if c.AccountTier("a1") != "pro" || c.FindKey("sk-2").Tier != "pro" || c.AccountTier("zz") != "default" {
		t.Fatalf("account tier: %q %q", c.AccountTier("a1"), c.FindKey("sk-2").Tier)
	}
	// accounts[].limits over the tier, field by field (window included).
	l := c.AccountLimits("a2", "pro")
	if l.RPM != 7 || l.TPM != 1000 || l.Window == nil || l.Window.Every != time.Minute || l.Mode != "window" {
		t.Fatalf("a2 over pro: %+v", l)
	}
	if c.AccountLimits("a1", "").TPM != 1000 {
		t.Fatal("configured tier")
	}
	if l := c.AccountLimits("a2", "missing"); l.RPM != 7 || l.TPM != 0 {
		t.Fatalf("unknown tier: %+v", l)
	}
	if !c.ProjectLimits("p8").IsZero() || c.ProjectLimits("p9").RPM != 1 {
		t.Fatal("project limits")
	}
	w := c.Warnings()
	if len(w) != 3 || !strings.Contains(w[0], `accounts[1] "a3"`) || !strings.Contains(w[1], `projects[0] "p9"`) || !strings.Contains(w[2], `projects[1] "p8"`) {
		t.Fatalf("warnings: %v", w)
	}
}

func TestPromotion(t *testing.T) {
	c, err := Parse([]byte(accountsYAML + `
  tier-3:
    promote_when: { spend_usd: 10, requests: 5 }
    next: tier-2
`))
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	cases := []struct {
		tier string
		u    PromotionUsage
		next string
	}{
		{"tier-1", PromotionUsage{PurchasedUSD: 50, Age: 7 * day}, "tier-2"},
		{"tier-1", PromotionUsage{PurchasedUSD: 49.99, Age: 8 * day}, ""},
		{"tier-1", PromotionUsage{PurchasedUSD: 500, Age: 7*day - time.Second}, ""},
		{"tier-3", PromotionUsage{SpentUSD: 10, Requests: 5}, "tier-2"},
		{"tier-3", PromotionUsage{SpentUSD: 10, Requests: 4}, ""},
		{"tier-3", PromotionUsage{SpentUSD: 9, Requests: 50}, ""},
		{"tier-2", PromotionUsage{PurchasedUSD: 1e9}, ""}, // no promote_when
		{"nope", PromotionUsage{}, ""},
	}
	for _, tc := range cases {
		next, ok := c.Promotion(tc.tier, tc.u)
		if next != tc.next || ok != (tc.next != "") {
			t.Errorf("%s %+v: got %q %v, want %q", tc.tier, tc.u, next, ok, tc.next)
		}
	}
	// next without promote_when is valid (manual chain) and never promotes.
	c.Tiers["tier-2"] = Tier{Next: "tier-1"}
	c.Tiers["tier-1"] = Tier{Next: ""}
	if _, ok := c.Promotion("tier-2", PromotionUsage{}); ok {
		t.Fatal("next without promote_when promoted")
	}
}

func TestAccountsValidation(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"account id empty", "accounts: [{tier: x}]", "accounts[0].id is required"},
		{"account id duplicated", "accounts: [{id: a}, {id: a}]", `accounts[1]: duplicate account id "a"`},
		{"account tier name", "accounts: [{id: a, tier: 'bad tier'}]", `accounts[0].tier: invalid tier name "bad tier"`},
		{"account limits", "accounts: [{id: a, limits: {rpm: -1}}]", "accounts[0].limits.rpm must not be negative"},
		{"project id empty", "projects: [{limits: {rpm: 1}}]", "projects[0].id is required"},
		{"project id duplicated", "projects: [{id: p}, {id: p}]", `projects[1]: duplicate project id "p"`},
		{"project limits", "projects: [{id: p, limits: {tpm: -2}}]", "projects[0].limits.tpm must not be negative"},
		{
			"keys of one account disagree",
			"keys: [{key: k1, account: a, tier: t1}, {key: k2, account: a, tier: t2}]\ntiers: {t1: {}, t2: {}}",
			`keys[1].tier "t2" conflicts with account "a" tier "t1"`,
		},
		{
			"key disagrees with accounts[].tier",
			"keys: [{key: k1, account: a, tier: t2}]\naccounts: [{id: a, tier: t1}]",
			`keys[0].tier "t2" conflicts with account "a" tier "t1"`,
		},
		{"next unknown", "tiers: {t1: {next: t9}}", `tiers.t1.next: unknown tier "t9"`},
		{"promote_when without next", "tiers: {t1: {promote_when: {requests: 1}}}", "tiers.t1.promote_when requires next"},
		{"promote_when empty", "tiers: {t1: {promote_when: {}, next: default}}", "tiers.t1.promote_when needs at least one condition > 0"},
		{"promote_when purchased", "tiers: {t1: {promote_when: {purchased_usd: -1}, next: default}}", "tiers.t1.promote_when.purchased_usd must not be negative"},
		{"promote_when spend", "tiers: {t1: {promote_when: {spend_usd: -1}, next: default}}", "tiers.t1.promote_when.spend_usd must not be negative"},
		{"promote_when age", "tiers: {t1: {promote_when: {age_days: -1}, next: default}}", "tiers.t1.promote_when.age_days must not be negative"},
		{"promote_when requests", "tiers: {t1: {promote_when: {requests: -1}, next: default}}", "tiers.t1.promote_when.requests must not be negative"},
		{"self cycle", "tiers: {t1: {next: t1}}", "tiers.t1: cycle in next (t1 -> t1)"},
		{"cycle", "tiers: {a: {next: b}, b: {next: c}, c: {next: a}}", "tiers.a: cycle in next (a -> b -> c -> a)"},
		{"cycle reached from outside", "tiers: {a: {next: b}, b: {next: c}, c: {next: b}}", "tiers.b: cycle in next (b -> c -> b)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	// Validate on a configuration without defaults: keys without account
	// or tier are not compared.
	c := &Config{Models: []ModelConfig{{ID: "m"}}, Keys: []KeyConfig{{Key: "a", Tier: "x"}, {Key: "b", Account: "z"}, {Key: "c", Account: "z", Tier: "y"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// The built-in keys share acct-test on the default tier and stay valid.
	if _, err := Parse(nil); err != nil {
		t.Fatal(err)
	}
}

// TestMergeModelLimits: a model map with entries at the account replaces
// the tier's whole map, like quota_usd; an empty or absent one keeps it
// (ADR 0072). IsZero sees a map with entries.
func TestMergeModelLimits(t *testing.T) {
	tier := Limits{
		RPM:                   10,
		SpendMonthlyModelsUSD: map[string]float64{"a": 1, "b": 2},
		QuotaUSD:              &QuotaUSD{WeeklyPct: 50, Models: map[string]float64{"a": 5}},
	}
	got := mergeLimits(tier, Limits{SpendMonthlyModelsUSD: map[string]float64{"b": 9}, QuotaUSD: &QuotaUSD{Models: map[string]float64{"c": 1}}})
	if !reflect.DeepEqual(got.SpendMonthlyModelsUSD, map[string]float64{"b": 9}) || got.QuotaUSD.WeeklyPct != 0 || got.QuotaUSD.Models["c"] != 1 || got.RPM != 10 {
		t.Errorf("replaced: %+v %+v", got, got.QuotaUSD)
	}
	if got := mergeLimits(tier, Limits{SpendMonthlyModelsUSD: map[string]float64{}}); !reflect.DeepEqual(got, tier) {
		t.Errorf("empty map changed the tier: %+v", got)
	}
	if !(Limits{SpendMonthlyModelsUSD: map[string]float64{}}).IsZero() || (Limits{SpendMonthlyModelsUSD: map[string]float64{"a": 0}}).IsZero() || (Limits{QuotaUSD: &QuotaUSD{}}).IsZero() {
		t.Error("IsZero with model maps")
	}
}
