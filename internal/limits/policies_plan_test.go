package limits

import (
	"strings"
	"sync"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

var planDriverOnce sync.Once

// TestPoliciesModelLimitsAndPlan: tier:<name> carries quota_usd.models and
// spend_monthly_models_usd; the plan of the YAML tier stays, and a policy
// tier cannot set one (ADR 0072).
func TestPoliciesModelLimitsAndPlan(t *testing.T) {
	planDriverOnce.Do(func() {
		config.RegisterDriver("wwz-plans-limits", func() (config.DriverDecl, bool) {
			return config.DriverDecl{Plans: []string{"go-teams"}}, true
		})
	})
	c, err := config.Parse([]byte(`
models: [{id: m1}]
tiers:
  go: {plan: go-teams, next: max, promote_when: {requests: 5}, limits: {rpm: 1}}
  max: {}
keys: [{key: sk-a, account: acme, tier: go}]
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePolicies(map[string]string{
		"version":  "3",
		"tier:go":  `{"rpm": 7, "quota_usd": {"weekly_pct": 50, "models": {"m1": 20}}, "spend_monthly_models_usd": {"m1": 4}}`,
		"tier:new": `{"spend_monthly_models_usd": {"m1": 1}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.apply(c)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Tiers["go"]
	if g.Plan != "go-teams" || g.Next != "max" || g.PromoteWhen == nil || g.Limits.RPM != 7 {
		t.Fatalf("tier go: %+v", g)
	}
	l := c.LimitsFor(&c.Keys[0], "").Account.Limits
	if l.QuotaUSD == nil || l.QuotaUSD.Models["m1"] != 20 || l.SpendMonthlyModelsUSD["m1"] != 4 {
		t.Fatalf("account limits: %+v", l)
	}
	if c.Tiers["new"].Plan != "" || c.Tiers["new"].Limits.SpendMonthlyModelsUSD["m1"] != 1 {
		t.Fatalf("tier new: %+v", c.Tiers["new"])
	}
	a.restore(c)
	if g := c.Tiers["go"]; g.Plan != "go-teams" || g.Limits.RPM != 1 || g.Limits.SpendMonthlyModelsUSD != nil {
		t.Fatalf("restored: %+v", g)
	}

	// plan is not a limit; an unknown model in a policy tier is refused
	// and leaves the configuration as it was.
	if _, err := ParsePolicies(map[string]string{"tier:go": `{"plan": "none"}`}); err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("plan in policies: %v", err)
	}
	bad, err := ParsePolicies(map[string]string{"version": "4", "tier:go": `{"quota_usd": {"models": {"nope": 1}}}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.apply(c); err == nil || !strings.Contains(err.Error(), `tiers.go.limits.quota_usd.models: unknown model "nope"`) {
		t.Fatalf("unknown model: %v", err)
	}
	if c.Tiers["go"].Limits.RPM != 1 || c.Tiers["go"].Plan != "go-teams" {
		t.Fatalf("refused version changed the tier: %+v", c.Tiers["go"])
	}
}
