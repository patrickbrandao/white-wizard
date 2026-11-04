package storetest

import (
	"context"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// entityCases cover the optional per-entity operations of ADR 0025:
// limits.TierSetter, limits.EntityResetter and limits.PolicySource. Each
// subtest is skipped when the store does not implement the interface.
func entityCases() []testCase {
	return []testCase{
		{"SetTierShowsAndPublishes", testSetTier},
		{"SetTierRemove", testSetTierRemove},
		{"PoliciesSurviveReset", testPoliciesSurviveReset},
		{"ResetEntityCountersOnlyThatEntity", testResetEntityCounters},
		{"ResetEntityKeepsStatus", testResetEntityKeepsStatus},
		{"ResetEntityStrikes", testResetEntityStrikes},
		{"ResetEntityUnknownNotCreated", testResetEntityUnknown},
	}
}

func tierSetter(t T, s limits.Store) limits.TierSetter {
	t.Helper()
	ts, ok := s.(limits.TierSetter)
	if !ok {
		t.Skip("store does not implement limits.TierSetter")
	}
	return ts
}

func entityResetter(t T, s limits.Store) limits.EntityResetter {
	t.Helper()
	er, ok := s.(limits.EntityResetter)
	if !ok {
		t.Skip("store does not implement limits.EntityResetter")
	}
	return er
}

func policies(t T, s limits.Store) limits.Policies {
	t.Helper()
	ps, ok := s.(limits.PolicySource)
	if !ok {
		t.Skip("store does not implement limits.PolicySource")
	}
	p, err := ps.Policies(context.Background())
	if err != nil {
		t.Fatalf("Policies: %v", err)
	}
	return p
}

func testSetTier(t T, s limits.Store) {
	ts := tierSetter(t, s)
	ps, ok := s.(limits.PolicySource)
	if !ok {
		t.Skip("store does not implement limits.PolicySource")
	}
	var mu sync.Mutex
	var got []int64
	ps.WatchPolicies(func(p limits.Policies, err error) {
		if err == nil {
			mu.Lock()
			got = append(got, p.Version)
			mu.Unlock()
		}
	})
	before := policies(t, s).Version
	ctx := context.Background()
	must(t, ts.SetTier(ctx, keyEnt, "tier-4"))
	st := get(t, s, keyEnt)
	if st.Tier != "tier-4" || st.Status != limits.StatusActive {
		t.Fatalf("Get after SetTier: %+v", st)
	}
	p := policies(t, s)
	if p.Version != before+1 || p.Entities[keyEnt] != "tier-4" || len(p.Entities) != 1 {
		t.Fatalf("policies after SetTier: %+v (before %d)", p, before)
	}
	// The watcher sees the new version before SetTier returns.
	mu.Lock()
	defer mu.Unlock()
	seen := false
	for _, v := range got {
		seen = seen || v == p.Version
	}
	if !seen {
		t.Fatalf("watcher did not get version %d: %v", p.Version, got)
	}
	if lst := list(t, s, "", 0); len(lst) != 1 || lst[0].Tier != "tier-4" {
		t.Fatalf("List after SetTier: %+v", lst)
	}
}

func testSetTierRemove(t T, s limits.Store) {
	ts := tierSetter(t, s)
	ctx := context.Background()
	must(t, ts.SetTier(ctx, keyEnt, "tier-4"))
	must(t, ts.SetTier(ctx, key2Ent, "tier-2"))
	must(t, ts.SetTier(ctx, keyEnt, ""))
	if st := get(t, s, keyEnt); st.Tier != "" {
		t.Fatalf("tier not removed: %+v", st)
	}
	p := policies(t, s)
	if _, ok := p.Entities[keyEnt]; ok || p.Entities[key2Ent] != "tier-2" {
		t.Fatalf("policies after removal: %+v", p.Entities)
	}
}

func testPoliciesSurviveReset(t T, s limits.Store) {
	ts := tierSetter(t, s)
	ctx := context.Background()
	must(t, ts.SetTier(ctx, keyEnt, "tier-4"))
	v := policies(t, s).Version
	must(t, s.Reset(ctx))
	if p := policies(t, s); p.Version != v || p.Entities[keyEnt] != "tier-4" {
		t.Fatalf("Reset must keep cfg:policies: %+v", p)
	}
}

func testResetEntityCounters(t T, s limits.Store) {
	er := entityResetter(t, s)
	ctx := context.Background()
	now := baseTime()
	l := config.Limits{RPM: 1, RPD: 1, Concurrency: 1}
	k1, k2 := sub(keyEnt, l), sub(key2Ent, l)
	mustAllow(t, checkAt(t, s, now, 0, k1), "key-1 first")
	mustAllow(t, checkAt(t, s, now, 0, k2), "key-2 first")
	finish(t, s, limits.FinishRequest{Subjects: subs(sub(keyEnt, config.Limits{})), Now: now, CostMicroUSD: 2_000_000, Status: 200})
	mustDeny(t, checkAt(t, s, now, 0, k1), keyEnt, "rpm", "key-1 exhausted")
	must(t, er.ResetEntity(ctx, keyEnt, true, false))
	st := get(t, s, keyEnt)
	if st.Requests != 0 || st.SpendUSD != 0 {
		t.Fatalf("ResetEntity must zero requests and spend: %+v", st)
	}
	// rpm, rpd and the in-flight slot are all cleared for key-1 only.
	mustAllow(t, checkAt(t, s, now, 0, k1), "key-1 after reset")
	mustDeny(t, checkAt(t, s, now, 0, k2), key2Ent, "rpm", "key-2 untouched")
	if st := get(t, s, key2Ent); st.Requests != 1 {
		t.Fatalf("key-2 requests changed: %+v", st)
	}
}

func testResetEntityKeepsStatus(t T, s limits.Store) {
	er := entityResetter(t, s)
	ts := tierSetter(t, s)
	ctx := context.Background()
	must(t, s.Block(ctx, accountEnt, "r", 0, "admin"))
	must(t, s.Ban(ctx, userEnt, "r", "admin"))
	must(t, s.SetCredits(ctx, accountEnt, 3))
	_, err := s.Strike(ctx, accountEnt, "s", time.Hour)
	must(t, err)
	must(t, ts.SetTier(ctx, keyEnt, "tier-4"))
	for _, e := range []limits.Entity{accountEnt, userEnt, keyEnt} {
		must(t, er.ResetEntity(ctx, e, true, false))
	}
	acct := get(t, s, accountEnt)
	if acct.Status != limits.StatusSuspended || acct.Strikes != 1 || acct.Credits == nil || !approx(*acct.Credits, 3, 1e-9) {
		t.Fatalf("ResetEntity must keep block, strikes and credits: %+v", acct)
	}
	if st := get(t, s, userEnt); st.Status != limits.StatusBanned {
		t.Fatalf("ResetEntity must keep the ban: %+v", st)
	}
	if st := get(t, s, keyEnt); st.Tier != "tier-4" {
		t.Fatalf("ResetEntity must keep the tier: %+v", st)
	}
}

func testResetEntityStrikes(t T, s limits.Store) {
	er := entityResetter(t, s)
	ctx := context.Background()
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 1})
	mustAllow(t, checkAt(t, s, now, 0, ks), "first")
	for range 2 {
		_, err := s.Strike(ctx, keyEnt, "s", time.Hour)
		must(t, err)
	}
	must(t, er.ResetEntity(ctx, keyEnt, false, true))
	st := get(t, s, keyEnt)
	if st.Strikes != 0 {
		t.Fatalf("strikes-only reset: %+v", st)
	}
	mustDeny(t, checkAt(t, s, now, 0, ks), keyEnt, "rpm", "counters kept")
	n, err := s.Strike(ctx, keyEnt, "s", time.Hour)
	must(t, err)
	if n != 1 {
		t.Fatalf("strikes restart at 1, got %d", n)
	}
}

func testResetEntityUnknown(t T, s limits.Store) {
	er := entityResetter(t, s)
	must(t, er.ResetEntity(context.Background(), keyEnt, true, true))
	if st := get(t, s, keyEnt); !st.Since.IsZero() || st.Status != limits.StatusActive {
		t.Fatalf("ResetEntity created the entity: %+v", st)
	}
	if lst := list(t, s, "", 0); len(lst) != 0 {
		t.Fatalf("ResetEntity indexed the entity: %+v", lst)
	}
}
