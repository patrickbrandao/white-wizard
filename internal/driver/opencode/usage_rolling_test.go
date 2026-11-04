package opencode_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestUsageRolling (task 296): rolling.percent is the account's quota spend
// of the last 5 h over rolling_5h_pct of the monthly quota, and
// rolling.resetsAt is 5 h after the window's oldest spend; without spend
// (or with a stale oldest spend) it is 5 h from now; in whole seconds.
func TestUsageRolling(t *testing.T) {
	lim := config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 60, Rolling5hPct: 20, WeeklyPct: 50}}
	since := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	u := usageOf(t, driver.ExtraContext{Limits: lim, AccountBalance: &driver.Balance{Quota5hUSD: 3, Quota5hSince: since, SpendWeeklyUSD: 3, SpendUSD: 3}})
	r := u.Usage["rolling"]
	if r.Percent != 25 || r.Status != "ok" {
		t.Errorf("rolling %+v, want 25%% (3 of 12 USD)", r)
	}
	if want := since.Add(5 * time.Hour).UTC().Truncate(time.Second).Format("2006-01-02T15:04:05.000Z"); r.ResetsAt != want {
		t.Errorf("rolling resetsAt %s, want %s (oldest spend + 5h)", r.ResetsAt, want)
	}
	for name, b := range map[string]*driver.Balance{
		"no spend":     {},
		"stale oldest": {Quota5hSince: time.Now().Add(-6 * time.Hour)},
	} {
		before := time.Now().Add(5 * time.Hour).Truncate(time.Second)
		r := usageOf(t, driver.ExtraContext{Limits: lim, AccountBalance: b}).Usage["rolling"]
		at, err := time.Parse(time.RFC3339, r.ResetsAt)
		if err != nil || at.Before(before) || at.After(time.Now().Add(5*time.Hour)) || r.Percent != 0 {
			t.Errorf("%s: rolling %+v (%v), want 0%% resetting 5h from now", name, r, err)
		}
	}
	if r := usageOf(t, driver.ExtraContext{Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 60}}, AccountBalance: &driver.Balance{Quota5hUSD: 3}}).Usage["rolling"]; r.Percent != 0 {
		t.Errorf("no rolling window: %+v, want 0%%", r)
	}
}
