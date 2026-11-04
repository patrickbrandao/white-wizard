package limits

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// fakeSwapper records SwapTier calls and answers with swapped/err.
type fakeSwapper struct {
	mu      sync.Mutex
	calls   []string // "account:expected>tier"
	swapped bool
	err     error
}

func (f *fakeSwapper) SwapTier(_ context.Context, e Entity, expected, tier string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, e.ID+":"+expected+">"+tier)
	return f.swapped, f.err
}

func (f *fakeSwapper) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const promoteYAML = `
keys:
  - {key: sk-1, label: k1, account: acme}
accounts:
  - {id: acme, tier: free}
tiers:
  free: {limits: {rpm: 1}, promote_when: {requests: 3, purchased_usd: 5}, next: pro}
  pro: {limits: {rpm: 100}}
`

func promoteConfig(t *testing.T, doc string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestPromoterEvaluate: all conditions together, one try per (account,
// tier of origin, policies version), a store error forgets the try, a lost
// race does not count, and nothing happens without promote_when, store or
// account.
func TestPromoterEvaluate(t *testing.T) {
	ctx := context.Background()
	c := promoteConfig(t, promoteYAML)
	sw := &fakeSwapper{swapped: true}
	var promoted []string
	p := &Promoter{Store: sw, Config: func() *config.Config { return c }, Log: slog.New(slog.DiscardHandler), OnPromote: func(from, to string) { promoted = append(promoted, from+">"+to) }}

	if ok, err := p.Evaluate(ctx, "acme", config.PromotionUsage{Requests: 3, PurchasedUSD: 4}); ok || err != nil || sw.n() != 0 {
		t.Fatalf("partial conditions promoted: %v %v", ok, err)
	}
	full := config.PromotionUsage{Requests: 3, PurchasedUSD: 5}
	if ok, err := p.Evaluate(ctx, "acme", full); !ok || err != nil || sw.calls[0] != "acme:>pro" || len(promoted) != 1 || promoted[0] != "free>pro" {
		t.Fatalf("promotion: %v %v %v %v", ok, err, sw.calls, promoted)
	}
	// Same version (no applier: always 0): tried once.
	if ok, _ := p.Evaluate(ctx, "acme", full); ok || sw.n() != 1 {
		t.Fatal("tried twice under the same policies")
	}

	// A store error forgets the try; a lost race keeps it.
	p2 := &Promoter{Store: &fakeSwapper{err: errors.New("down")}, Config: p.Config, Log: p.Log}
	if ok, err := p2.Evaluate(ctx, "acme", full); ok || err == nil {
		t.Fatalf("store error: %v %v", ok, err)
	}
	if _, err := p2.Evaluate(ctx, "acme", full); err == nil {
		t.Fatal("not retried after a store error")
	}
	lost := &fakeSwapper{}
	p3 := &Promoter{Store: lost, Config: p.Config}
	if ok, err := p3.Evaluate(ctx, "acme", full); ok || err != nil || lost.n() != 1 {
		t.Fatalf("lost race: %v %v", ok, err)
	}
	if ok, _ := p3.Evaluate(ctx, "acme", full); ok || lost.n() != 1 || p3.log() == nil {
		t.Fatal("lost race retried under the same policies")
	}

	// Nothing to do.
	var nilP *Promoter
	if ok, _ := nilP.Evaluate(ctx, "acme", full); ok {
		t.Fatal("nil promoter")
	}
	nilP.Wait()
	for _, q := range []*Promoter{
		{Config: p.Config},
		{Store: sw},
		{Store: sw, Config: func() *config.Config { return nil }},
		{Store: sw, Config: func() *config.Config { return promoteConfig(t, "tiers:\n  free: {limits: {rpm: 1}}\n") }},
	} {
		if ok, _ := q.Evaluate(ctx, "acme", full); ok {
			t.Fatalf("promoted with %+v", q)
		}
	}
	if ok, _ := (&Promoter{Store: sw, Config: p.Config}).Evaluate(ctx, "", full); ok {
		t.Fatal("promoted without an account")
	}
}

// TestPromoterObserveWithPolicies: Observe on a memory store with a policy
// applier promotes in the background, reads the assigned tier as origin and
// the raw field as the expected value, and skips non-account subjects.
func TestPromoterObserveWithPolicies(t *testing.T) {
	c := promoteConfig(t, promoteYAML+"  max: {limits: {rpm: 1000}}\n")
	c.Tiers["pro"] = config.Tier{Limits: c.Tiers["pro"].Limits, PromoteWhen: &config.Promote{Requests: 10}, Next: "max"}
	m := NewMemory()
	cur := c
	var mu sync.Mutex
	pa := &PolicyApplier{Log: slog.New(slog.DiscardHandler)}
	pa.Current = func() *config.Config { mu.Lock(); defer mu.Unlock(); return cur }
	pa.Apply = func(n *config.Config) error {
		pa.Override(n)
		mu.Lock()
		cur = n
		mu.Unlock()
		pa.Committed(n)
		return nil
	}
	m.WatchPolicies(pa.Offer)
	var mo sync.Mutex
	var promoted []string
	p := &Promoter{Store: m, Policies: pa, Config: pa.Current, OnPromote: func(from, to string) {
		mo.Lock()
		promoted = append(promoted, from+">"+to)
		mo.Unlock()
	}}
	acme := Entity{Kind: KindAccount, ID: "acme"}

	p.Observe(Entity{Kind: KindKey, ID: "k1"}, config.PromotionUsage{Requests: 100, PurchasedUSD: 100})
	p.Observe(acme, config.PromotionUsage{Requests: 3, PurchasedUSD: 5})
	p.Wait()
	if pa.AccountTier("acme") != "pro" || pa.EntityTier(acme) != "pro" {
		t.Fatalf("after Observe: %q", pa.AccountTier("acme"))
	}
	// From pro (assigned), one step per evaluation.
	p.Timeout = time.Second
	p.Observe(acme, config.PromotionUsage{Requests: 10, PurchasedUSD: 5})
	p.Wait()
	if pa.AccountTier("acme") != "max" {
		t.Fatalf("second step: %q", pa.AccountTier("acme"))
	}
	if len(promoted) != 2 || promoted[0] != "free>pro" || promoted[1] != "pro>max" {
		t.Fatalf("promotions: %v", promoted)
	}
	// Removing the policies leaves no assigned tier.
	if err := m.SetTier(context.Background(), acme, ""); err != nil {
		t.Fatal(err)
	}
	if pa.AccountTier("acme") != "" {
		t.Fatalf("removed: %q", pa.AccountTier("acme"))
	}
	var nilA *PolicyApplier
	if nilA.AccountTier("acme") != "" {
		t.Fatal("nil applier")
	}
	pa.refreshTiers(&Policies{Entities: map[Entity]string{acme: "pro"}}, nil)
	if pa.AccountTier("acme") != "" {
		t.Fatal("no configuration")
	}
}

// TestPromoterNeverDemotes: once promoted, usage falling below the
// threshold (a new window, a reset counter) keeps the new tier (spec 010).
func TestPromoterNeverDemotes(t *testing.T) {
	c := promoteConfig(t, promoteYAML)
	m := NewMemory()
	pa := &PolicyApplier{Log: slog.New(slog.DiscardHandler)}
	cur := c
	pa.Current = func() *config.Config { return cur }
	pa.Apply = func(n *config.Config) error {
		pa.Override(n)
		cur = n
		pa.Committed(n)
		return nil
	}
	m.WatchPolicies(pa.Offer)
	p := &Promoter{Store: m, Policies: pa, Config: pa.Current}
	acme := Entity{Kind: KindAccount, ID: "acme"}

	p.Observe(acme, config.PromotionUsage{Requests: 3, PurchasedUSD: 5})
	p.Wait()
	if pa.AccountTier("acme") != "pro" {
		t.Fatalf("not promoted: %q", pa.AccountTier("acme"))
	}
	for _, u := range []config.PromotionUsage{{}, {Requests: 1}, {Requests: 2, PurchasedUSD: 1}} {
		p.Observe(acme, u)
		p.Wait()
		if got := pa.AccountTier("acme"); got != "pro" {
			t.Fatalf("usage %+v demoted to %q", u, got)
		}
	}
	if ok, _ := p.Evaluate(context.Background(), "acme", config.PromotionUsage{}); ok {
		t.Fatal("promoted again")
	}
}
