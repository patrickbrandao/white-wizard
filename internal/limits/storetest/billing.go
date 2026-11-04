package storetest

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// billingCases cover the quota debit apart from the cost, the auxiliary
// entities model_quota and model_spend and the billing fields of the event
// (ADR 0072; cache/valkey/README.md §1 and §3.5; asyncapi 0.5.0).
func billingCases() []testCase {
	return []testCase{
		{"QuotaDebitedWithZeroCost", testQuotaDebitZeroCost},
		{"QuotaOnlyKeepsBalanceAndSpend", testQuotaOnlyKeepsBalance},
		{"QuotaAndCostApart", testQuotaAndCostApart},
		{"QuotaFollowsCostWithoutQuotaSet", testQuotaFollowsCost},
		{"ModelQuotaDeniesBy5h", testModelQuotaDenies},
		{"ModelSpendDeniesBySpendMonthly", testModelSpendDenies},
		{"EventBillingFields", testEventBillingFields},
		{"QuotaWeeklyAndMonthlyWindows", testQuotaCalendarWindows},
	}
}

var (
	modelQuotaEnt = limits.Entity{Kind: limits.KindModelQuota, ID: "acct-1/glm-5.3"}
	modelSpendEnt = limits.Entity{Kind: limits.KindModelSpend, ID: "account:acct-1/glm-5.3"}
)

// Get reports the quota debit of the UTC week (from Monday 00:00, the
// window of quota_weekly) and of the UTC month (quota_monthly), apart from
// the spend: a quota-only debit (cost 0) moves QuotaWeeklyUSD and
// QuotaMonthlyUSD and never SpendWeeklyUSD or SpendUSD (task 432). Fixed
// instants far in the future, as in testSpendWindows (task 413), so every
// calendar case always runs; each case has its own entity because the
// windows never go back.
func testQuotaCalendarWindows(t T, s limits.Store) {
	for i, c := range []struct {
		what        string
		now         time.Time
		week, month float64
	}{
		{"Saturday mid-month (yesterday in the same week and month)", time.Date(2090, time.October, 14, 12, 0, 0, 0, time.UTC), 2.75, 2.75},
		{"Monday mid-month (the week turned)", time.Date(2090, time.October, 16, 12, 0, 0, 0, time.UTC), 0.75, 2.75},
		{"Wednesday the 1st (the month turned)", time.Date(2090, time.November, 1, 12, 0, 0, 0, time.UTC), 2.75, 0.75},
		{"Monday the 1st (both turned)", time.Date(2090, time.May, 1, 12, 0, 0, 0, time.UTC), 0.75, 0.75},
	} {
		e := limits.Entity{Kind: limits.KindAccount, ID: "acct-qcal-" + string(rune('a'+i))}
		as := sub(e, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1000}})
		yesterday := dayStartOf(c.now).Add(-time.Hour)
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: yesterday, QuotaSet: true, QuotaMicroUSD: 2_000_000, Status: 200})
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: c.now, QuotaSet: true, QuotaMicroUSD: 500_000, Status: 200})
		finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: c.now, QuotaSet: true, QuotaMicroUSD: 250_000, Status: 200})
		st := get(t, s, e)
		if !approx(st.QuotaWeeklyUSD, c.week, 1e-9) || !approx(st.QuotaMonthlyUSD, c.month, 1e-9) {
			t.Fatalf("%s: quota week %v month %v, want %v %v", c.what, st.QuotaWeeklyUSD, st.QuotaMonthlyUSD, c.week, c.month)
		}
		if st.SpendWeeklyUSD != 0 || st.SpendUSD != 0 || st.SpendDailyUSD != 0 || st.SpentTotalUSD != 0 {
			t.Fatalf("%s: spend day %v week %v month %v total %v, want 0 (quota-only debit)", c.what, st.SpendDailyUSD, st.SpendWeeklyUSD, st.SpendUSD, st.SpentTotalUSD)
		}
	}
	// A cost-only debit (QuotaSet with 0) moves the spend and no quota.
	e := limits.Entity{Kind: limits.KindAccount, ID: "acct-qcal-cost"}
	as := sub(e, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1000}})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: time.Now(), CostMicroUSD: 1_000_000, QuotaSet: true, Status: 200})
	if st := get(t, s, e); st.QuotaWeeklyUSD != 0 || st.QuotaMonthlyUSD != 0 || !approx(st.SpendWeeklyUSD, 1, 1e-9) || !approx(st.SpendUSD, 1, 1e-9) {
		t.Fatalf("cost only: quota week %v month %v, spend week %v month %v, want 0 0 1 1", st.QuotaWeeklyUSD, st.QuotaMonthlyUSD, st.SpendWeeklyUSD, st.SpendUSD)
	}
	// Without a quota_usd subject nothing debits the quota windows.
	if st := get(t, s, accountEnt); st.QuotaWeeklyUSD != 0 || st.QuotaMonthlyUSD != 0 {
		t.Fatalf("untouched entity: %+v", st)
	}
}

