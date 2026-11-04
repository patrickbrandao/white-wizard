package engine

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// deploymentPolicyDriver limits per deployment without shaping the limits.
type deploymentPolicyDriver struct{ driver.Driver }

func (deploymentPolicyDriver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"key", "account", "deployment"}, TPMReserve: "max_tokens"}
}

// deploymentLimitsDriver also shapes them (Azure's DeploymentLimits).
type deploymentLimitsDriver struct{ deploymentPolicyDriver }

func (deploymentLimitsDriver) DeploymentLimits(_ *config.ModelConfig, l config.Limits) (config.Limits, time.Duration) {
	l.RPM = l.TPM / 100
	return l, 10 * time.Second
}

func deploymentCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
tiers:
  big: {limits: {rpm: 100, tpm: 5000, spend_monthly_usd: 10, credits_usd: 3}}
keys:
  - {key: k1, label: a1, account: acme, tier: big, limits: {rpm: 2}}
  - {key: k2, label: solo}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Task 295 (spec 006 16.6): the tier's count limits go to the deployment
// "<account>/<model>", the account keeps the dollar limits and the key its
// ceiling; the driver shapes the deployment's limits and window.
func TestSubjectsDeployment(t *testing.T) {
	cfg := deploymentCfg(t)
	base, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	model := &config.ModelConfig{ID: "gpt-x"}
	for _, c := range []struct {
		drv    driver.Driver
		rpm    int
		window time.Duration
	}{
		{deploymentPolicyDriver{base}, 100, 0},
		{deploymentLimitsDriver{deploymentPolicyDriver{base}}, 50, 10 * time.Second},
	} {
		got := map[limits.EntityKind]limits.Subject{}
		for _, s := range e.subjects(cfg, c.drv, cfg.FindKey("k1"), &core.Request{}, model) {
			got[s.Entity.Kind] = s
		}
		d, ok := got[limits.KindDeployment]
		if !ok || d.Entity.ID != "acme/gpt-x" || d.Limits.TPM != 5000 || d.Limits.RPM != c.rpm || d.Window != c.window || d.Limits.SpendMonthlyUSD != 0 || d.Limits.CreditsUSD != nil {
			t.Errorf("deployment = %+v (present %v)", d, ok)
		}
		if a := got[limits.KindAccount].Limits; a.RPM != 0 || a.TPM != 0 || a.SpendMonthlyUSD != 10 || a.CreditsUSD == nil || *a.CreditsUSD != 3 {
			t.Errorf("account = %+v, want only the dollar limits", a)
		}
		if k := got[limits.KindKey].Limits; k.RPM != 2 {
			t.Errorf("key = %+v", k)
		}
	}
	// A key without keys[].account: the deployment of its implicit account.
	subs := e.subjects(cfg, deploymentPolicyDriver{base}, cfg.FindKey("k2"), &core.Request{}, model)
	var dep *limits.Subject
	for i := range subs {
		if subs[i].Entity.Kind == limits.KindDeployment {
			dep = &subs[i]
		}
	}
	if dep == nil || dep.Entity.ID != "acct-solo/gpt-x" {
		t.Errorf("deployment of a key without account = %+v", dep)
	}
	// No key (auth none): no deployment.
	for _, s := range e.subjects(cfg, deploymentPolicyDriver{base}, nil, &core.Request{}, model) {
		if s.Entity.Kind == limits.KindDeployment {
			t.Errorf("deployment without a key: %+v", s)
		}
	}
}
