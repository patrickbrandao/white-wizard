package storetest

import (
	"context"
	"fmt"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func concurrencyCases() []testCase {
	return []testCase{
		{"CheckIncrementsFinishReleases", testConcurrencySlots},
		{"QueueMode", testConcurrencyQueue},
		{"FinishWithoutCheckNeverGoesNegative", testConcurrencyNoNegative},
	}
}

func testConcurrencySlots(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Concurrency: 2})
	mustAllow(t, checkAt(t, s, now, 0, ks), "slot 1")
	mustAllow(t, checkAt(t, s, now, 0, ks), "slot 2")
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "concurrency", "no slot left")
	if d.Kind != core.ErrRateLimit || d.Queue || d.RetryAfter <= 0 {
		t.Fatalf("concurrency reject: %+v", d)
	}
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200})
	mustAllow(t, checkAt(t, s, now, 0, ks), "slot released by Finish")
	mustDeny(t, checkAt(t, s, now, 0, ks), keyEnt, "concurrency", "full again")
}

func testConcurrencyQueue(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Concurrency: 1, OnLimit: "queue", QueueTimeout: 2 * time.Second})
	mustAllow(t, checkAt(t, s, now, 0, ks), "slot")
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "concurrency", "queue")
	if !d.Queue || d.Kind != core.ErrOverloaded || d.RetryAfter != 2*time.Second {
		t.Fatalf("queue decision: %+v", d)
	}
}

func testConcurrencyNoNegative(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Concurrency: 1})
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, Status: 200})
	if n := countAllowed(t, s, 3, limits.CheckRequest{Subjects: subs(ks), Now: now}); n != 1 {
		t.Fatalf("allowed %d with concurrency 1 after stray Finish, want 1", n)
	}
}

func multiLevelCases() []testCase {
	return []testCase{
		{"FirstDenyingLevelDecides", testFirstDenyingLevel},
		{"LevelOrder", testLevelOrder},
		{"RollbackTokenBucket", testRollbackTokenBucket},
		{"RollbackConcurrency", testRollbackConcurrency},
		{"RollbackWithinSubject", testRollbackWithinSubject},
		{"RollbackWindowCounters", testRollbackWindowCounters},
		{"RollbackRequestCounter", testRollbackRequestCounter},
	}
}

func fourLevels(ip, key, user, acct config.Limits) []limits.Subject {
	return subs(sub(ipEnt, ip), sub(keyEnt, key), sub(userEnt, user), sub(accountEnt, acct))
}

func testFirstDenyingLevel(t T, s limits.Store) {
	ctx := context.Background()
	must(t, s.Ban(ctx, userEnt, "abuse", "test"))
	must(t, s.Ban(ctx, accountEnt, "abuse", "test"))
	d := check(t, s, limits.CheckRequest{Subjects: fourLevels(config.Limits{}, config.Limits{}, config.Limits{}, config.Limits{}), Now: time.Now()})
	mustDeny(t, d, userEnt, "", "user is the first banned level")
	if d.Kind != core.ErrAccountBanned {
		t.Fatalf("kind = %s, want %s", d.Kind, core.ErrAccountBanned)
	}
}

func testLevelOrder(t T, s limits.Store) {
	now := baseTime()
	one := config.Limits{RPM: 1}
	ss := fourLevels(config.Limits{}, one, config.Limits{}, one)
	mustAllow(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: now}), "first")
	mustDeny(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: now}), keyEnt, "rpm", "key is evaluated before account")
}

func testRollbackTokenBucket(t T, s limits.Store) {
	ctx := context.Background()
	now := time.Now()
	ss := fourLevels(config.Limits{RPS: 1}, config.Limits{RPM: 1, TPM: 100}, config.Limits{}, config.Limits{})
	must(t, s.Ban(ctx, userEnt, "x", "test"))
	req := limits.CheckRequest{Subjects: ss, EstPromptTokens: 100, Now: now}
	mustDeny(t, check(t, s, req), userEnt, "", "user banned")
	must(t, s.Unban(ctx, userEnt))
	mustAllow(t, check(t, s, req), "ip rps, key rpm and key tpm were rolled back")
	mustDeny(t, check(t, s, req), ipEnt, "rps", "now really spent")
}

