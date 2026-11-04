package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// ttlShort and ttlWait bound the real-time TTL subtests (spec: no subtest
// sleeps more than 50 ms).
const (
	ttlShort = 15 * time.Millisecond
	ttlWait  = 30 * time.Millisecond
)

func statusCases() []testCase {
	return []testCase{
		{"GetUnknownIsActive", testGetUnknown},
		{"BlockWithDuration", testBlockWithDuration},
		{"BlockExpiresWithoutCheck", testBlockExpires},
		{"BlockUntilUnblock", testBlockForever},
		{"BanAndUnban", testBanUnban},
		{"BlockSurvivesUnban", testBlockSurvivesUnban},
		{"Strikes", testStrikes},
		{"StrikeTTL", testStrikeTTL},
		{"ListByStatusWithLimit", testList},
	}
}

func testGetUnknown(t T, s limits.Store) {
	st := get(t, s, keyEnt)
	if st.Entity != keyEnt || st.Status != limits.StatusActive || st.Strikes != 0 || st.Requests != 0 {
		t.Fatalf("unknown entity: %+v", st)
	}
}

func testBlockWithDuration(t T, s limits.Store) {
	ctx := context.Background()
	start := time.Now()
	must(t, s.Block(ctx, keyEnt, "too many 429", time.Hour, "admin"))
	st := get(t, s, keyEnt)
	if st.Status != limits.StatusSuspended || st.Reason != "too many 429" || st.By != "admin" {
		t.Fatalf("blocked state: %+v", st)
	}
	if st.Until.Before(start.Add(time.Hour-time.Second)) || st.Until.After(time.Now().Add(time.Hour+time.Second)) {
		t.Fatalf("Until = %v, want about now+1h", st.Until)
	}
	if got := list(t, s, limits.StatusSuspended, 0); len(got) != 1 || got[0].Entity != keyEnt {
		t.Fatalf("List(suspended) = %+v", got)
	}
	ks := sub(keyEnt, config.Limits{})
	d := checkAt(t, s, time.Now(), 0, ks)
	mustDeny(t, d, keyEnt, "", "blocked")
	if d.Kind != core.ErrAccountSuspended || d.RetryAfter < 59*time.Minute || d.RetryAfter > time.Hour {
		t.Fatalf("block denial: kind=%s retry=%v", d.Kind, d.RetryAfter)
	}
	if d.Message != "too many 429" {
		t.Fatalf("Message = %q, want the block reason", d.Message)
	}
	mustAllow(t, checkAt(t, s, time.Now().Add(2*time.Hour), 0, ks), "block expired")
}

func testBlockExpires(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Block(ctx, keyEnt, "short", ttlShort, "admin"))
	time.Sleep(ttlWait)
	if st := get(t, s, keyEnt); st.Status != limits.StatusActive {
		t.Fatalf("status after block expiry = %s, want active", st.Status)
	}
	if got := list(t, s, limits.StatusSuspended, 0); len(got) != 0 {
		t.Fatalf("List(suspended) after expiry = %+v", got)
	}
}

func testBlockForever(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Block(ctx, keyEnt, "manual", 0, "admin"))
	if st := get(t, s, keyEnt); st.Status != limits.StatusSuspended || !st.Until.IsZero() {
		t.Fatalf("block without duration: %+v", st)
	}
	ks := sub(keyEnt, config.Limits{})
	d := checkAt(t, s, time.Now().Add(365*24*time.Hour), 0, ks)
	mustDeny(t, d, keyEnt, "", "block without duration never expires")
	if d.Kind != core.ErrAccountSuspended || d.RetryAfter < 0 {
		t.Fatalf("block denial: %+v", d)
	}
	must(t, s.Unban(ctx, keyEnt)) // Unban does not lift a block.
	mustDeny(t, checkAt(t, s, time.Now(), 0, ks), keyEnt, "", "still blocked after Unban")
	must(t, s.Unblock(ctx, keyEnt))
	mustAllow(t, checkAt(t, s, time.Now(), 0, ks), "unblocked")
	if st := get(t, s, keyEnt); st.Status != limits.StatusActive || st.Reason != "" {
		t.Fatalf("after Unblock: %+v", st)
	}
}

