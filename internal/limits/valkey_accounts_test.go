package limits_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// TestValkeyLegacyEntities: data written before ADR 0051 stays readable. A
// legacy ent:key:<label> of cfg:policies is parsed and resolved to the
// key's account (below ent:account:<id>), and an ent hash without the
// purchased/spent fields reads as zero and starts counting on the next
// purchase and finish.
func TestValkeyLegacyEntities(t *testing.T) {
	v := newValkeyStore(t)
	ctx := context.Background()
	c := v.Client()
	panelWrite(t, v, true, "ent:key:k1", "pro")
	p, err := v.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("keys:\n  - {key: sk-1, label: k1, account: acme}\ntiers:\n  pro: {limits: {rpm: 1}}\n  max: {limits: {rpm: 2}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.AccountTiers(cfg); got["acme"] != "pro" {
		t.Fatalf("legacy ent:key: %v", got)
	}
	panelWrite(t, v, true, "ent:account:acme", "max")
	if p, _ = v.Policies(ctx); p.AccountTiers(cfg)["acme"] != "max" {
		t.Fatalf("ent:account must win: %+v", p.Entities)
	}

	// An ent hash of an older wwz: credits, no purchased/spent.
	acct := limits.Entity{Kind: limits.KindAccount, ID: "old"}
	key := "wwz:ent:{account:old}"
	if err := c.Do(ctx, c.B().Hset().Key(key).FieldValue().FieldValue("kind", "account").FieldValue("id", "old").FieldValue("since", "1700000000000").FieldValue("status", "active").FieldValue("credits", "2000000").FieldValue("requests", "4").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	st, err := v.Get(ctx, acct)
	if err != nil || st.PurchasedUSD != 0 || st.SpentTotalUSD != 0 || st.Requests != 4 || *st.Credits != 2 {
		t.Fatalf("old hash: %+v %v", st, err)
	}
	if st, err = v.AddCredits(ctx, acct, 1); err != nil || st.PurchasedUSD != 1 || *st.Credits != 3 {
		t.Fatalf("purchase on an old hash: %+v %v", st, err)
	}
	var u config.PromotionUsage
	err = v.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: acct}}, Now: time.Now(), CostMicroUSD: 500_000, Status: 200, OnUsage: func(_ limits.Entity, x config.PromotionUsage) { u = x }})
	if err != nil || u.SpentUSD != 0.5 || u.PurchasedUSD != 1 || u.Requests != 4 || u.Age < 24*time.Hour {
		t.Fatalf("usage of an old hash: %+v %v", u, err)
	}
}

// TestValkeySwapTierAcrossInstances: two instances promoting the same
// account at once write the field once (set_policy with cas), and both see
// the new version through cfg:changed.
func TestValkeySwapTierAcrossInstances(t *testing.T) {
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	rec := &polRec{}
	b.WatchPolicies(rec.add)
	waitFor(t, "first delivery on subscribe", func() bool { return rec.has(0) })
	acct := limits.Entity{Kind: limits.KindAccount, ID: "acme"}
	var won atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*limits.Valkey{a, b, a, b} {
		wg.Go(func() {
			ok, err := s.SwapTier(context.Background(), acct, "", "pro")
			if err != nil {
				t.Error(err)
			}
			if ok {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d swaps won, want 1", won.Load())
	}
	waitFor(t, "promotion on the other instance", func() bool { return rec.has(1) })
	if p, _, _ := rec.find(1); p.Entities[acct] != "pro" {
		t.Fatalf("policies: %+v", p)
	}
	// A manual SetTier (cas = 0) always writes; a stale promotion then loses.
	if err := a.SetTier(context.Background(), acct, "max"); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.SwapTier(context.Background(), acct, "pro", "max"); err != nil || ok {
		t.Fatalf("stale swap = %v, %v", ok, err)
	}
}
