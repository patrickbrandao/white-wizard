// Package storetest is the shared behavioural suite of limits.Store
// (specs 010 and 012). Every Store implementation (memory, Valkey) must pass
// the same suite, so it is the owner of the store contract.
//
// Usage, in a _test.go file of the implementation:
//
//	func TestValkeyStore(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) limits.Store {
//			s := newValkeyStore(t) // connect, FLUSHDB
//			t.Cleanup(func() { _ = s.Close() })
//			return s
//		})
//	}
//
// Contract of Run:
//
//   - newStore is called once per subtest and must return a clean, isolated
//     store (empty counters, statuses, attempts and events). The constructor
//     owns isolation (e.g. FLUSHDB on a dedicated database) and teardown
//     (t.Cleanup calling Close). Subtests never run in parallel with each
//     other, so a shared backend database is fine.
//   - Rate-limit time is controlled by CheckRequest.Now and FinishRequest.Now;
//     the store must use them instead of the wall clock for windows, buckets
//     and spend. Block, Strike and Attempts have no Now in their signature, so
//     their TTL subtests use real time with sleeps of at most 30 ms.
//   - The optional interfaces limits.CounterResetter and limits.Escalator,
//     limits.ConversationResetter, limits.TierSetter, limits.EntityResetter,
//     limits.PolicySource, limits.PromptCacher, limits.CreditAdder,//     limits.TierSwapper and limits.ResponseRegistry are exercised only
//     when the store implements them; otherwise those subtests are skipped.
//   - KnownBug(name, reason) skips one subtest (by its full path relative to
//     Run, e.g. "MultiLevel/RollbackWindowCounters") for a single
//     implementation while the bug is tracked in a task. Remove the option
//     when the bug is fixed.
//
// The suite needs no network by itself and runs in well under a second
// against the memory store; run it with -race.
package storetest

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Factory builds a clean store for one subtest.
type Factory func(t *testing.T) limits.Store

// Option customizes Run.
type Option func(*options)

type options struct {
	bugs map[string]string
}

// KnownBug skips the subtest whose path (relative to Run, segments joined by
// "/") is name, citing reason (which should reference the task that tracks
// the fix).
func KnownBug(name, reason string) Option {
	return func(o *options) { o.bugs[name] = reason }
}

// T is the subset of *testing.T the subtests use. The suite's own tests
// pass a fake T to check that every assertion fires against broken stores.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Skip(args ...any)
}

var _ T = (*testing.T)(nil)

type testCase struct {
	name string
	fn   func(t T, s limits.Store)
}

type group struct {
	name  string
	cases []testCase
}

// Run executes the whole suite against stores built by newStore.
func Run(t *testing.T, newStore Factory, opts ...Option) {
	t.Helper()
	o := options{bugs: map[string]string{}}
	for _, opt := range opts {
		opt(&o)
	}
	groups := suite()
	checkBugs(t, groups, o.bugs)
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					if reason, ok := o.bugs[g.name+"/"+c.name]; ok {
						t.Skip(reason)
					}
					c.fn(t, newStore(t))
				})
			}
		})
	}
}

// suite lists the groups of subtests in execution order.
func suite() []group {
	return []group{
		{"RateLimits", rateLimitCases()},
		{"Concurrency", concurrencyCases()},
		{"MultiLevel", multiLevelCases()},
		{"Spend", spendCases()},
		{"Status", statusCases()},
		{"Attempts", attemptsCases()},
		{"Reset", resetCases()},
		{"Entity", entityCases()},
		{"Events", eventsCases()},
		{"Parallel", parallelCases()},
		{"PromptCache", promptCacheCases()},
		{"Accounts", accountCases()},
		{"Responses", responseCases()},
		{"ProjectTokens", projectCases()},
		{"Escalation", escalationCases()},
		{"Billing", billingCases()},
	}
}