func testBanUnban(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Ban(ctx, accountEnt, "leaked key", "panel"))
	st := get(t, s, accountEnt)
	if st.Status != limits.StatusBanned || st.Reason != "leaked key" || st.By != "panel" {
		t.Fatalf("banned state: %+v", st)
	}
	as := sub(accountEnt, config.Limits{})
	d := checkAt(t, s, time.Now().Add(365*24*time.Hour), 0, as)
	mustDeny(t, d, accountEnt, "", "ban is permanent")
	if d.Kind != core.ErrAccountBanned {
		t.Fatalf("ban kind = %s", d.Kind)
	}
	must(t, s.Unblock(ctx, accountEnt)) // Unblock does not lift a ban.
	mustDeny(t, checkAt(t, s, time.Now(), 0, as), accountEnt, "", "still banned after Unblock")
	must(t, s.Unban(ctx, accountEnt))
	mustAllow(t, checkAt(t, s, time.Now(), 0, as), "unbanned")
	if st := get(t, s, accountEnt); st.Status != limits.StatusActive {
		t.Fatalf("after Unban: %+v", st)
	}
}

// Blocks and bans are independent (block:{id} and ban:{id} in spec 012):
// lifting the ban keeps a block that is still running.
func testBlockSurvivesUnban(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Block(ctx, keyEnt, "429 storm", time.Hour, "abuse"))
	must(t, s.Ban(ctx, keyEnt, "leak", "admin"))
	must(t, s.Unban(ctx, keyEnt))
	d := checkAt(t, s, time.Now(), 0, sub(keyEnt, config.Limits{}))
	if d.Allowed || d.Kind != core.ErrAccountSuspended {
		t.Fatalf("after Unban the 1h block must still apply: %+v", d)
	}
}

func testStrikes(t T, s limits.Store) {
	ctx := context.Background()
	for want := 1; want <= 3; want++ {
		n, err := s.Strike(ctx, accountEnt, "moderation", time.Hour)
		must(t, err)
		if n != want {
			t.Fatalf("strike %d returned %d", want, n)
		}
	}
	n, err := s.Strike(ctx, key2Ent, "moderation", time.Hour)
	must(t, err)
	if n != 1 {
		t.Fatalf("strikes are per entity: got %d", n)
	}
	if st := get(t, s, accountEnt); st.Strikes != 3 {
		t.Fatalf("Get().Strikes = %d, want 3", st.Strikes)
	}
}

func testStrikeTTL(t T, s limits.Store) {
	ctx := context.Background()
	_, err := s.Strike(ctx, keyEnt, "429", ttlShort)
	must(t, err)
	time.Sleep(ttlWait)
	n, err := s.Strike(ctx, keyEnt, "429", ttlShort)
	must(t, err)
	if n != 1 {
		t.Fatalf("expired strike still counted: %d", n)
	}
}

func testList(t T, s limits.Store) {
	ctx := context.Background()
	for i := range 3 {
		must(t, s.Block(ctx, limits.Entity{Kind: limits.KindKey, ID: fmt.Sprintf("blk-%d", i)}, "r", time.Hour, "admin"))
	}
	for i := range 2 {
		must(t, s.Ban(ctx, limits.Entity{Kind: limits.KindIP, ID: fmt.Sprintf("10.0.0.%d", i)}, "r", "admin"))
	}
	if got := list(t, s, limits.StatusSuspended, 0); len(got) != 3 {
		t.Fatalf("List(suspended, 0) = %d, want 3", len(got))
	}
	if got := list(t, s, limits.StatusSuspended, 2); len(got) != 2 {
		t.Fatalf("List(suspended, 2) = %d, want 2", len(got))
	}
	banned := list(t, s, limits.StatusBanned, 10)
	if len(banned) != 2 {
		t.Fatalf("List(banned) = %d, want 2", len(banned))
	}
	for _, st := range banned {
		if st.Status != limits.StatusBanned || st.Entity.Kind != limits.KindIP {
			t.Fatalf("List(banned) item: %+v", st)
		}
	}
	if got := list(t, s, "", 0); len(got) < 5 {
		t.Fatalf("List(all) = %d, want >= 5", len(got))
	}
	if got := list(t, s, limits.StatusLimited, 0); len(got) != 0 {
		t.Fatalf("List(limited) = %+v, want none", got)
	}
}

