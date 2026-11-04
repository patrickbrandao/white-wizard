package limits

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

const policyYAML = `
keys:
  - {key: sk-a, label: a, tier: free}
  - {key: sk-b, label: b, tier: free, limits: {rpm: 7}}
  - {key: sk-c, label: "c:1"}
tiers:
  free: {limits: {rpm: 10}}
  pro: {limits: {rpm: 100}, next: free}
`

func policyConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(policyYAML))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// keyRPM is the rpm a request of the key meets (ADR 0051): the tier of its
// account (tierOf: the tier the policies assign, "" = configured) under the
// key's own ceiling.
func keyRPM(t *testing.T, c *config.Config, tierOf func(string) string, label string) int {
	t.Helper()
	k := findKey(c, label)
	if k == nil {
		t.Fatalf("key %q not found", label)
	}
	tier := ""
	if tierOf != nil {
		tier = tierOf(k.Account)
	}
	eff := c.LimitsFor(k, tier)
	rpm := eff.Account.Limits.RPM
	if r := eff.Key.Limits.RPM; r != 0 && (rpm == 0 || r < rpm) {
		rpm = r
	}
	return rpm
}

// tiersOf is the account tier lookup of policies p on c (apply no longer
// writes it in the configuration, task 191).
func tiersOf(p *Policies, c *config.Config) func(string) string {
	m := p.AccountTiers(c)
	return func(acct string) string { return m[acct] }
}

var (
	entA = Entity{Kind: KindKey, ID: "a"}
	entB = Entity{Kind: KindKey, ID: "b"}
	entC = Entity{Kind: KindKey, ID: "c:1"}
)

