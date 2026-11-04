package storetest

import (
	"context"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// escalationCases cover task 258: the escalation record of the abuse
// ladder (limits.Escalator) outlives the strike window, and the spend of
// the UTC day and week, the accumulated spend and the :free requests of
// the day are readable by Get (OpenRouter's /api/v1/key).
func escalationCases() []testCase {
	return []testCase{
		{"EscalateCountsLevelAndStrikes", testEscalateCounts},
		{"EscalateOutlivesStrikeWindow", testEscalateOutlivesWindow},
		{"EscalateExpires", testEscalateExpires},
		{"ResetEntityStrikesDropsEscalation", testResetEntityDropsEscalation},
		{"ResetCountersKeepsEscalation", testResetCountersKeepsEscalation},
		{"SpendWindowsReadByGet", testSpendWindows},
		{"FreeModelRequestsPerDay", testFreeModelRequests},
	}
}

func escalator(t T, s limits.Store) limits.Escalator {
	t.Helper()
	es, ok := s.(limits.Escalator)
	if !ok {
		t.Skip("store does not implement limits.Escalator")
	}
	return es
}

func counterResetter(t T, s limits.Store) limits.CounterResetter {
	t.Helper()
	cr, ok := s.(limits.CounterResetter)
	if !ok {
		t.Skip("store does not implement limits.CounterResetter")
	}
	return cr
}

func escalate(t T, es limits.Escalator, e limits.Entity, escalated bool, ttl time.Duration) limits.Escalation {
	t.Helper()
	got, err := es.Escalate(context.Background(), e, escalated, ttl)
	if err != nil {
		t.Fatalf("Escalate(%v, %v): %v", e, escalated, err)
	}
	return got
}

func wantEscalation(t T, got limits.Escalation, level, strikes int, what string) {
	t.Helper()
	if got.Level != level || got.Strikes != strikes {
		t.Fatalf("%s: %+v, want level %d strikes %d", what, got, level, strikes)
	}
}

func testEscalateCounts(t T, s limits.Store) {
	es := escalator(t, s)
	wantEscalation(t, escalate(t, es, keyEnt, false, time.Hour), 0, 1, "strike without escalation")
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 1, 2, "first escalation")
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 2, 3, "second escalation")
	// Another entity has its own record.
	wantEscalation(t, escalate(t, es, key2Ent, true, time.Hour), 1, 1, "other entity")
}

func testEscalateOutlivesWindow(t T, s limits.Store) {
	es := escalator(t, s)
	ctx := context.Background()
	if n, err := s.Strike(ctx, keyEnt, "rate_limit", 20*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("Strike = %d, %v", n, err)
	}
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 1, 1, "first escalation")
	time.Sleep(30 * time.Millisecond)
	// The strike window restarted; the record did not.
	if n, err := s.Strike(ctx, keyEnt, "rate_limit", 20*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("Strike after the window = %d, %v", n, err)
	}
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 2, 2, "escalation after the window")
}

func testEscalateExpires(t T, s limits.Store) {
	es := escalator(t, s)
	wantEscalation(t, escalate(t, es, keyEnt, true, 20*time.Millisecond), 1, 1, "first escalation")
	time.Sleep(30 * time.Millisecond)
	wantEscalation(t, escalate(t, es, keyEnt, true, 20*time.Millisecond), 1, 1, "after the TTL")
}

func testResetEntityDropsEscalation(t T, s limits.Store) {
	es := escalator(t, s)
	er := entityResetter(t, s)
	must(t, s.SetCredits(context.Background(), keyEnt, 1))
	escalate(t, es, keyEnt, true, time.Hour)
	must(t, er.ResetEntity(context.Background(), keyEnt, true, false))
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 2, 2, "counters reset keeps the record")
	must(t, er.ResetEntity(context.Background(), keyEnt, false, true))
	wantEscalation(t, escalate(t, es, keyEnt, false, time.Hour), 0, 1, "strikes reset drops the record")
}