func attemptsCases() []testCase {
	return []testCase{
		{"IncrementPerConversationAndStep", testAttempts},
		{"TTL", testAttemptsTTL},
	}
}

func attempts(t T, s limits.Store, conv, step string, ttl time.Duration) int {
	t.Helper()
	n, err := s.Attempts(context.Background(), conv, step, ttl)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	return n
}

func testAttempts(t T, s limits.Store) {
	for want := 1; want <= 3; want++ {
		if n := attempts(t, s, "conv-a", "step-1", time.Hour); n != want {
			t.Fatalf("attempt %d = %d", want, n)
		}
	}
	if n := attempts(t, s, "conv-a", "step-2", time.Hour); n != 1 {
		t.Fatalf("other step = %d, want 1", n)
	}
	if n := attempts(t, s, "conv-b", "step-1", time.Hour); n != 1 {
		t.Fatalf("other conversation = %d, want 1", n)
	}
}

func testAttemptsTTL(t T, s limits.Store) {
	attempts(t, s, "conv", "step", ttlShort)
	attempts(t, s, "conv", "step", ttlShort)
	time.Sleep(ttlWait)
	if n := attempts(t, s, "conv", "step", ttlShort); n != 1 {
		t.Fatalf("after TTL = %d, want 1", n)
	}
}

func resetCases() []testCase {
	return []testCase{
		{"ResetClearsEverything", testReset},
		{"ResetCountersKeepsStatus", testResetCounters},
		{"ResetConversations", testResetConversations},
		{"KindNotEmpty", testKind},
	}
}

func testKind(t T, s limits.Store) {
	if s.Kind() == "" {
		t.Fatal("Kind() is empty")
	}
}

func testReset(t T, s limits.Store) {
	ctx := context.Background()
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 1})
	mustAllow(t, checkAt(t, s, now, 0, ks), "first")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200})
	must(t, s.Block(ctx, accountEnt, "r", 0, "admin"))
	must(t, s.Ban(ctx, userEnt, "r", "admin"))
	attempts(t, s, "conv", "step", time.Hour)
	must(t, s.Reset(ctx))
	mustAllow(t, checkAt(t, s, now, 0, ks), "rpm counter cleared")
	if st := get(t, s, accountEnt); st.Status != limits.StatusActive {
		t.Fatalf("block survived Reset: %+v", st)
	}
	if st := get(t, s, userEnt); st.Status != limits.StatusActive {
		t.Fatalf("ban survived Reset: %+v", st)
	}
	if n := attempts(t, s, "conv", "step", time.Hour); n != 1 {
		t.Fatalf("attempts survived Reset: %d", n)
	}
	if evs := events(t, s, 0); len(evs) != 0 {
		t.Fatalf("events survived Reset: %s", eventTypes(evs))
	}
}

func testResetCounters(t T, s limits.Store) {
	cr, ok := s.(limits.CounterResetter)
	if !ok {
		t.Skip("store does not implement limits.CounterResetter")
	}
	ctx := context.Background()
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 1, Concurrency: 1})
	mustAllow(t, checkAt(t, s, now, 0, ks), "first (holds the slot)")
	finish(t, s, limits.FinishRequest{Subjects: subs(sub(key2Ent, config.Limits{})), Now: now, CostMicroUSD: 1_000_000, Status: 200})
	must(t, s.Block(ctx, accountEnt, "r", 0, "admin"))
	_, err := s.Strike(ctx, accountEnt, "s", time.Hour)
	must(t, err)
	must(t, s.SetCredits(ctx, accountEnt, 3))
	must(t, cr.ResetCounters(ctx))

	acct := get(t, s, accountEnt)
	if acct.Status != limits.StatusSuspended || acct.Strikes != 1 || acct.Credits == nil || !approx(*acct.Credits, 3, 1e-9) {
		t.Fatalf("ResetCounters must keep block, strikes and credits: %+v", acct)
	}
	if st := get(t, s, key2Ent); st.SpendUSD != 0 || st.Requests != 0 {
		t.Fatalf("ResetCounters must zero spend/requests: %+v", st)
	}
	if st := get(t, s, keyEnt); st.Requests != 0 {
		t.Fatalf("ResetCounters must zero requests: %+v", st)
	}
	// The rpm window is cleared, but the in-flight slot is kept.
	mustDeny(t, checkAt(t, s, now, 0, ks), keyEnt, "concurrency", "in-flight slot kept")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200})
	mustAllow(t, checkAt(t, s, now, 0, ks), "rpm window was reset and slot released")
}