func TestParsePolicies(t *testing.T) {
	p, err := ParsePolicies(map[string]string{
		"version":        "4",
		"tier:pro":       `{"rpm": 600, "tpm": 400000, "queue_timeout": "30s", "on_limit": "queue"}`,
		"tier:tier-4":    `{"rpm": 5}`,
		"ent:key:c:1":    "pro",
		"ent:ip:1.2.3.4": "tier-4",
		"ent:key:gone":   "",
		"other":          "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 4 || p.Tiers["pro"].RPM != 600 || p.Tiers["pro"].QueueTimeout != 30*time.Second || p.Tiers["tier-4"].RPM != 5 {
		t.Fatalf("parsed: %+v", p)
	}
	if p.Entities[entC] != "pro" || p.Entities[Entity{Kind: KindIP, ID: "1.2.3.4"}] != "tier-4" || len(p.Entities) != 2 {
		t.Fatalf("entities: %+v", p.Entities)
	}
	if p, err := ParsePolicies(nil); err != nil || p.Version != 0 || p.Tiers == nil || p.Entities == nil {
		t.Fatalf("empty: %+v %v", p, err)
	}
	for name, h := range map[string]map[string]string{
		"version":     {"version": "x"},
		"negative":    {"version": "-1"},
		"json":        {"version": "2", "tier:pro": "{broken"},
		"notObject":   {"version": "2", "tier:pro": "[1]"},
		"unknownLim":  {"version": "2", "tier:pro": `{"rpmm": 1}`},
		"badType":     {"version": "2", "tier:pro": `{"rpm": "many"}`},
		"emptyName":   {"version": "2", "tier:": `{}`},
		"entNoID":     {"version": "2", "ent:key": "pro"},
		"entEmptyID":  {"version": "2", "ent:key:": "pro"},
		"entEmptyKnd": {"version": "2", "ent::x": "pro"},
	} {
		p, err := ParsePolicies(h)
		if err == nil {
			t.Errorf("%s: no error (%+v)", name, p)
			continue
		}
		want := int64(2)
		if strings.HasPrefix(name, "version") || name == "negative" {
			want = 0
		}
		if p.Version != want {
			t.Errorf("%s: version %d, want %d (%v)", name, p.Version, want, err)
		}
	}
}

func TestPoliciesApplyAndRestore(t *testing.T) {
	c := policyConfig(t)
	p := Policies{Version: 1, Tiers: map[string]config.Limits{"free": {RPM: 20}, "gold": {RPM: 1000}}, Entities: map[Entity]string{
		entA: "pro", entB: "gold", {Kind: KindIP, ID: "x"}: "gold", {Kind: KindKey, ID: "nobody"}: "pro",
	}}
	a, err := p.apply(c)
	if err != nil {
		t.Fatal(err)
	}
	// keys[].limits > entity tier > keys[].tier > default.
	tf := tiersOf(&p, c)
	if keyRPM(t, c, tf, "a") != 100 || keyRPM(t, c, tf, "b") != 7 || keyRPM(t, c, tf, "c:1") != 0 {
		t.Fatalf("rpm a=%d b=%d c=%d", keyRPM(t, c, tf, "a"), keyRPM(t, c, tf, "b"), keyRPM(t, c, tf, "c:1"))
	}
	// The tier of an entity is never written in the configuration.
	if findKey(c, "a").Tier != "free" || c.AccountTier("acct-a") != "free" {
		t.Fatalf("apply wrote the entity tier: %+v", findKey(c, "a"))
	}
	if c.Tiers["free"].Limits.RPM != 20 || c.Tiers["gold"].Limits.RPM != 1000 || c.Tiers["pro"].Next != "free" {
		t.Fatalf("tiers: %+v", c.Tiers)
	}
	a.restore(c)
	if c.Tiers["free"].Limits.RPM != 10 || len(c.Tiers) != 3 || findKey(c, "a").Tier != "free" || findKey(c, "b").Tier != "free" {
		t.Fatalf("restore: %+v", c.Tiers)
	}
	(*appliedPolicies)(nil).restore(c)

	// A value changed after the application (a patch) is kept.
	a, err = p.apply(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Tiers["free"] = config.Tier{Limits: config.Limits{RPM: 33}}
	a.restore(c)
	if c.Tiers["free"].Limits.RPM != 33 || c.Tiers["gold"].Limits.RPM != 0 {
		t.Fatalf("patched values must survive restore: %+v", c.Tiers)
	}

	// Unknown tier and invalid limits leave the configuration untouched.
	c = policyConfig(t)
	bad := Policies{Version: 2, Entities: map[Entity]string{entA: "nope"}}
	if _, err := bad.apply(c); err == nil || !strings.Contains(err.Error(), "unknown tier") {
		t.Fatalf("unknown tier: %v", err)
	}
	bad = Policies{Version: 2, Tiers: map[string]config.Limits{"free": {RPM: -1}}, Entities: map[Entity]string{entA: "pro"}}
	if _, err := bad.apply(c); err == nil {
		t.Fatal("invalid limits accepted")
	}
	if c.Tiers["free"].Limits.RPM != 10 || findKey(c, "a").Tier != "free" {
		t.Fatalf("failed apply changed the configuration: %+v", c.Tiers)
	}
	c.Tiers = nil
	if _, err := (&Policies{Tiers: map[string]config.Limits{"x": {RPM: 1}}}).apply(c); err != nil || c.Tiers["x"].Limits.RPM != 1 {
		t.Fatalf("nil tiers: %v %+v", err, c.Tiers)
	}
}

func TestCloneConfig(t *testing.T) {
	if _, err := cloneConfig(nil); err == nil {
		t.Fatal("nil config cloned")
	}
	c := policyConfig(t)
	x, err := cloneConfig(c)
	if err != nil || x == c || keyRPM(t, x, nil, "b") != 7 {
		t.Fatalf("clone: %v", err)
	}
}

// fakeAdmin mimics admin.Server.Apply: Override, validation, swap, observers.
type fakeAdmin struct {
	mu   sync.Mutex
	cfg  *config.Config
	a    *PolicyApplier
	fail error
}

func (f *fakeAdmin) current() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeAdmin) apply(c *config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.a.Override(c)
	if err := c.Validate(); err != nil {
		return err
	}
	if f.fail != nil {
		return f.fail
	}
	f.cfg = c
	f.a.Committed(c)
	return nil
}

func newApplier(t *testing.T) (*PolicyApplier, *fakeAdmin, *[]int64, *int) {
	t.Helper()
	var versions []int64
	errs := 0
	a := &PolicyApplier{Log: slog.New(slog.DiscardHandler), OnVersion: func(v int64) { versions = append(versions, v) }, OnError: func() { errs++ }}
	f := &fakeAdmin{cfg: policyConfig(t), a: a}
	a.Current, a.Apply = f.current, f.apply
	return a, f, &versions, &errs
}

func TestPolicyApplier(t *testing.T) {
	a, f, versions, errs := newApplier(t)
	if a.Version() != 0 || a.log() == nil {
		t.Fatal("fresh applier")
	}
	a.Offer(Policies{}, errors.New("valkey down")) // version 0: ignored
	p1 := Policies{Version: 1, Tiers: map[string]config.Limits{"free": {RPM: 50}}, Entities: map[Entity]string{entA: "pro"}}
	a.Offer(p1, nil)
	if a.Version() != 1 || keyRPM(t, f.current(), a.AccountTier, "a") != 100 || f.current().Tiers["free"].Limits.RPM != 50 {
		t.Fatalf("v1 not applied: %d", a.Version())
	}
	a.Offer(Policies{Version: 1, Entities: map[Entity]string{entA: "free"}}, nil) // same version: ignored
	if keyRPM(t, f.current(), a.AccountTier, "a") != 100 || *errs != 0 {
		t.Fatal("same version re-applied")
	}

	// Invalid versions are counted once and the previous ones stay.
	a.Offer(Policies{Version: 2, Entities: map[Entity]string{entA: "nope"}}, nil)
	a.Offer(Policies{Version: 3}, errors.New("broken json"))
	a.Offer(Policies{Version: 3}, nil)
	if *errs != 2 || a.Version() != 1 || keyRPM(t, f.current(), a.AccountTier, "a") != 100 {
		t.Fatalf("errors %d, version %d", *errs, a.Version())
	}

	// File reload: a fresh YAML gets the policies on top.
	if err := f.apply(policyConfig(t)); err != nil {
		t.Fatal(err)
	}
	if keyRPM(t, f.current(), a.AccountTier, "a") != 100 || f.current().Tiers["free"].Limits.RPM != 50 {
		t.Fatal("reload lost the policies")
	}
	// Patch: starts from the effective configuration; an explicit change wins.
	x, err := cloneConfig(f.current())
	if err != nil {
		t.Fatal(err)
	}
	x.Tiers["pro"] = config.Tier{Limits: config.Limits{RPM: 300}}
	if err := f.apply(x); err != nil {
		t.Fatal(err)
	}
	if keyRPM(t, f.current(), a.AccountTier, "a") != 300 || f.current().Tiers["free"].Limits.RPM != 50 {
		t.Fatalf("patch: a=%d", keyRPM(t, f.current(), a.AccountTier, "a"))
	}

	// Removing the policies restores the YAML (with the patch).
	a.Offer(Policies{Version: 4}, nil)
	if a.Version() != 4 || keyRPM(t, f.current(), a.AccountTier, "a") != 10 || f.current().Tiers["free"].Limits.RPM != 10 || f.current().Tiers["pro"].Limits.RPM != 300 {
		t.Fatalf("v4: a=%d free=%d", keyRPM(t, f.current(), a.AccountTier, "a"), f.current().Tiers["free"].Limits.RPM)
	}

	// A swap refused by the admin keeps the previous version.
	f.fail = errors.New("restart needed")
	a.Offer(Policies{Version: 5, Entities: map[Entity]string{entA: "pro"}}, nil)
	f.fail = nil
	if a.Version() != 4 || *errs != 3 || keyRPM(t, f.current(), a.AccountTier, "a") != 10 {
		t.Fatalf("refused swap: version %d errors %d", a.Version(), *errs)
	}
	if err := f.apply(policyConfig(t)); err != nil || keyRPM(t, f.current(), a.AccountTier, "a") != 10 {
		t.Fatalf("reload after refused swap: %v", err)
	}
	if (*versions)[len(*versions)-1] != 4 {
		t.Fatalf("versions %v", *versions)
	}

	// An invalid base is left alone (the admin refuses it).
	bad := policyConfig(t)
	bad.Models = nil
	a.Override(bad)
	a.Committed(policyConfig(t)) // not the candidate of the last Override: no change
	if a.Version() != 4 {
		t.Fatal("invalid base changed the version")
	}
}

func TestPolicyApplierFallsBackToCurrent(t *testing.T) {
	a, f, _, _ := newApplier(t)
	a.Offer(Policies{Version: 1, Tiers: map[string]config.Limits{"gold": {RPM: 9}}, Entities: map[Entity]string{entA: "gold"}}, nil)
	if keyRPM(t, f.current(), a.AccountTier, "a") != 9 {
		t.Fatal("v1")
	}
	// A reload whose YAML makes the wanted policies invalid keeps the
	// configuration without them (and the admin swaps it in).
	c := policyConfig(t)
	c.Keys[0].Limits = &config.Limits{RPM: 2}
	a.mu.Lock()
	a.want = &Policies{Version: 9, Entities: map[Entity]string{entA: "nope"}}
	a.mu.Unlock()
	if err := f.apply(c); err != nil {
		t.Fatal(err)
	}
	if a.Version() != 1 || keyRPM(t, f.current(), a.AccountTier, "a") != 2 || f.current().Tiers["gold"].Limits.RPM != 9 {
		t.Fatalf("fallback to current: version %d", a.Version())
	}
	// A first version that no longer validates, with nothing in effect.
	b, fb, _, _ := newApplier(t)
	b.mu.Lock()
	b.want = &Policies{Version: 9, Entities: map[Entity]string{entA: "nope"}}
	b.mu.Unlock()
	if err := fb.apply(policyConfig(t)); err != nil || b.Version() != 0 {
		t.Fatalf("no policies in effect: %v %d", err, b.Version())
	}
	// Neither applies: the base alone.
	a.mu.Lock()
	a.want = &Policies{Version: 9, Entities: map[Entity]string{entA: "nope"}}
	a.cur = a.want
	a.mu.Unlock()
	if err := f.apply(policyConfig(t)); err != nil {
		t.Fatal(err)
	}
	if a.Version() != 0 || keyRPM(t, f.current(), a.AccountTier, "a") != 10 {
		t.Fatalf("base alone: version %d", a.Version())
	}
}

func TestPolicyApplierNoConfiguration(t *testing.T) {
	errs := 0
	a := &PolicyApplier{Current: func() *config.Config { return nil }, OnError: func() { errs++ }}
	a.Offer(Policies{Version: 1}, nil)
	if errs != 1 || a.Version() != 0 {
		t.Fatalf("errors %d", errs)
	}
	// Dry run passes, but the effective configuration vanished before Apply.
	calls := 0
	c := policyConfig(t)
	a = &PolicyApplier{Current: func() *config.Config {
		calls++
		if calls > 1 {
			return nil
		}
		return c
	}}
	a.Offer(Policies{Version: 1}, nil)
	if a.Version() != 0 {
		t.Fatal("applied without a configuration")
	}
	// Apply succeeded but did not commit the version (no Override wired).
	a = &PolicyApplier{Current: func() *config.Config { return c }, Apply: func(*config.Config) error { return nil }}
	a.Offer(Policies{Version: 1}, nil)
	if a.Version() != 0 {
		t.Fatal("version without commit")
	}
}

func TestMemoryPolicies(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	var got []Policies
	m.WatchPolicies(func(p Policies, err error) {
		if err != nil {
			t.Error(err)
		}
		got = append(got, p)
	})
	m.SetPolicies(Policies{Version: 7, Tiers: map[string]config.Limits{"pro": {RPM: 1}}})
	if len(got) != 1 || got[0].Version != 7 || got[0].Tiers["pro"].RPM != 1 || got[0].Entities == nil {
		t.Fatalf("watchers: %+v", got)
	}
	if err := m.SetTier(ctx, entA, "pro"); err != nil {
		t.Fatal(err)
	}
	p, _ := m.Policies(ctx)
	if p.Version != 8 || p.Entities[entA] != "pro" || len(got) != 2 {
		t.Fatalf("SetTier: %+v", p)
	}
	m2 := NewMemory()
	if err := m2.SetTier(ctx, entA, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := m2.Policies(ctx); p.Version != 1 || len(p.Entities) != 0 {
		t.Fatalf("remove on empty: %+v", p)
	}
}

// TestPoliciesKeyTierIsAccountTier: a legacy ent:key:<label> sets the tier
// of the key's account (ADR 0051), read through AccountTiers; the
// configuration keeps the YAML tiers.
func TestPoliciesKeyTierIsAccountTier(t *testing.T) {
	c, err := config.Parse([]byte(`
keys:
  - {key: sk-1, label: k1, account: acme}
  - {key: sk-2, label: k2, account: acme}
  - {key: sk-3, label: k3, account: solo, tier: free}
  - {key: sk-4, label: k4}
accounts:
  - {id: acme, tier: free}
  - {id: solo}
tiers:
  free: {limits: {rpm: 10}}
  pro: {limits: {rpm: 100}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := Policies{Version: 1, Entities: map[Entity]string{
		{Kind: KindKey, ID: "k1"}: "pro", {Kind: KindKey, ID: "k2"}: "pro", {Kind: KindKey, ID: "k3"}: "pro",
	}}
	if _, err := p.apply(c); err != nil {
		t.Fatal(err)
	}
	tiers := p.AccountTiers(c)
	if len(tiers) != 2 || tiers["acme"] != "pro" || tiers["solo"] != "pro" {
		t.Fatalf("account tiers: %v", tiers)
	}
	if c.AccountLimits("acme", tiers["acme"]).RPM != 100 || c.AccountLimits("acct-k4", tiers["acct-k4"]).RPM != 0 {
		t.Fatal("account limits")
	}
	// The configuration keeps the YAML tiers.
	if findKey(c, "k1").Tier != "free" || c.FindAccount("acme").Tier != "free" || findKey(c, "k3").Tier != "free" || c.AccountTier("acme") != "free" {
		t.Fatalf("apply wrote the entity tier: %+v %+v", c.Keys, c.Accounts)
	}
}

// TestPolicyApplierOlderVersionIgnored: after v3, an older v2 (even valid
// and different) changes nothing and is not an error (spec 012).
func TestPolicyApplierOlderVersionIgnored(t *testing.T) {
	a, f, _, errs := newApplier(t)
	a.Offer(Policies{Version: 3, Tiers: map[string]config.Limits{"free": {RPM: 50}}, Entities: map[Entity]string{entA: "pro"}}, nil)
	if a.Version() != 3 || keyRPM(t, f.current(), a.AccountTier, "a") != 100 {
		t.Fatalf("v3 not applied: %d", a.Version())
	}
	a.Offer(Policies{Version: 2, Tiers: map[string]config.Limits{"free": {RPM: 7}}, Entities: map[Entity]string{entA: "free"}}, nil)
	if a.Version() != 3 || keyRPM(t, f.current(), a.AccountTier, "a") != 100 || f.current().Tiers["free"].Limits.RPM != 50 || *errs != 0 {
		t.Fatalf("v2 applied after v3: version %d, errors %d", a.Version(), *errs)
	}
}