// checkBugs reports KnownBug names that match no subtest.
func checkBugs(t T, groups []group, bugs map[string]string) {
	t.Helper()
	known := map[string]bool{}
	for _, g := range groups {
		for _, c := range g.cases {
			known[g.name+"/"+c.name] = true
		}
	}
	for name := range bugs {
		if !known[name] {
			t.Errorf("storetest.KnownBug(%q): no such subtest", name)
		}
	}
}

// Fixed entities used by the subtests (the store is clean for each one).
var (
	ipEnt      = limits.Entity{Kind: limits.KindIP, ID: "203.0.113.7"}
	keyEnt     = limits.Entity{Kind: limits.KindKey, ID: "key-1"}
	key2Ent    = limits.Entity{Kind: limits.KindKey, ID: "key-2"}
	userEnt    = limits.Entity{Kind: limits.KindUser, ID: "user-1"}
	accountEnt = limits.Entity{Kind: limits.KindAccount, ID: "acct-1"}
)

// baseTime is the start of the next wall-clock minute, so fixed windows of a
// minute begin exactly at base and backends that also use the wall clock for
// key TTLs see plausible timestamps.
func baseTime() time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
}

// nextUTCMidnight returns the next 00:00 UTC after now.
func nextUTCMidnight() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
}

func sub(e limits.Entity, l config.Limits) limits.Subject {
	return limits.Subject{Entity: e, Limits: l}
}

func subs(ss ...limits.Subject) []limits.Subject { return ss }

func check(t T, s limits.Store, req limits.CheckRequest) limits.Decision {
	t.Helper()
	d, err := s.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return d
}

// checkAt checks a single subject at now with prompt tokens.
func checkAt(t T, s limits.Store, now time.Time, prompt int, ss ...limits.Subject) limits.Decision {
	t.Helper()
	return check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: prompt, Now: now})
}

func finish(t T, s limits.Store, req limits.FinishRequest) {
	t.Helper()
	if err := s.Finish(context.Background(), req); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func get(t T, s limits.Store, e limits.Entity) limits.EntityState {
	t.Helper()
	st, err := s.Get(context.Background(), e)
	if err != nil {
		t.Fatalf("Get(%v): %v", e, err)
	}
	return st
}

func list(t T, s limits.Store, status limits.Status, limit int) []limits.EntityState {
	t.Helper()
	out, err := s.List(context.Background(), status, limit)
	if err != nil {
		t.Fatalf("List(%q, %d): %v", status, limit, err)
	}
	return out
}

func events(t T, s limits.Store, limit int) []limits.Event {
	t.Helper()
	out, err := s.Events(context.Background(), limit)
	if err != nil {
		t.Fatalf("Events(%d): %v", limit, err)
	}
	return out
}

func must(t T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustAllow(t T, d limits.Decision, what string) {
	t.Helper()
	if !d.Allowed {
		t.Fatalf("%s: expected allowed, got %+v", what, d)
	}
}

// mustDeny asserts a denial by limitName on entity e with the given kind.
func mustDeny(t T, d limits.Decision, e limits.Entity, limitName string, what string) {
	t.Helper()
	if d.Allowed {
		t.Fatalf("%s: expected denied by %s, got allowed %+v", what, limitName, d)
	}
	if limitName != "" && d.LimitName != limitName {
		t.Fatalf("%s: LimitName = %q, want %q (%+v)", what, d.LimitName, limitName, d)
	}
	if d.Entity != e {
		t.Fatalf("%s: Entity = %v, want %v", what, d.Entity, e)
	}
}

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// countAllowed runs n checks at now and returns how many were allowed.
func countAllowed(t T, s limits.Store, n int, req limits.CheckRequest) int {
	t.Helper()
	ok := 0
	for range n {
		if check(t, s, req).Allowed {
			ok++
		}
	}
	return ok
}

func eventTypes(evs []limits.Event) string {
	ts := make([]string, len(evs))
	for i, e := range evs {
		ts[i] = e.Type
	}
	return strings.Join(ts, ",")
}