func testResetConversations(t T, s limits.Store) {
	cr, ok := s.(limits.ConversationResetter)
	if !ok {
		t.Skip("store does not implement limits.ConversationResetter")
	}
	ctx := context.Background()
	attempts(t, s, "c1", "a", time.Hour)
	attempts(t, s, "c1", "b", time.Hour)
	attempts(t, s, "c2", "a", time.Hour)
	if n, err := cr.ResetConversations(ctx, "c1"); err != nil || n != 1 {
		t.Fatalf("ResetConversations(c1) = %d, %v; want 1", n, err)
	}
	if n, err := cr.ResetConversations(ctx, "missing"); err != nil || n != 0 {
		t.Fatalf("ResetConversations(missing) = %d, %v; want 0", n, err)
	}
	if n := attempts(t, s, "c1", "a", time.Hour); n != 1 {
		t.Fatalf("c1 not reset: %d", n)
	}
	if n := attempts(t, s, "c2", "a", time.Hour); n != 2 {
		t.Fatalf("c2 must be kept: %d", n)
	}
	if n, err := cr.ResetConversations(ctx, ""); err != nil || n != 2 {
		t.Fatalf("ResetConversations(all) = %d, %v; want 2", n, err)
	}
	if n := attempts(t, s, "c2", "a", time.Hour); n != 1 {
		t.Fatalf("c2 not reset by all: %d", n)
	}
}

func eventsCases() []testCase {
	return []testCase{
		{"FinishEmitsRequestAndError", testEventsFinish},
		{"DenialEmitsErrorWithoutCounters", testEventsDenial},
		{"EntityIsKeyWithKeyAndIP", testEventsEntity},
		{"UsageFieldsForPanel", testEventsUsageFields},
		{"StatusChangesEmit", testEventsStatus},
		{"NewestFirstAndLimit", testEventsLimit},
	}
}

func testEventsFinish(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{})
	finish(t, s, limits.FinishRequest{Subjects: subs(ks, sub(accountEnt, config.Limits{})), RequestID: "req-1", Now: now, Status: 200, Model: "gpt-x", Driver: "openai", Endpoint: "messages"})
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), RequestID: "req-2", Now: now.Add(time.Second), Status: 429, ErrorKind: core.ErrRateLimit, Model: "gpt-x", Driver: "openai", Endpoint: "responses"})
	evs := events(t, s, 10)
	if len(evs) != 2 {
		t.Fatalf("events = %s, want 2", eventTypes(evs))
	}
	e, r := evs[0], evs[1]
	if e.Type != "error" || e.Error != string(core.ErrRateLimit) || e.Status != 429 {
		t.Fatalf("error event: %+v", e)
	}
	if r.Type != "request" || r.Status != 200 || r.Driver != "openai" || r.Model != "gpt-x" {
		t.Fatalf("request event: %+v", r)
	}
	if r.Endpoint != "messages" || e.Endpoint != "responses" {
		t.Fatalf("endpoint: request %q, error %q", r.Endpoint, e.Endpoint)
	}
	if r.Entity == nil || *r.Entity != keyEnt {
		t.Fatalf("request event entity = %v, want the first subject %v", r.Entity, keyEnt)
	}
	if d := r.Time.Sub(now); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("event time = %v, want FinishRequest.Now %v", r.Time, now)
	}
}

