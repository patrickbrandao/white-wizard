package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// accountCases cover the per-account history of ADR 0051 (README 2.1, 3.5,
// 3.12): purchases (limits.CreditAdder), the accumulated spend reported by
// Finish for promote_when and the compare-and-swap of the tier
// (limits.TierSwapper). Subtests that need an optional interface are
// skipped when the store does not implement it.
func accountCases() []testCase {
	return []testCase{
		{"AddCreditsAccumulates", testAddCredits},
		{"AddCreditsRejectsNonPositive", testAddCreditsInvalid},
		{"FinishReportsAccountUsage", testFinishUsage},
		{"ResetZeroesSpentKeepsPurchased", testResetKeepsPurchased},
		{"SwapTierCompareAndSwap", testSwapTier},
		{"SwapTierConcurrentOnce", testSwapTierConcurrent},
	}
}

func creditAdder(t T, s limits.Store) limits.CreditAdder {
	t.Helper()
	ca, ok := s.(limits.CreditAdder)
	if !ok {
		t.Skip("store does not implement limits.CreditAdder")
	}
	return ca
}

func tierSwapper(t T, s limits.Store) limits.TierSwapper {
	t.Helper()
	ts, ok := s.(limits.TierSwapper)
	if !ok {
		t.Skip("store does not implement limits.TierSwapper")
	}
	return ts
}

func addCredits(t T, ca limits.CreditAdder, e limits.Entity, usd float64) limits.EntityState {
	t.Helper()
	st, err := ca.AddCredits(context.Background(), e, usd)
	if err != nil {
		t.Fatalf("AddCredits(%v, %v): %v", e, usd, err)
	}
	return st
}

func testAddCredits(t T, s limits.Store) {
	ca := creditAdder(t, s)
	st := addCredits(t, ca, accountEnt, 5)
	if st.Entity != accountEnt || st.Credits == nil || !approx(*st.Credits, 5, 1e-9) || !approx(st.PurchasedUSD, 5, 1e-9) || st.Status != limits.StatusActive {
		t.Fatalf("first purchase: %+v", st)
	}
	st = addCredits(t, ca, accountEnt, 2.5)
	if !approx(*st.Credits, 7.5, 1e-9) || !approx(st.PurchasedUSD, 7.5, 1e-9) {
		t.Fatalf("second purchase: %+v", st)
	}
	// A debit lowers the balance, never the purchases; SetCredits sets the
	// balance without counting as a purchase.
	finish(t, s, limits.FinishRequest{Subjects: subs(sub(accountEnt, config.Limits{})), Now: baseTime(), CostMicroUSD: 1_500_000, Status: 200})
	if st := get(t, s, accountEnt); !approx(*st.Credits, 6, 1e-9) || !approx(st.PurchasedUSD, 7.5, 1e-9) {
		t.Fatalf("after debit: %+v", st)
	}
	must(t, s.SetCredits(context.Background(), accountEnt, 1))
	if st := get(t, s, accountEnt); !approx(*st.Credits, 1, 1e-9) || !approx(st.PurchasedUSD, 7.5, 1e-9) {
		t.Fatalf("after SetCredits: %+v", st)
	}
	if lst := list(t, s, "", 0); len(lst) != 1 || lst[0].Entity != accountEnt {
		t.Fatalf("AddCredits must index the entity: %+v", lst)
	}
	// A purchase that leaves the balance negative keeps it limited.
	must(t, s.SetCredits(context.Background(), key2Ent, -10))
	if st := addCredits(t, ca, key2Ent, 4); st.Status != limits.StatusLimited || !approx(*st.Credits, -6, 1e-9) {
		t.Fatalf("negative balance after purchase: %+v", st)
	}
	if lst := list(t, s, limits.StatusLimited, 0); len(lst) != 1 || lst[0].Entity != key2Ent {
		t.Fatalf("limited after purchase: %+v", lst)
	}
	if st := addCredits(t, ca, key2Ent, 6); st.Status != limits.StatusActive {
		t.Fatalf("balance back to zero: %+v", st)
	}
	if lst := list(t, s, limits.StatusLimited, 0); len(lst) != 0 {
		t.Fatalf("still limited: %+v", lst)
	}
}

func testAddCreditsInvalid(t T, s limits.Store) {
	ca := creditAdder(t, s)
	for _, usd := range []float64{0, -1} {
		if _, err := ca.AddCredits(context.Background(), accountEnt, usd); err == nil {
			t.Fatalf("AddCredits(%v) must fail", usd)
		}
	}
	if st := get(t, s, accountEnt); !st.Since.IsZero() || st.Credits != nil {
		t.Fatalf("a refused purchase created the entity: %+v", st)
	}
}

// usageLog records the OnUsage calls of Finish.
type usageLog struct {
	mu  sync.Mutex
	got []limits.Entity
	u   []config.PromotionUsage
}

func (l *usageLog) fn(e limits.Entity, u config.PromotionUsage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, e)
	l.u = append(l.u, u)
}