func testRollbackConcurrency(t T, s limits.Store) {
	ctx := context.Background()
	now := time.Now()
	ss := fourLevels(config.Limits{}, config.Limits{Concurrency: 1}, config.Limits{}, config.Limits{})
	must(t, s.Block(ctx, accountEnt, "x", 0, "test"))
	mustDeny(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: now}), accountEnt, "", "account blocked")
	must(t, s.Unblock(ctx, accountEnt))
	mustAllow(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: now}), "key slot was rolled back")
	mustDeny(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: now}), keyEnt, "concurrency", "slot now in use")
}

func testRollbackWithinSubject(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 2, TPM: 100})
	mustDeny(t, checkAt(t, s, now, 200, ks), keyEnt, "tpm", "tpm denies after rpm took a token")
	mustAllow(t, checkAt(t, s, now, 10, ks), "rpm token 1 was returned")
	mustAllow(t, checkAt(t, s, now, 10, ks), "rpm token 2")
	mustDeny(t, checkAt(t, s, now, 10, ks), keyEnt, "rpm", "rpm spent")
}

// Window counters (fixed rpm/tpm, rpd, tpd) incremented on earlier levels
// must be reverted when a later level denies (spec 012, step 2).
func testRollbackWindowCounters(t T, s limits.Store) {
	ctx := context.Background()
	now := time.Now()
	key := config.Limits{RPM: 1, TPM: 100, Algorithm: "fixed_window", RPD: 1, TPD: 100}
	ss := fourLevels(config.Limits{}, key, config.Limits{}, config.Limits{})
	req := limits.CheckRequest{Subjects: ss, EstPromptTokens: 100, Now: now}
	must(t, s.Ban(ctx, userEnt, "x", "test"))
	mustDeny(t, check(t, s, req), userEnt, "", "user banned")
	must(t, s.Unban(ctx, userEnt))
	mustAllow(t, check(t, s, req), "key window counters were rolled back")
}

func testRollbackRequestCounter(t T, s limits.Store) {
	ctx := context.Background()
	ss := fourLevels(config.Limits{}, config.Limits{}, config.Limits{}, config.Limits{})
	must(t, s.Ban(ctx, accountEnt, "x", "test"))
	mustDeny(t, check(t, s, limits.CheckRequest{Subjects: ss, Now: time.Now()}), accountEnt, "", "account banned")
	if st := get(t, s, keyEnt); st.Requests != 0 {
		t.Fatalf("key Requests = %d after a denied request, want 0", st.Requests)
	}
}

func spendCases() []testCase {
	return []testCase{
		{"MonthlySpend", testMonthlySpend},
		{"MonthlySpendResetsNextMonth", testMonthlySpendNextMonth},
		{"CreditsDebitAndExhaust", testCreditsDebit},
		{"SetCreditsOverrides", testSetCredits},
		{"NegativeBalanceIsLimited", testNegativeBalanceLimited},
		{"QuotaRolling5h", testQuota5h},
		{"QuotaWeekly", testQuotaWeekly},
		{"QuotaWeeklyTurnsMonday", testQuotaWeeklyMonday},
		{"Quota5hState", testQuota5hState},
		{"QuotaMonthly", testQuotaMonthly},
	}
}

func testMonthlySpend(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{SpendMonthlyUSD: 1})
	mustAllow(t, checkAt(t, s, now, 0, ks), "below budget")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, CostMicroUSD: 1_500_000, Status: 200})
	if st := get(t, s, keyEnt); !approx(st.SpendUSD, 1.5, 1e-9) {
		t.Fatalf("SpendUSD = %v, want 1.5", st.SpendUSD)
	}
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "spend_monthly", "over budget")
	if d.Kind != core.ErrSpendLimit || d.RetryAfter <= 0 {
		t.Fatalf("spend denial: %+v", d)
	}
}

