package limits

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// TestAccountTiers: ent:account:<id> wins over the legacy ent:key:<label>
// (resolved to the key's account), unknown keys and other kinds are ignored
// (ADR 0051, README 2.3).
func TestAccountTiers(t *testing.T) {
	c, err := config.Parse([]byte(`
keys:
  - {key: sk-1, label: k1, account: acme}
  - {key: sk-2, label: k2, account: acme}
  - {key: sk-3, label: k3, account: solo}
tiers:
  free: {limits: {rpm: 10}}
  pro: {limits: {rpm: 100}}
  max: {limits: {rpm: 1000}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := Policies{Version: 3, Entities: map[Entity]string{
		{Kind: KindKey, ID: "k1"}:         "free",
		{Kind: KindKey, ID: "k2"}:         "pro", // last label of acme wins among legacy fields
		{Kind: KindAccount, ID: "acme"}:   "max",
		{Kind: KindKey, ID: "k3"}:         "pro",
		{Kind: KindKey, ID: "gone"}:       "pro",
		{Kind: KindIP, ID: "1.2.3.4"}:     "free",
		{Kind: KindAccount, ID: "nokeys"}: "free",
	}}
	got := p.AccountTiers(c)
	if len(got) != 3 || got["acme"] != "max" || got["solo"] != "pro" || got["nokeys"] != "free" {
		t.Fatalf("AccountTiers = %v", got)
	}
	delete(p.Entities, Entity{Kind: KindAccount, ID: "acme"})
	if got := p.AccountTiers(c); got["acme"] != "pro" {
		t.Fatalf("legacy ent:key: %v", got)
	}
}

// TestPoliciesAccountTierApplied: ent:account:<id> is the tier of every key
// of the account through AccountTiers (LimitsFor with it), never written in
// the configuration (task 191); an unknown tier is refused, and
// tier:<name> keeps promote_when and next.
func TestPoliciesAccountTierApplied(t *testing.T) {
	c, err := config.Parse([]byte(`
keys:
  - {key: sk-1, label: k1, account: acme}
  - {key: sk-2, label: k2, account: acme}
  - {key: sk-3, label: k3}
accounts:
  - {id: acme, tier: free}
tiers:
  free: {limits: {rpm: 10}, promote_when: {purchased_usd: 5}, next: pro}
  pro: {limits: {rpm: 100}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := Policies{Version: 1, Tiers: map[string]config.Limits{"free": {RPM: 20}}, Entities: map[Entity]string{
		{Kind: KindAccount, ID: "acme"}: "pro", {Kind: KindKey, ID: "k1"}: "free",
	}}
	a, err := p.apply(c)
	if err != nil {
		t.Fatal(err)
	}
	if findKey(c, "k1").Tier != "free" || c.FindAccount("acme").Tier != "free" || findKey(c, "k3").Tier != "default" {
		t.Fatalf("apply wrote the entity tier: %+v %+v", c.Keys, c.Accounts)
	}
	tiers := p.AccountTiers(c)
	if l := c.LimitsFor(findKey(c, "k2"), tiers["acme"]); l.Account.Tier != "pro" || l.Account.Limits.RPM != 100 {
		t.Fatalf("LimitsFor: %+v", l)
	}
	if ft := c.Tiers["free"]; ft.Limits.RPM != 20 || ft.Next != "pro" || ft.PromoteWhen == nil || ft.PromoteWhen.PurchasedUSD != 5 {
		t.Fatalf("tier:free must keep promote_when/next: %+v", ft)
	}
	a.restore(c)
	if c.FindAccount("acme").Tier != "free" || c.Tiers["free"].Limits.RPM != 10 {
		t.Fatalf("restored: %+v %+v %+v", c.Keys, c.Accounts, c.Tiers["free"])
	}
	bad := Policies{Version: 2, Entities: map[Entity]string{{Kind: KindAccount, ID: "acme"}: "nope"}}
	if _, err := bad.apply(c); err == nil {
		t.Fatal("unknown tier in ent:account must be refused")
	}
}

// TestPolicyApplierEntityTier: the raw field of the policies in effect, the
// expected value of a promotion's SwapTier.
func TestPolicyApplierEntityTier(t *testing.T) {
	c, err := config.Parse([]byte("keys:\n  - {key: sk-1, label: k1, account: acme}\ntiers:\n  pro: {limits: {rpm: 1}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewMemory()
	acme := Entity{Kind: KindAccount, ID: "acme"}
	cur := c
	pa := &PolicyApplier{Log: slog.New(slog.DiscardHandler)}
	pa.Current = func() *config.Config { return cur }
	pa.Apply = func(n *config.Config) error {
		pa.Override(n)
		cur = n
		pa.Committed(n)
		return nil
	}
	if pa.EntityTier(acme) != "" {
		t.Fatal("no policies yet")
	}
	m.WatchPolicies(pa.Offer)
	ok, err := m.SwapTier(context.Background(), acme, "", "pro")
	if err != nil || !ok {
		t.Fatalf("SwapTier = %v, %v", ok, err)
	}
	if pa.EntityTier(acme) != "pro" || pa.EntityTier(Entity{Kind: KindAccount, ID: "x"}) != "" {
		t.Fatalf("EntityTier = %q", pa.EntityTier(acme))
	}
	if pa.AccountTier("acme") != "pro" || pa.AccountTier("other") != "" || cur.AccountTier("acme") != "default" {
		t.Fatalf("effective account tier: %q (config %q)", pa.AccountTier("acme"), cur.AccountTier("acme"))
	}
}

// TestUsage covers EntityState.Usage: age from since, never negative.
func TestUsage(t *testing.T) {
	now := time.Now()
	st := EntityState{Since: now.Add(-48 * time.Hour), Requests: 7, PurchasedUSD: 5, SpentTotalUSD: 2}
	if u := st.Usage(now); u.Age != 48*time.Hour || u.Requests != 7 || u.PurchasedUSD != 5 || u.SpentUSD != 2 {
		t.Fatalf("Usage = %+v", u)
	}
	if u := (EntityState{Since: now.Add(time.Hour)}).Usage(now); u.Age != 0 {
		t.Fatalf("future since: %+v", u)
	}
	if u := (EntityState{}).Usage(now); u.Age != 0 {
		t.Fatalf("zero since: %+v", u)
	}
}

func TestCheckPurchase(t *testing.T) {
	for _, usd := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if checkPurchase(usd) == nil {
			t.Errorf("checkPurchase(%v) must fail", usd)
		}
	}
	if checkPurchase(0.01) != nil {
		t.Error("0.01 is a valid purchase")
	}
}

// TestFallbackAccountsDown: with the circuit open, purchases and swaps are
// admin operations (ErrStoreDown) and the interim memory store reports no
// account history.
func TestFallbackAccountsDown(t *testing.T) {
	cfg := config.StoreConfig{Kind: "valkey", Addr: []string{"127.0.0.1:1"}, ReadTimeout: 5 * time.Millisecond, OnDown: "local", Prefix: "wwz"}
	f := OpenValkey(context.Background(), cfg, ValkeyOptions{Heartbeat: -1}, FallbackOptions{ProbeInterval: time.Hour})
	t.Cleanup(func() { _ = f.Close() })
	if !f.CircuitOpen() {
		t.Fatal("circuit must be open")
	}
	ctx := context.Background()
	acct := Entity{Kind: KindAccount, ID: "a"}
	if _, err := f.AddCredits(ctx, acct, 1); !errors.Is(err, ErrStoreDown) {
		t.Fatalf("AddCredits = %v", err)
	}
	if _, err := f.SwapTier(ctx, acct, "", "pro"); !errors.Is(err, ErrStoreDown) {
		t.Fatalf("SwapTier = %v", err)
	}
	called := false
	err := f.Finish(ctx, FinishRequest{Subjects: []Subject{{Entity: acct}}, Now: time.Now(), CostMicroUSD: 10, Status: 200, OnUsage: func(Entity, config.PromotionUsage) { called = true }})
	if err != nil || called {
		t.Fatalf("Finish on the interim store: err %v, OnUsage called %v", err, called)
	}
}