// dayStartOf is 00:00 UTC of the day of now.
func dayStartOf(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// The quota windows are debited by QuotaMicroUSD even when the cost is 0
// (OpenCode Go: cost 0, the allowance counts in the quota).
func testQuotaDebitZeroCost(t T, s limits.Store) {
	now := time.Now()
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1}})
	mustAllow(t, checkAt(t, s, now, 0, as), "fresh quota")
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 0, QuotaSet: true, QuotaMicroUSD: 1_000_000, Status: 200})
	if st := get(t, s, accountEnt); !approx(st.Quota5hUSD, 1, 1e-9) {
		t.Fatalf("Quota5hUSD = %v, want 1 (the quota debit, cost 0)", st.Quota5hUSD)
	}
	d := checkAt(t, s, now, 0, as)
	mustDeny(t, d, accountEnt, "quota_monthly", "quota used up by the allowance alone")
	if d.Kind != core.ErrQuota {
		t.Fatalf("quota_monthly kind = %s, want %s", d.Kind, core.ErrQuota)
	}
}

// A quota-only debit leaves the balance, the monthly spend and the total
// spent untouched.
func testQuotaOnlyKeepsBalance(t T, s limits.Store) {
	now := time.Now()
	c := 5.0
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}, CreditsUSD: &c, SpendMonthlyUSD: 100})
	mustAllow(t, checkAt(t, s, now, 0, as), "fresh account")
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, QuotaSet: true, QuotaMicroUSD: 2_000_000, Status: 200})
	st := get(t, s, accountEnt)
	if st.Credits == nil || !approx(*st.Credits, 5, 1e-9) {
		t.Fatalf("Credits = %v, want 5 (quota debit never touches the balance)", st.Credits)
	}
	if st.SpendUSD != 0 || st.SpentTotalUSD != 0 {
		t.Fatalf("SpendUSD = %v, SpentTotalUSD = %v, want 0", st.SpendUSD, st.SpentTotalUSD)
	}
	if !approx(st.Quota5hUSD, 2, 1e-9) {
		t.Fatalf("Quota5hUSD = %v, want 2", st.Quota5hUSD)
	}
}

// With both set, the cost debits spend/credits/spent and the quota debit the
// quota windows, each by its own amount; QuotaSet with 0 debits no quota.
func testQuotaAndCostApart(t T, s limits.Store) {
	now := time.Now()
	c := 5.0
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}, CreditsUSD: &c})
	mustAllow(t, checkAt(t, s, now, 0, as), "fresh account")
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 1_000_000, QuotaSet: true, QuotaMicroUSD: 300_000, Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 500_000, QuotaSet: true, QuotaMicroUSD: 0, Status: 200})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 0, QuotaSet: true, QuotaMicroUSD: -7, Status: 200})
	st := get(t, s, accountEnt)
	if st.Credits == nil || !approx(*st.Credits, 3.5, 1e-9) {
		t.Fatalf("Credits = %v, want 3.5 (5 − 1 − 0.5)", st.Credits)
	}
	if !approx(st.SpendUSD, 1.5, 1e-9) || !approx(st.SpentTotalUSD, 1.5, 1e-9) {
		t.Fatalf("SpendUSD = %v, SpentTotalUSD = %v, want 1.5", st.SpendUSD, st.SpentTotalUSD)
	}
	if !approx(st.Quota5hUSD, 0.3, 1e-9) {
		t.Fatalf("Quota5hUSD = %v, want 0.3 (only the quota debits)", st.Quota5hUSD)
	}
}

// Without QuotaSet the quota is debited by the cost, as before ADR 0072.
func testQuotaFollowsCost(t T, s limits.Store) {
	now := time.Now()
	as := sub(accountEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1}})
	finish(t, s, limits.FinishRequest{Subjects: subs(as), Now: now, CostMicroUSD: 1_000_000, QuotaMicroUSD: 5, Status: 200})
	st := get(t, s, accountEnt)
	if !approx(st.Quota5hUSD, 1, 1e-9) || !approx(st.SpendUSD, 1, 1e-9) {
		t.Fatalf("Quota5hUSD = %v, SpendUSD = %v, want 1 and 1 (QuotaMicroUSD ignored without QuotaSet)", st.Quota5hUSD, st.SpendUSD)
	}
	mustDeny(t, checkAt(t, s, now, 0, as), accountEnt, "quota_monthly", "quota used by the cost")
}