// Spend is per calendar month (UTC): it persists for the whole month and
// resets on the 1st. Twelve consecutive months make the check independent of
// the day the suite runs.
func testMonthlySpendNextMonth(t T, s limits.Store) {
	n := time.Now().UTC()
	for i := 1; i <= 12; i++ {
		month := time.Date(n.Year(), n.Month()+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		next := month.AddDate(0, 1, 0)
		e := limits.Entity{Kind: limits.KindKey, ID: fmt.Sprintf("spend-%d", i)}
		ks := sub(e, config.Limits{SpendMonthlyUSD: 1})
		finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: month.Add(time.Hour), CostMicroUSD: 2_000_000, Status: 200})
		before := next.Add(-time.Hour)
		d := checkAt(t, s, before, 0, ks)
		mustDeny(t, d, e, "spend_monthly", month.Format("2006-01")+": spend lasts the whole month")
		if d.RetryAfter != time.Hour {
			t.Fatalf("%s: RetryAfter = %v, want 1h (until the next month)", month.Format("2006-01"), d.RetryAfter)
		}
		mustAllow(t, checkAt(t, s, next.Add(time.Hour), 0, ks), month.Format("2006-01")+": new calendar month resets spend")
	}
}

func testCreditsDebit(t T, s limits.Store) {
	now := baseTime()
	c := 0.01
	ks := sub(keyEnt, config.Limits{CreditsUSD: &c})
	mustAllow(t, checkAt(t, s, now, 0, ks), "positive balance")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, CostMicroUSD: 8000, Status: 200})
	st := get(t, s, keyEnt)
	if st.Credits == nil || !approx(*st.Credits, 0.002, 1e-9) {
		t.Fatalf("credits after debit = %v, want 0.002", st.Credits)
	}
	mustAllow(t, checkAt(t, s, now, 0, ks), "still positive")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, CostMicroUSD: 4000, Status: 200})
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "credits", "negative balance")
	if d.Kind != core.ErrQuota {
		t.Fatalf("credits kind = %s, want %s", d.Kind, core.ErrQuota)
	}
}

func testSetCredits(t T, s limits.Store) {
	ctx := context.Background()
	now := baseTime()
	c := 10.0
	ks := sub(keyEnt, config.Limits{CreditsUSD: &c})
	must(t, s.SetCredits(ctx, keyEnt, 0))
	mustDeny(t, checkAt(t, s, now, 0, ks), keyEnt, "credits", "SetCredits(0) overrides the configured balance")
	must(t, s.SetCredits(ctx, keyEnt, 5))
	mustAllow(t, checkAt(t, s, now, 0, ks), "topped up")
	if st := get(t, s, keyEnt); st.Credits == nil || !approx(*st.Credits, 5, 1e-9) {
		t.Fatalf("Credits = %v, want 5", st.Credits)
	}
}

// spec 012, abuse policy: negative balance puts the entity in "limited"
// until it is credited.
func testNegativeBalanceLimited(t T, s limits.Store) {
	ctx := context.Background()
	now := baseTime()
	c := 0.001
	ks := sub(keyEnt, config.Limits{CreditsUSD: &c})
	mustAllow(t, checkAt(t, s, now, 0, ks), "positive")
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, CostMicroUSD: 2000, Status: 200})
	if st := get(t, s, keyEnt); st.Status != limits.StatusLimited {
		t.Fatalf("status with negative balance = %s, want limited", st.Status)
	}
	must(t, s.SetCredits(ctx, keyEnt, 1))
	if st := get(t, s, keyEnt); st.Status != limits.StatusActive {
		t.Fatalf("status after credit = %s, want active", st.Status)
	}
}

