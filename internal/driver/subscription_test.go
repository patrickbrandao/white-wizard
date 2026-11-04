package driver_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestSubscriptionView: the 5 h and weekly windows of a quota_usd (spec
// 005, interface (c)): caps from the monthly amount and the percentages,
// the debits of the store's state, the 5 h reset 5 h after the oldest
// debit (or from now), the week's on the next Monday 00:00 UTC.
func TestSubscriptionView(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC) // a Wednesday
	monday := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	q := &config.QuotaUSD{MonthlyUSD: 200, Rolling5hPct: 5, WeeklyPct: 25}
	since := now.Add(-time.Hour)
	v := driver.NewSubscriptionView(q, &driver.Balance{Quota5hUSD: 5, Quota5hSince: since, QuotaWeeklyUSD: 40}, now)
	if v.FiveHour.LimitUSD != 10 || v.FiveHour.Utilization() != 0.5 || !v.FiveHour.Reset.Equal(since.Add(5*time.Hour)) ||
		v.Weekly.LimitUSD != 50 || v.Weekly.Utilization() != 0.8 || !v.Weekly.Reset.Equal(monday) {
		t.Fatalf("%+v", v)
	}
	for name, v := range map[string]driver.SubscriptionView{
		"no quota":        driver.NewSubscriptionView(nil, &driver.Balance{Quota5hUSD: 5}, now),
		"zero quota":      driver.NewSubscriptionView(&config.QuotaUSD{}, nil, now),
		"nothing debited": driver.NewSubscriptionView(q, nil, now),
	} {
		if v.FiveHour.Utilization() != 0 || v.Weekly.Utilization() != 0 || !v.FiveHour.Reset.Equal(now.Add(5*time.Hour)) || !v.Weekly.Reset.Equal(monday) {
			t.Errorf("%s: %+v", name, v)
		}
	}
	for at, want := range map[time.Time]time.Time{
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC):                      time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), // on Monday 00:00
		time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC):                    monday,                                        // Sunday
		time.Date(2026, 11, 8, 23, 0, 0, 0, time.FixedZone("x", -3*3600)): time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), // Monday in UTC
	} {
		if got := driver.NextWeek(at); !got.Equal(want) {
			t.Errorf("NextWeek(%v) = %v, want %v", at, got, want)
		}
	}
}

// TestSetSubscription: the view rides in a request-header slot a client
// cannot send, and comes back as stored; a request without one has none.
func TestSetSubscription(t *testing.T) {
	v := driver.SubscriptionView{
		FiveHour: driver.QuotaWindow{SpentUSD: 1.25, LimitUSD: 10, Reset: time.Unix(1_800_000_000, 5)},
		Weekly:   driver.QuotaWindow{SpentUSD: 0.1, LimitUSD: 0, Reset: time.Unix(1_800_100_000, 0)},
	}
	h := driver.MapHeaders{}
	driver.SetSubscription(h, v)
	driver.SetSubscription(nil, v) // no-op
	got, ok := driver.SubscriptionOf(&core.Request{Headers: h})
	if !ok || got.FiveHour.SpentUSD != 1.25 || got.FiveHour.LimitUSD != 10 || !got.FiveHour.Reset.Equal(v.FiveHour.Reset) ||
		got.Weekly.SpentUSD != 0.1 || !got.Weekly.Reset.Equal(v.Weekly.Reset) {
		t.Fatalf("%+v %v", got, ok)
	}
	for k := range h {
		if k[0] != 0 {
			t.Errorf("slot %q is a header a client can send", k)
		}
	}
	for _, req := range []*core.Request{nil, {}, {Headers: map[string]string{"\x00wwz-subscription": "1|2"}}} {
		if _, ok := driver.SubscriptionOf(req); ok {
			t.Errorf("view in %+v", req)
		}
	}
}
