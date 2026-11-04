package engine

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// noAccountDriver is a driver whose policy has no account level.
type noAccountDriver struct{ driver.Driver }

func (noAccountDriver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key"}, TPMReserve: "none", ExcludeCacheRead: true}
}

func subjectsCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
tiers:
  big: {limits: {rpm: 100, tpm: 5000, spend_monthly_usd: 10}}
projects:
  - {id: web, limits: {rpm: 3}}
keys:
  - {key: k1, label: a1, account: acme, project: web, tier: big, limits: {rpm: 2, tpm: 9000}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Task 189 (ADR 0051): the tier counts on the account, the project and the
// key only their declared ceilings, and the driver's token options hold on
// every level.
func TestSubjectsPerLevel(t *testing.T) {
	cfg := subjectsCfg(t)
	drv, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	got := map[limits.EntityKind]config.Limits{}
	for _, s := range e.subjects(cfg, drv, cfg.FindKey("k1"), &core.Request{ClientIP: "10.0.0.1"}, &config.ModelConfig{}) {
		got[s.Entity.Kind] = s.Limits
	}
	if l := got[limits.KindAccount]; l.RPM != 100 || l.TPM != 5000 || l.SpendMonthlyUSD != 10 || l.TPMReserve != "none" || !l.ExcludeCacheRead {
		t.Errorf("account = %+v, want the tier with the driver's token options", l)
	}
	if l := got[limits.KindProject]; l.RPM != 3 || l.TPM != 0 {
		t.Errorf("project = %+v, want only projects[].limits", l)
	}
	if l := got[limits.KindKey]; l.RPM != 2 || l.TPM != 9000 || l.SpendMonthlyUSD != 0 {
		t.Errorf("key = %+v, want only keys[].limits", l)
	}
	if l, ok := got[limits.KindIP]; !ok || l.RPM != 0 {
		t.Errorf("ip = %+v (present %v), want only block and ban", l, ok)
	}
}

// TestSubjectsWithoutAccountLevel: a driver without the account level counts
// the tier on the key, each count limit the tighter of tier and ceiling.
func TestSubjectsWithoutAccountLevel(t *testing.T) {
	cfg := subjectsCfg(t)
	base, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	subs := e.subjects(cfg, noAccountDriver{base}, cfg.FindKey("k1"), &core.Request{}, &config.ModelConfig{})
	if len(subs) != 1 || subs[0].Entity.Kind != limits.KindKey {
		t.Fatalf("subjects = %+v, want the key alone", subs)
	}
	if l := subs[0].Limits; l.RPM != 2 || l.TPM != 5000 || l.SpendMonthlyUSD != 10 {
		t.Errorf("key = %+v, want rpm 2 (ceiling), tpm 5000 (tier), spend 10 (tier)", l)
	}
}

func TestUnderCeiling(t *testing.T) {
	got := underCeiling(config.Limits{RPM: 10, TPM: 100}, config.Limits{RPM: 20, TPM: 50, RPD: 7})
	if got.RPM != 10 || got.TPM != 50 || got.RPD != 7 {
		t.Errorf("underCeiling = %+v, want rpm 10, tpm 50, rpd 7", got)
	}
}
