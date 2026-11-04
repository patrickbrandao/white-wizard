package storetest

import (
	"context"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Prompt-cache registry (cache/valkey/README.md 2.4 and 3.14, ADR 0046),
// exercised only when the store implements limits.PromptCacher.

func promptCacheCases() []testCase {
	return []testCase{
		{"MissThenHit", testPCMissThenHit},
		{"Expiry", testPCExpiry},
		{"RenewAndDeadline", testPCRenewAndDeadline},
		{"TTLNeverShrinks", testPCTTLNeverShrinks},
		{"AccountsIsolated", testPCAccountsIsolated},
		{"ResetClears", testPCResetClears},
		{"ZeroTTL", testPCZeroTTL},
	}
}

const (
	pcH1 = "00000000000000000000000000000001"
	pcH2 = "00000000000000000000000000000002"
	pcH3 = "00000000000000000000000000000003"
	pcH4 = "00000000000000000000000000000004"
)

func promptCacher(t T, s limits.Store) limits.PromptCacher {
	t.Helper()
	pc, ok := s.(limits.PromptCacher)
	if !ok {
		t.Skip("store does not implement limits.PromptCacher")
	}
	return pc
}

func pcLookup(t T, pc limits.PromptCacher, a limits.Entity, now time.Time, want int, hashes ...string) {
	t.Helper()
	got, err := pc.PromptCacheLookup(context.Background(), a, hashes, now)
	if err != nil {
		t.Fatalf("PromptCacheLookup: %v", err)
	}
	if got != want {
		t.Errorf("PromptCacheLookup(%v) at %v = %d, want %d", hashes, now.Format(time.TimeOnly), got, want)
	}
}

func pcCommit(t T, pc limits.PromptCacher, a limits.Entity, now time.Time, want int, entries ...limits.PromptCacheEntry) {
	t.Helper()
	got, err := pc.PromptCacheCommit(context.Background(), a, entries, now)
	if err != nil {
		t.Fatalf("PromptCacheCommit: %v", err)
	}
	if got != want {
		t.Errorf("PromptCacheCommit created %d, want %d", got, want)
	}
}

func pcE(h string, ttl, life time.Duration) limits.PromptCacheEntry {
	return limits.PromptCacheEntry{Hash: h, TTL: ttl, MaxLife: life}
}

// testPCMissThenHit: nothing is cached until a commit; the lookup answers
// the longest live prefix.
func testPCMissThenHit(t T, s limits.Store) {
	pc := promptCacher(t, s)
	now := baseTime()
	pcLookup(t, pc, accountEnt, now, -1, pcH1, pcH2, pcH3)
	pcLookup(t, pc, accountEnt, now, -1)
	pcCommit(t, pc, accountEnt, now, 0)
	pcCommit(t, pc, accountEnt, now, 3, pcE(pcH1, 5*time.Minute, 0), pcE(pcH2, 5*time.Minute, 0), pcE(pcH3, 5*time.Minute, 0))
	pcLookup(t, pc, accountEnt, now.Add(time.Second), 2, pcH1, pcH2, pcH3, pcH4)
	pcLookup(t, pc, accountEnt, now.Add(time.Second), 1, pcH4, pcH2, pcH4)
	pcLookup(t, pc, accountEnt, now.Add(time.Second), -1, pcH4)
}

// testPCExpiry: an entry lives exactly its TTL after the commit.
func testPCExpiry(t T, s limits.Store) {
	pc := promptCacher(t, s)
	now := baseTime()
	pcCommit(t, pc, accountEnt, now, 2, pcE(pcH1, 2*time.Second, 0), pcE(pcH2, time.Second, 0))
	pcLookup(t, pc, accountEnt, now.Add(999*time.Millisecond), 1, pcH1, pcH2)
	pcLookup(t, pc, accountEnt, now.Add(time.Second), 0, pcH1, pcH2)
	pcLookup(t, pc, accountEnt, now.Add(2*time.Second), -1, pcH1, pcH2)
	// A dead entry is created again.
	pcCommit(t, pc, accountEnt, now.Add(3*time.Second), 1, pcE(pcH2, time.Second, 0))
	pcLookup(t, pc, accountEnt, now.Add(3*time.Second), 1, pcH1, pcH2)
}

// testPCRenewAndDeadline: a commit renews a live entry by its TTL, never
// past max_life counted from the creation.
func testPCRenewAndDeadline(t T, s limits.Store) {
	pc := promptCacher(t, s)
	t0 := baseTime()
	e := pcE(pcH1, 10*time.Minute, time.Hour)
	pcCommit(t, pc, accountEnt, t0, 1, e)
	pcCommit(t, pc, accountEnt, t0.Add(9*time.Minute), 0, e)
	pcLookup(t, pc, accountEnt, t0.Add(18*time.Minute), 0, pcH1)
	pcLookup(t, pc, accountEnt, t0.Add(19*time.Minute), -1, pcH1)
	short := pcE(pcH1, 10*time.Minute, 15*time.Minute)
	pcCommit(t, pc, accountEnt, t0.Add(55*time.Minute), 1, short) // dead: new life, deadline t0+70m
	pcCommit(t, pc, accountEnt, t0.Add(64*time.Minute), 0, short)
	pcLookup(t, pc, accountEnt, t0.Add(69*time.Minute), 0, pcH1)
	pcLookup(t, pc, accountEnt, t0.Add(70*time.Minute), -1, pcH1)
}

// testPCTTLNeverShrinks: renewing with a shorter TTL keeps the longer one.
func testPCTTLNeverShrinks(t T, s limits.Store) {
	pc := promptCacher(t, s)
	t0 := baseTime()
	pcCommit(t, pc, accountEnt, t0, 1, pcE(pcH1, time.Hour, 0))
	pcCommit(t, pc, accountEnt, t0.Add(time.Minute), 0, pcE(pcH1, 5*time.Minute, 0))
	pcLookup(t, pc, accountEnt, t0.Add(60*time.Minute), 0, pcH1)
	pcCommit(t, pc, accountEnt, t0.Add(60*time.Minute), 0, pcE(pcH1, 5*time.Minute, 0))
	pcLookup(t, pc, accountEnt, t0.Add(119*time.Minute), 0, pcH1)
	pcLookup(t, pc, accountEnt, t0.Add(120*time.Minute), -1, pcH1)
}

// testPCAccountsIsolated: accounts never share entries.
func testPCAccountsIsolated(t T, s limits.Store) {
	pc := promptCacher(t, s)
	now := baseTime()
	pcCommit(t, pc, accountEnt, now, 1, pcE(pcH1, time.Minute, 0))
	pcCommit(t, pc, ipEnt, now, 1, pcE(pcH2, time.Minute, 0))
	pcLookup(t, pc, ipEnt, now, -1, pcH1)
	pcLookup(t, pc, accountEnt, now, -1, pcH2)
	pcLookup(t, pc, ipEnt, now, 1, pcH1, pcH2)
	pcLookup(t, pc, accountEnt, now, 0, pcH1, pcH2)
}

// testPCResetClears: Reset drops the registry.
func testPCResetClears(t T, s limits.Store) {
	pc := promptCacher(t, s)
	now := baseTime()
	pcCommit(t, pc, accountEnt, now, 1, pcE(pcH1, time.Minute, 0))
	if err := s.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	pcLookup(t, pc, accountEnt, now, -1, pcH1)
}

// testPCZeroTTL: an entry with no TTL is dead at once and leaves nothing.
func testPCZeroTTL(t T, s limits.Store) {
	pc := promptCacher(t, s)
	now := baseTime()
	pcCommit(t, pc, accountEnt, now, 1, pcE(pcH1, 0, 0))
	pcLookup(t, pc, accountEnt, now, -1, pcH1)
	pcCommit(t, pc, accountEnt, now, 1, pcE(pcH1, time.Minute, 0))
	pcLookup(t, pc, accountEnt, now, 0, pcH1)
}