// testEventsDenial: a request refused by Check is published as an error
// event (Finish with Denial) and does not release concurrency, refund
// tokens or debit spend.
func testEventsDenial(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Concurrency: 1, RPM: 1})
	as := sub(accountEnt, config.Limits{})
	mustAllow(t, checkAt(t, s, now, 0, ks, as), "first request")
	d := checkAt(t, s, now, 0, ks, as)
	if d.Allowed {
		t.Fatalf("second request allowed: %+v", d)
	}
	finish(t, s, limits.FinishRequest{
		Subjects: subs(ks, as), RequestID: "req-denied", Now: now, Status: 429, ErrorKind: d.Kind,
		CostMicroUSD: 1_000_000, Model: "gpt-x", Driver: "openai", TestID: "t-1",
		Denial: &limits.Denial{Entity: d.Entity, Limit: d.LimitName},
	})
	mustDeny(t, checkAt(t, s, now, 0, ks, as), keyEnt, d.LimitName, "slot still held after a denial Finish")
	if st := get(t, s, accountEnt); st.SpendUSD != 0 {
		t.Fatalf("denial debited spend: %+v", st)
	}
	evs := events(t, s, 10)
	if len(evs) != 1 {
		t.Fatalf("events = %s, want 1", eventTypes(evs))
	}
	e := evs[0]
	if e.Type != "error" || e.Status != 429 || e.Error != string(d.Kind) || e.RequestID != "req-denied" || e.TestID != "t-1" || e.Driver != "openai" || e.Model != "gpt-x" {
		t.Fatalf("denial event: %+v", e)
	}
	if e.Entity == nil || *e.Entity != keyEnt {
		t.Fatalf("denial event entity = %v, want %v", e.Entity, keyEnt)
	}
	if e.Fields["limit"] != d.LimitName || e.Fields["denied"] != true {
		t.Fatalf("denial event fields = %v", e.Fields)
	}
	for _, k := range []string{"duration_ms", "prompt_tokens", "output_tokens"} {
		if _, ok := e.Fields[k]; !ok {
			t.Fatalf("denial event without fields.%s (asyncapi): %v", k, e.Fields)
		}
	}
}

// testEventsEntity: request/error events point entity at the key (or the IP
// when there is no key) and carry key and ip explicitly, in any subject
// order; a denial keeps entity = who denied and still carries key/ip
// (task 066).
func testEventsEntity(t T, s limits.Store) {
	now := baseTime()
	ips, ks, as := sub(ipEnt, config.Limits{}), sub(keyEnt, config.Limits{}), sub(accountEnt, config.Limits{})
	finish(t, s, limits.FinishRequest{Subjects: subs(ips, ks, as), RequestID: "r-ip-key", Now: now, Status: 200, Driver: "openai"})
	finish(t, s, limits.FinishRequest{Subjects: subs(ips, sub(userEnt, config.Limits{})), RequestID: "r-ip", Now: now.Add(time.Second), Status: 200, Driver: "openai"})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), RequestID: "r-acct", Now: now.Add(2 * time.Second), Status: 200, Driver: "openai"})
	finish(t, s, limits.FinishRequest{
		Subjects: subs(ips, ks, as), RequestID: "r-denied", Now: now.Add(3 * time.Second), Status: 429, ErrorKind: core.ErrQuota, Driver: "openai",
		Denial: &limits.Denial{Entity: accountEnt, Limit: "spend"},
	})
	evs := events(t, s, 10)
	if len(evs) != 4 {
		t.Fatalf("events = %s, want 4", eventTypes(evs))
	}
	want := []struct {
		id      string
		entity  limits.Entity
		key, ip string
	}{
		{"r-denied", accountEnt, keyEnt.ID, ipEnt.ID},
		{"r-acct", accountEnt, "", ""},
		{"r-ip", ipEnt, "", ipEnt.ID},
		{"r-ip-key", keyEnt, keyEnt.ID, ipEnt.ID},
	}
	for i, w := range want {
		e := evs[i]
		if e.RequestID != w.id || e.Entity == nil || *e.Entity != w.entity || e.Key != w.key || e.IP != w.ip {
			t.Errorf("event %d = %+v (entity %v), want %s entity %v key %q ip %q", i, e, e.Entity, w.id, w.entity, w.key, w.ip)
		}
	}
}