func testFinishUsage(t T, s limits.Store) {
	ca := creditAdder(t, s)
	now := baseTime()
	ss := subs(sub(keyEnt, config.Limits{RPM: 100}), sub(accountEnt, config.Limits{RPM: 100}))
	addCredits(t, ca, accountEnt, 10)
	var log usageLog
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Second)
		mustAllow(t, checkAt(t, s, at, 10, ss...), "check")
		finish(t, s, limits.FinishRequest{Subjects: ss, Now: at, CostMicroUSD: 1_250_000, Status: 200, OnUsage: log.fn})
	}
	if len(log.got) != 2 || log.got[0] != accountEnt || log.got[1] != accountEnt {
		t.Fatalf("OnUsage must be called once per finish, for the account only: %+v", log.got)
	}
	u := log.u[1]
	if u.Requests != 2 || !approx(u.SpentUSD, 2.5, 1e-9) || !approx(u.PurchasedUSD, 10, 1e-9) || u.Age < 0 {
		t.Fatalf("usage after two requests: %+v", u)
	}
	if log.u[0].Requests != 1 || !approx(log.u[0].SpentUSD, 1.25, 1e-9) {
		t.Fatalf("usage after one request: %+v", log.u[0])
	}
	// Get shows the purchases; the next purchase returns the whole history.
	if st := get(t, s, accountEnt); !approx(st.PurchasedUSD, 10, 1e-9) {
		t.Fatalf("Get after spend: %+v", st)
	}
	if st := addCredits(t, ca, accountEnt, 1); !approx(st.SpentTotalUSD, 2.5, 1e-9) || !approx(st.Usage(now).PurchasedUSD, 11, 1e-9) {
		t.Fatalf("AddCredits after spend: %+v", st)
	}
	// A denial reports nothing.
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: now, Status: 429, Denial: &limits.Denial{Entity: accountEnt, Limit: "rpm"}, OnUsage: log.fn})
	if len(log.got) != 2 {
		t.Fatalf("OnUsage called for a denial: %+v", log.got)
	}
}

// history reads the promotion history of an account through a Finish
// without cost (the concurrency limit keeps the finish under
// store.local_first).
func history(t T, s limits.Store, e limits.Entity, now time.Time) config.PromotionUsage {
	t.Helper()
	var log usageLog
	finish(t, s, limits.FinishRequest{Subjects: subs(sub(e, config.Limits{Concurrency: 1})), Now: now, Status: 200, OnUsage: log.fn})
	if len(log.u) != 1 {
		t.Fatalf("OnUsage calls for %v: %d", e, len(log.u))
	}
	return log.u[0]
}

func testResetKeepsPurchased(t T, s limits.Store) {
	ca := creditAdder(t, s)
	er := entityResetter(t, s)
	cr, ok := s.(limits.CounterResetter)
	if !ok {
		t.Skip("store does not implement limits.CounterResetter")
	}
	ctx := context.Background()
	now := baseTime()
	acct2 := limits.Entity{Kind: limits.KindAccount, ID: "acct-2"}
	spend := func(e limits.Entity) {
		finish(t, s, limits.FinishRequest{Subjects: subs(sub(e, config.Limits{})), Now: now, CostMicroUSD: 2_000_000, Status: 200})
	}
	for _, e := range []limits.Entity{accountEnt, acct2} {
		addCredits(t, ca, e, 3)
		spend(e)
	}
	must(t, er.ResetEntity(ctx, accountEnt, true, false))
	if u := history(t, s, accountEnt, now); u.SpentUSD != 0 || !approx(u.PurchasedUSD, 3, 1e-9) {
		t.Fatalf("ResetEntity must zero spent and keep purchased: %+v", u)
	}
	if st := get(t, s, accountEnt); !approx(st.PurchasedUSD, 3, 1e-9) || !approx(*st.Credits, 1, 1e-9) {
		t.Fatalf("ResetEntity must keep purchased and credits: %+v", st)
	}
	if u := history(t, s, acct2, now); !approx(u.SpentUSD, 2, 1e-9) {
		t.Fatalf("ResetEntity touched another entity: %+v", u)
	}
	spend(accountEnt)
	must(t, cr.ResetCounters(ctx))
	for _, e := range []limits.Entity{accountEnt, acct2} {
		if u := history(t, s, e, now); u.SpentUSD != 0 || !approx(u.PurchasedUSD, 3, 1e-9) {
			t.Fatalf("ResetCounters must zero spent and keep purchased (%v): %+v", e, u)
		}
	}
}

func testSwapTier(t T, s limits.Store) {
	ts := tierSwapper(t, s)
	ctx := context.Background()
	swap := func(expected, tier string, want bool) {
		t.Helper()
		ok, err := ts.SwapTier(ctx, accountEnt, expected, tier)
		must(t, err)
		if ok != want {
			t.Fatalf("SwapTier(%q → %q) = %v, want %v", expected, tier, ok, want)
		}
	}
	v0 := policies(t, s).Version
	swap("", "tier-2", true)
	swap("", "tier-3", false)       // the field is no longer absent
	swap("tier-3", "tier-4", false) // nor tier-3
	swap("tier-2", "tier-3", true)  // one step up
	p := policies(t, s)
	if p.Version != v0+2 || p.Entities[accountEnt] != "tier-3" {
		t.Fatalf("policies after swaps: %+v (before %d)", p, v0)
	}
	if st := get(t, s, accountEnt); st.Tier != "tier-3" {
		t.Fatalf("display tier: %+v", st)
	}
}

func testSwapTierConcurrent(t T, s limits.Store) {
	ts := tierSwapper(t, s)
	v0 := policies(t, s).Version
	var won atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			ok, err := ts.SwapTier(context.Background(), accountEnt, "", "tier-2")
			if err != nil {
				errs <- err
			}
			if ok {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("SwapTier: %v", err)
	}
	if won.Load() != 1 {
		t.Fatalf("%d instances promoted, want exactly 1", won.Load())
	}
	if p := policies(t, s); p.Version != v0+1 || p.Entities[accountEnt] != "tier-2" {
		t.Fatalf("policies: %+v (before %d)", p, v0)
	}
}