func testQuota5h(t T, s limits.Store) {
	now := baseTime()
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 20}})
	mustAllow(t, checkAt(t, s, now, 0, as), "fresh quota")
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 2_000_000, Status: 200})
	d := checkAt(t, s, now.Add(time.Hour), 0, as)
	mustDeny(t, d, accountEnt, "quota_5h", "20% of 10 USD spent in 5h")
	if d.Kind != core.ErrQuota || d.RetryAfter <= 0 || d.RetryAfter > 4*time.Hour {
		t.Fatalf("quota_5h denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now.Add(5*time.Hour+time.Minute), 0, as), "rolling window moved past the spend")
}

func testQuotaWeekly(t T, s limits.Store) {
	now := mondayUTC(baseTime()).Add(time.Hour)
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 30, WeeklyPct: 50}})
	for i := range 3 { // 3 × 2 USD spread 6h apart: never 3 USD in 5h, 6 USD in the week.
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now.Add(time.Duration(i) * 6 * time.Hour), CostMicroUSD: 2_000_000, Status: 200})
	}
	d := checkAt(t, s, now.Add(18*time.Hour), 0, as)
	mustDeny(t, d, accountEnt, "quota_weekly", "50% of 10 USD in the week")
	if d.Kind != core.ErrQuota || d.RetryAfter <= 0 {
		t.Fatalf("quota_weekly denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now.Add(8*24*time.Hour), 0, as), "week rolled")
}

// mondayUTC is Monday 00:00 UTC of the week of now.
func mondayUTC(now time.Time) time.Time {
	d := now.UTC().Truncate(24 * time.Hour)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// testQuotaWeeklyMonday: quota_weekly counts the UTC calendar week, from
// Monday 00:00, as OpenCode's usage route reports it (spec 006, task 296):
// the spend of a Sunday denies until Monday 00:00 (retry-after up to it)
// and no longer counts a minute later, where 7 sliding days still would.
func testQuotaWeeklyMonday(t T, s limits.Store) {
	next := mondayUTC(baseTime()).AddDate(0, 0, 7)
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 30, WeeklyPct: 50}})
	for _, h := range []int{14, 8, 2} { // Sunday, 6h apart: never 3 USD in 5h.
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: next.Add(-time.Duration(h) * time.Hour), CostMicroUSD: 2_000_000, Status: 200})
	}
	d := checkAt(t, s, next.Add(-time.Hour), 0, as)
	mustDeny(t, d, accountEnt, "quota_weekly", "Sunday spend over 50% of the week")
	if d.Kind != core.ErrQuota || d.RetryAfter != time.Hour {
		t.Fatalf("quota_weekly denial: %+v, want retry-after 1h (Monday 00:00 UTC)", d)
	}
	mustAllow(t, checkAt(t, s, next.Add(time.Minute), 0, as), "week turned on Monday 00:00 UTC")
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: next.Add(2 * time.Minute), CostMicroUSD: 2_000_000, Status: 200})
	mustAllow(t, checkAt(t, s, next.Add(6*time.Hour), 0, as), "only Monday's 2 USD count in the new week")
}

// testQuota5hState: Get reports the quota spend of the last 5 h and its
// oldest spend (the base of OpenCode's rolling.percent and resetsAt; task
// 296); spend older than 5 h does not enter it, and an entity without
// spend reads zero. Get reads the wall clock.
func testQuota5hState(t T, s limits.Store) {
	if st := get(t, s, accountEnt); st.Quota5hUSD != 0 || !st.Quota5hSince.IsZero() {
		t.Fatalf("fresh entity: 5h %v since %v, want zero", st.Quota5hUSD, st.Quota5hSince)
	}
	now := time.Now()
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 100}})
	for _, f := range []struct {
		ago  time.Duration
		cost int64
	}{{6 * time.Hour, 5_000_000}, {2 * time.Hour, 1_000_000}, {time.Hour, 500_000}} {
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now.Add(-f.ago), CostMicroUSD: f.cost, Status: 200})
	}
	st := get(t, s, accountEnt)
	if !approx(st.Quota5hUSD, 1.5, 1e-9) {
		t.Fatalf("Quota5hUSD = %v, want 1.5 (the 5 USD of 6h ago left the window)", st.Quota5hUSD)
	}
	if want := now.Add(-2 * time.Hour); st.Quota5hSince.After(want) || want.Sub(st.Quota5hSince) >= time.Minute {
		t.Fatalf("Quota5hSince = %v, want the minute of %v", st.Quota5hSince, want)
	}
}

func testQuotaMonthly(t T, s limits.Store) {
	now := baseTime()
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1}})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 1_000_000, Status: 200})
	d := checkAt(t, s, now, 0, as)
	mustDeny(t, d, accountEnt, "quota_monthly", "monthly quota used")
	if d.Kind != core.ErrQuota || d.RetryAfter <= 0 {
		t.Fatalf("quota_monthly denial: %+v", d)
	}
}