func testEventsStatus(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Block(ctx, keyEnt, "r", time.Hour, "admin"))
	must(t, s.Ban(ctx, accountEnt, "r", "admin"))
	_, err := s.Strike(ctx, userEnt, "r", time.Hour)
	must(t, err)
	evs := events(t, s, 0)
	if got := eventTypes(evs); got != "strike,ban,block" {
		t.Fatalf("event types = %q, want strike,ban,block", got)
	}
	if evs[2].Entity == nil || *evs[2].Entity != keyEnt || evs[1].Entity == nil || *evs[1].Entity != accountEnt {
		t.Fatalf("event entities: %+v", evs)
	}
}

func testEventsLimit(t T, s limits.Store) {
	now := baseTime()
	for i := range 5 {
		finish(t, s, limits.FinishRequest{Subjects: subs(sub(keyEnt, config.Limits{})), Now: now.Add(time.Duration(i) * time.Second), Status: 200 + i})
	}
	evs := events(t, s, 3)
	if len(evs) != 3 {
		t.Fatalf("Events(3) returned %d", len(evs))
	}
	for i, want := range []int{204, 203, 202} {
		if evs[i].Status != want {
			t.Fatalf("Events(3)[%d].Status = %d, want %d (newest first)", i, evs[i].Status, want)
		}
	}
	if all := events(t, s, 100); len(all) != 5 {
		t.Fatalf("Events(100) returned %d, want 5", len(all))
	}
}

func parallelCases() []testCase {
	return []testCase{
		{"ConcurrencyLimitHeld", testParallelConcurrency},
		{"RPMExactUnderContention", testParallelRPM},
		{"MultiLevelUnderContention", testParallelMultiLevel},
	}
}

func testParallelConcurrency(t T, s limits.Store) {
	const limit, workers, iters = 5, 32, 20
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Concurrency: limit})
	ctx := context.Background()
	var inflight, peak, allowed atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			for range iters {
				d, err := s.Check(ctx, limits.CheckRequest{Subjects: subs(ks), Now: now})
				if err != nil {
					errs <- err
					return
				}
				if !d.Allowed {
					continue
				}
				allowed.Add(1)
				cur := inflight.Add(1)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				// Hold the slot briefly so workers overlap and the limit is
				// really contended.
				time.Sleep(50 * time.Microsecond)
				inflight.Add(-1)
				if err := s.Finish(ctx, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("peak in-flight %d > concurrency %d", p, limit)
	}
	if allowed.Load() == 0 {
		t.Fatal("no request was allowed")
	}
	if n := countAllowed(t, s, limit+2, limits.CheckRequest{Subjects: subs(ks), Now: now}); n != limit {
		t.Fatalf("after all Finish %d slots free, want %d", n, limit)
	}
}

func testParallelRPM(t T, s limits.Store) {
	const limit, workers = 50, 200
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: limit, Algorithm: "fixed_window"})
	if n := parallelAllowed(t, s, workers, limits.CheckRequest{Subjects: subs(ks), Now: now}); n != limit {
		t.Fatalf("allowed %d of %d with rpm %d", n, workers, limit)
	}
}

// Two keys of the same account race; the account limit must hold exactly and
// the key that loses must not leak its own counters.
func testParallelMultiLevel(t T, s limits.Store) {
	const acctLimit, workers = 20, 100
	now := baseTime()
	acct := sub(accountEnt, config.Limits{RPM: acctLimit})
	var wg sync.WaitGroup
	var a1, a2 int
	wg.Go(func() {
		a1 = parallelAllowed(t, s, workers, limits.CheckRequest{Subjects: subs(sub(keyEnt, config.Limits{RPM: 1000}), acct), Now: now})
	})
	wg.Go(func() {
		a2 = parallelAllowed(t, s, workers, limits.CheckRequest{Subjects: subs(sub(key2Ent, config.Limits{RPM: 1000}), acct), Now: now})
	})
	wg.Wait()
	if a1+a2 != acctLimit {
		t.Fatalf("account allowed %d+%d, want %d", a1, a2, acctLimit)
	}
}