// model_quota is an ordinary entity: its quota_5h denies and the denial
// consumes nothing of the subjects admitted before it.
func testModelQuotaDenies(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPD: 1})
	mq := sub(modelQuotaEnt, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 20}})
	finish(t, s, limits.FinishRequest{Subjects: subs(mq), Now: now, QuotaSet: true, QuotaMicroUSD: 2_000_000, Status: 200})
	d := checkAt(t, s, now.Add(time.Minute), 0, ks, mq)
	mustDeny(t, d, modelQuotaEnt, "quota_5h", "20% of the model's 10 USD in 5h")
	if d.Kind != core.ErrQuota || d.RetryAfter <= 0 {
		t.Fatalf("quota_5h denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now.Add(time.Minute), 0, ks), "the denial did not consume the key's rpd")
	mustAllow(t, checkAt(t, s, now.Add(5*time.Hour+2*time.Minute), 0, mq), "window moved past the debit")
	if st := get(t, s, modelQuotaEnt); st.Entity != modelQuotaEnt {
		t.Fatalf("Get(model_quota).Entity = %v, want %v", st.Entity, modelQuotaEnt)
	}
}

// model_spend is an ordinary entity: its spend_monthly denies, with the
// spend_limit kind, and the denial consumes nothing of the other subjects.
func testModelSpendDenies(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPD: 1})
	ms := sub(modelSpendEnt, config.Limits{SpendMonthlyUSD: 1})
	finish(t, s, limits.FinishRequest{Subjects: subs(ms), Now: now, CostMicroUSD: 1_500_000, Status: 200})
	d := checkAt(t, s, now, 0, ks, ms)
	mustDeny(t, d, modelSpendEnt, "spend_monthly", "model budget spent")
	if d.Kind != core.ErrSpendLimit || d.RetryAfter <= 0 {
		t.Fatalf("spend_monthly denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now, 0, ks), "the denial did not consume the key's rpd")
	if st := get(t, s, modelSpendEnt); !approx(st.SpendUSD, 1.5, 1e-9) || st.Entity != modelSpendEnt {
		t.Fatalf("Get(model_spend) = %+v, want SpendUSD 1.5", st)
	}
}

// testEventBillingFields: a served request with BillingSource carries
// fields.billing_source and, when the quota was debited with origin go or
// go-plus, fields.allowance_usd (asyncapi 0.5.0); without BillingSource and
// on a denial neither appears.
func testEventBillingFields(t T, s limits.Store) {
	now := baseTime()
	q := config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 100}}
	ks, as, aq := sub(keyEnt, config.Limits{}), sub(accountEnt, config.Limits{}), sub(accountEnt, q)
	base := func(id string, at int, ss ...limits.Subject) limits.FinishRequest {
		return limits.FinishRequest{Subjects: ss, RequestID: id, Now: now.Add(time.Duration(at) * time.Second), Status: 200, Driver: "opencode"}
	}
	goReq := base("r-go", 0, ks, aq)
	goReq.BillingSource, goReq.QuotaSet, goReq.QuotaMicroUSD = "go", true, 600_000
	plus := base("r-plus", 1, ks, aq)
	plus.BillingSource, plus.QuotaSet, plus.QuotaMicroUSD = "go-plus", true, 250_000
	bal := base("r-balance", 2, ks, aq)
	bal.BillingSource, bal.CostMicroUSD = "balance", 1_000_000
	free := base("r-free", 3, ks, as)
	free.BillingSource = "free"
	noQuota := base("r-go-noquota", 4, ks, as)
	noQuota.BillingSource, noQuota.QuotaSet, noQuota.QuotaMicroUSD = "go", true, 600_000
	plain := base("r-plain", 5, ks, as)
	denied := base("r-denied", 6, ks, aq)
	denied.BillingSource, denied.QuotaSet, denied.QuotaMicroUSD, denied.Status = "go", true, 600_000, 429
	denied.ErrorKind, denied.Denial = core.ErrQuota, &limits.Denial{Entity: accountEnt, Limit: "quota_5h"}
	for _, r := range []limits.FinishRequest{goReq, plus, bal, free, noQuota, plain, denied} {
		finish(t, s, r)
	}
	evs := events(t, s, 20)
	want := map[string]map[string]any{
		"r-go":         {"billing_source": "go", "allowance_usd": 0.6},
		"r-plus":       {"billing_source": "go-plus", "allowance_usd": 0.25},
		"r-balance":    {"billing_source": "balance"},
		"r-free":       {"billing_source": "free"},
		"r-go-noquota": {"billing_source": "go"},
		"r-plain":      {},
		"r-denied":     {},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %s, want %d", eventTypes(evs), len(want))
	}
	for _, e := range evs {
		w, ok := want[e.RequestID]
		if !ok {
			t.Fatalf("unexpected event %q", e.RequestID)
		}
		f, _ := jsonValue(t, e.Fields).(map[string]any)
		for _, k := range []string{"billing_source", "allowance_usd"} {
			got, has := f[k]
			exp, wantHas := w[k]
			if has != wantHas || (has && got != exp) {
				t.Errorf("event %s: fields.%s = %v (present %v), want %v (present %v)", e.RequestID, k, got, has, exp, wantHas)
			}
		}
		if e.RequestID != "r-denied" && e.RequestID != "r-balance" && f["cost_usd"] != 0.0 {
			t.Errorf("event %s: cost_usd = %v, want 0", e.RequestID, f["cost_usd"])
		}
	}
}