func testResetCountersKeepsEscalation(t T, s limits.Store) {
	es := escalator(t, s)
	cr := counterResetter(t, s)
	finish(t, s, limits.FinishRequest{Subjects: subs(sub(keyEnt, config.Limits{})), Now: time.Now(), CostMicroUSD: 1, Status: 200})
	escalate(t, es, keyEnt, true, time.Hour)
	must(t, cr.ResetCounters(context.Background()))
	wantEscalation(t, escalate(t, es, keyEnt, true, time.Hour), 2, 2, "after ResetCounters")
}

func testSpendWindows(t T, s limits.Store) {
	// Fixed instants far in the future, so both calendar cases always run and
	// the windows (which keep counting while the reader's clock is behind
	// them) are still read at the real time: a Saturday in mid-month
	// (yesterday in the same week and month) and a Monday on the 1st
	// (yesterday in the previous week and month).
	spendWindowsCase(t, s, time.Date(2090, time.October, 14, 12, 0, 0, 0, time.UTC), 2.75, 2.75)
	spendWindowsCase(t, s, time.Date(2090, time.May, 1, 12, 0, 0, 0, time.UTC), 0.75, 0.75)
}

// spendWindowsCase spends 2 USD one hour before midnight of the day before
// now, then 0.75 USD at now, and expects wantWeek and wantMonth in the weekly
// and monthly windows.
func spendWindowsCase(t T, s limits.Store, now time.Time, wantWeek, wantMonth float64) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-time.Hour)
	kl := sub(keyEnt, config.Limits{})
	finish(t, s, limits.FinishRequest{Subjects: subs(kl), Now: yesterday, CostMicroUSD: 2_000_000, Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: subs(kl), Now: now, CostMicroUSD: 500_000, Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: subs(kl), Now: now, CostMicroUSD: 250_000, Status: 200})
	st := get(t, s, keyEnt)
	if !approx(st.SpendDailyUSD, 0.75, 1e-9) || !approx(st.SpendWeeklyUSD, wantWeek, 1e-9) || !approx(st.SpendUSD, wantMonth, 1e-9) || !approx(st.SpentTotalUSD, 2.75, 1e-9) {
		t.Fatalf("spend at %v: day %v week %v month %v total %v, want 0.75 %v %v 2.75", now, st.SpendDailyUSD, st.SpendWeeklyUSD, st.SpendUSD, st.SpentTotalUSD, wantWeek, wantMonth)
	}
	// A late finish of yesterday never moves the day back.
	finish(t, s, limits.FinishRequest{Subjects: subs(kl), Now: yesterday, CostMicroUSD: 1_000_000, Status: 200})
	if st := get(t, s, keyEnt); !approx(st.SpendDailyUSD, 1.75, 1e-9) || !approx(st.SpentTotalUSD, 3.75, 1e-9) {
		t.Fatalf("after a late finish: day %v total %v, want 1.75 3.75", st.SpendDailyUSD, st.SpentTotalUSD)
	}
	// The counter resets zero every window and the total.
	must(t, counterResetter(t, s).ResetCounters(context.Background()))
	if st := get(t, s, keyEnt); st.SpendDailyUSD != 0 || st.SpendWeeklyUSD != 0 || st.SpentTotalUSD != 0 {
		t.Fatalf("after ResetCounters: %+v", st)
	}
}

func testFreeModelRequests(t T, s limits.Store) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	ss := subs(sub(keyEnt, config.Limits{}), sub(accountEnt, config.Limits{}))
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: today.Add(-time.Minute), Model: "meta/llama:free", Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: now, Model: "meta/llama:free", Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: now, Model: "meta/llama:free", Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: now, Model: "meta/llama", Status: 200, CostMicroUSD: 10})
	finish(t, s, limits.FinishRequest{Subjects: ss, Now: now, Model: "meta/llama:free", Status: 429, Denial: &limits.Denial{Entity: keyEnt, Limit: "rpm"}})
	for _, e := range []limits.Entity{keyEnt, accountEnt} {
		if st := get(t, s, e); st.FreeRequestsToday != 2 {
			t.Fatalf("%v: free requests today = %d, want 2 (yesterday's, the paid model and the denial do not count)", e, st.FreeRequestsToday)
		}
	}
	if st := get(t, s, key2Ent); st.FreeRequestsToday != 0 {
		t.Fatalf("untouched entity: %+v", st)
	}
}
