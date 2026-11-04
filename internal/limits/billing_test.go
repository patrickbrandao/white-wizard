package limits

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// TestFinishQuotaArgs checks ARGV 13 of finish (quota_micro: the debit with
// QuotaSet, -1 = the cost without it; README 3.5, ADR 0072) and the ARGV that
// moved after it (day_start, week_start, free_flag).
func TestFinishQuotaArgs(t *testing.T) {
	v := &Valkey{p: "wwz"}
	now := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	ms0 := periodStarts(now)
	s := Subject{Entity: Entity{Kind: KindModelQuota, ID: "acme/glm-5.3"}, Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}}}
	for _, c := range []struct {
		name string
		req  FinishRequest
		want string
	}{
		{"cost", FinishRequest{CostMicroUSD: 900, QuotaMicroUSD: 5}, "-1"},
		{"allowance", FinishRequest{QuotaSet: true, QuotaMicroUSD: 600_000}, "600000"},
		{"negative", FinishRequest{QuotaSet: true, QuotaMicroUSD: -3}, "0"},
	} {
		c.req.Model = "x:free"
		cl, ok := v.finishCall(s, c.req, now, ms0)
		if !ok || len(cl.args) != 16 {
			t.Fatalf("%s: call %v %v", c.name, ok, cl.args)
		}
		if cl.args[12] != c.want || cl.args[6] != "1" {
			t.Errorf("%s: quota_micro = %s, quota_flag = %s, want %s and 1", c.name, cl.args[12], cl.args[6], c.want)
		}
		if cl.args[13] != ms0[1] || cl.args[14] != ms0[2] || cl.args[15] != "1" {
			t.Errorf("%s: day/week/free = %v", c.name, cl.args[13:])
		}
	}
}

// TestLocalFirstFinishQuota: with store.local_first the finish of a subject
// that only has a quota is kept when the quota is debited and skipped when
// nothing lands in Valkey (README 3.5).
func TestLocalFirstFinishQuota(t *testing.T) {
	v := &Valkey{p: "wwz", lf: &localFirst{}}
	now := time.Now()
	ms0 := periodStarts(now)
	q := Subject{Entity: Entity{Kind: KindAccount, ID: "acme"}, Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}}}
	plain := Subject{Entity: Entity{Kind: KindKey, ID: "k"}}
	for _, c := range []struct {
		name string
		s    Subject
		req  FinishRequest
		want bool
	}{
		{"quota debit, cost 0", q, FinishRequest{QuotaSet: true, QuotaMicroUSD: 1}, true},
		{"quota set to 0", q, FinishRequest{QuotaSet: true}, false},
		{"debit without quota_usd", plain, FinishRequest{QuotaSet: true, QuotaMicroUSD: 1}, false},
		{"cost", plain, FinishRequest{CostMicroUSD: 1}, true},
	} {
		if _, ok := v.finishCall(c.s, c.req, now, ms0); ok != c.want {
			t.Errorf("%s: finish sent = %v, want %v", c.name, ok, c.want)
		}
	}
}

// TestEventAllowance: allowance_usd needs a quota debit, a subject with
// quota_usd and origin go or go-plus; billing_source alone follows
// BillingSource (asyncapi 0.5.0).
func TestEventAllowance(t *testing.T) {
	q := Subject{Entity: Entity{Kind: KindAccount, ID: "acme"}, Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}}}
	plain := Subject{Entity: Entity{Kind: KindAccount, ID: "acme"}}
	for _, c := range []struct {
		name  string
		req   FinishRequest
		src   any
		allow any
	}{
		{"go", FinishRequest{Subjects: []Subject{q}, BillingSource: "go", QuotaSet: true, QuotaMicroUSD: 600_000}, "go", 0.6},
		{"go by cost", FinishRequest{Subjects: []Subject{q}, BillingSource: "go", CostMicroUSD: 500_000}, "go", 0.5},
		{"go zero", FinishRequest{Subjects: []Subject{q}, BillingSource: "go", QuotaSet: true}, "go", nil},
		{"go no quota", FinishRequest{Subjects: []Subject{plain}, BillingSource: "go", QuotaSet: true, QuotaMicroUSD: 1}, "go", nil},
		{"balance", FinishRequest{Subjects: []Subject{q}, BillingSource: "balance", CostMicroUSD: 1}, "balance", nil},
		{"none", FinishRequest{Subjects: []Subject{q}, QuotaSet: true, QuotaMicroUSD: 1}, nil, nil},
	} {
		ev := c.req.event(time.Now())
		if ev.Fields["billing_source"] != c.src || ev.Fields["allowance_usd"] != c.allow {
			t.Errorf("%s: billing_source %v allowance_usd %v, want %v and %v", c.name, ev.Fields["billing_source"], ev.Fields["allowance_usd"], c.src, c.allow)
		}
	}
}