func parallelAllowed(t T, s limits.Store, workers int, req limits.CheckRequest) int {
	t.Helper()
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			d, err := s.Check(context.Background(), req)
			if err != nil {
				failed.Add(1)
				return
			}
			if d.Allowed {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if f := failed.Load(); f > 0 {
		t.Errorf("%d Check calls failed", f)
	}
	return int(ok.Load())
}

// testEventsUsageFields: request/error events carry account at the top level
// (explicit, or from the account subject) and, in fields, reasoning_tokens,
// cached_tokens and cost_usd in dollars (zero included), tool_calls as the
// list of names and input_breakdown as the header's object only when set; a
// denial carries none of them (asyncapi 0.4.0, ADR 0032, tasks 075 and 105). Fields are compared after a JSON round trip,
// the form the panel reads from ev:stream.
func testEventsUsageFields(t T, s limits.Store) {
	now := baseTime()
	ips, ks, as := sub(ipEnt, config.Limits{}), sub(keyEnt, config.Limits{}), sub(accountEnt, config.Limits{})
	finish(t, s, limits.FinishRequest{
		Subjects: subs(ips, ks), RequestID: "r-full", Now: now, Status: 200, Driver: "openai", Model: "gpt-x",
		Account: "org-carga", ActualPrompt: 120, ActualOutput: 340, ReasoningTokens: 128, CachedTokens: 64,
		CostMicroUSD: 4215, ToolCalls: []string{"read_file", "read_file", "run_tests"}, DurationMS: 812,
		InputBreakdown: &limits.InputBreakdown{System: 40, ToolSchemas: 30, User: 20, Assistant: 10, ToolCalls: 8, ToolResults: 12, Cached: 64, Total: 120},
	})
	finish(t, s, limits.FinishRequest{Subjects: subs(ks, as), RequestID: "r-plain", Now: now.Add(time.Second), Status: 200, Driver: "openai", ActualPrompt: 5, ActualOutput: 6})
	finish(t, s, limits.FinishRequest{
		Subjects: subs(ks, as), RequestID: "r-denied", Now: now.Add(2 * time.Second), Status: 429, ErrorKind: core.ErrRateLimit, Driver: "openai",
		Account: "org-carga", ReasoningTokens: 1, CachedTokens: 1, CostMicroUSD: 1, ToolCalls: []string{"x"},
		InputBreakdown: &limits.InputBreakdown{User: 1, Total: 1},
		Denial:         &limits.Denial{Entity: keyEnt, Limit: "rpm"},
	})
	evs := events(t, s, 10)
	if len(evs) != 3 {
		t.Fatalf("events = %s, want 3", eventTypes(evs))
	}
	want := []struct {
		id      string
		account string
		fields  map[string]any
	}{
		{"r-denied", "org-carga", map[string]any{"duration_ms": 0, "prompt_tokens": 0, "output_tokens": 0, "denied": true, "limit": "rpm"}},
		{"r-plain", accountEnt.ID, map[string]any{"duration_ms": 0, "prompt_tokens": 5, "output_tokens": 6, "reasoning_tokens": 0, "cached_tokens": 0, "cost_usd": 0}},
		{"r-full", "org-carga", map[string]any{
			"duration_ms": 812, "prompt_tokens": 120, "output_tokens": 340, "reasoning_tokens": 128, "cached_tokens": 64,
			"cost_usd": 0.004215, "tool_calls": []string{"read_file", "read_file", "run_tests"},
			"input_breakdown": map[string]any{"system": 40, "tool_schemas": 30, "user": 20, "assistant": 10, "tool_calls": 8, "tool_results": 12, "images": 0, "cached": 64, "total": 120},
		}},
	}
	for i, w := range want {
		e := evs[i]
		if e.RequestID != w.id || e.Account != w.account {
			t.Errorf("event %d = %s account %q, want %s account %q", i, e.RequestID, e.Account, w.id, w.account)
		}
		if got, exp := jsonValue(t, e.Fields), jsonValue(t, w.fields); !reflect.DeepEqual(got, exp) {
			t.Errorf("event %s fields = %v, want %v", w.id, got, exp)
		}
	}
}

// jsonValue returns v after a JSON round trip.
func jsonValue(t T, v any) any {
	t.Helper()
	var out any
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &out) // cannot fail on Marshal's output
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}
