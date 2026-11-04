package anthropic

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestUnifiedHeaders: the status and the claim of the subscription
// headers (spec 006 16.2.1): allowed, allowed_warning past 0.9 of the 5 h
// window or 0.75 of the week, rejected with the window that refused; the
// claim is the window of the larger use (five_hour on a tie) and reset is
// its reset.
func TestUnifiedHeaders(t *testing.T) {
	r5, r7 := time.Unix(1000, 0), time.Unix(2000, 0)
	view := func(s5, s7 float64) driver.SubscriptionView {
		return driver.SubscriptionView{
			FiveHour: driver.QuotaWindow{SpentUSD: s5, LimitUSD: 10, Reset: r5},
			Weekly:   driver.QuotaWindow{SpentUSD: s7, LimitUSD: 100, Reset: r7},
		}
	}
	for _, c := range []struct {
		name                       string
		v                          driver.SubscriptionView
		claim                      string
		status, rep, reset, u5, u7 string
	}{
		{"idle", view(0, 0), "", "allowed", "five_hour", "1000", "0", "0"},
		{"5h warning", view(9.5, 10), "", "allowed_warning", "five_hour", "1000", "0.95", "0.1"},
		{"week warning", view(1, 80), "", "allowed_warning", "seven_day", "2000", "0.1", "0.8"},
		{"week larger", view(1, 50), "", "allowed", "seven_day", "2000", "0.1", "0.5"},
		{"rejected 5h", view(10, 10), claimFiveHour, "rejected", "five_hour", "1000", "1", "0.1"},
		{"rejected week", view(1, 105), claimSevenDay, "rejected", "seven_day", "2000", "0.1", "1.05"},
	} {
		h := unifiedHeaders(c.v, c.claim)
		if h[unifiedPrefix+"status"] != c.status || h[unifiedPrefix+"representative-claim"] != c.rep || h[unifiedPrefix+"reset"] != c.reset ||
			h[unifiedPrefix+"5h-utilization"] != c.u5 || h[unifiedPrefix+"7d-utilization"] != c.u7 ||
			h[unifiedPrefix+"5h-reset"] != "1000" || h[unifiedPrefix+"7d-reset"] != "2000" ||
			h[unifiedPrefix+"overage-status"] != "rejected" || h[unifiedPrefix+"overage-disabled-reason"] != "org_level_disabled" {
			t.Errorf("%s: %v", c.name, h)
		}
		if _, ok := h[unifiedPrefix+"fallback"]; ok {
			t.Errorf("%s: fallback present", c.name)
		}
	}
}

// TestQuotaClaim: only the quota windows' denials are the subscription's
// 429; the month counts as the week.
func TestQuotaClaim(t *testing.T) {
	q := func(limit string) *core.Error {
		return &core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": limit}}
	}
	for _, c := range []struct {
		e    *core.Error
		want string
	}{
		{nil, ""},
		{&core.Error{Kind: core.ErrRateLimit}, ""},
		{q("credits"), ""},
		{q("quota_5h"), claimFiveHour},
		{q("quota_weekly"), claimSevenDay},
		{q("quota_monthly"), claimSevenDay},
	} {
		if got := quotaClaim(c.e); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}

// TestSubscriptionHooksErrors: a subscription's error other than the
// quota keeps its envelope and carries the unified headers where the
// limit headers go (an rpm denial with the limiter's state).
func TestSubscriptionHooksErrors(t *testing.T) {
	d := newOAuthDriver(t, nil)
	h := driver.MapHeaders{}
	driver.SetSubscription(h, driver.SubscriptionView{FiveHour: driver.QuotaWindow{Reset: time.Unix(5, 0)}, Weekly: driver.QuotaWindow{Reset: time.Unix(7, 0)}})
	req := &core.Request{Headers: h, RequestID: "req_011CAAAAAAAAAAAAAAAAAAAA"}
	w := core.NewMemWriter()
	d.WriteError(w, req, &core.Error{Kind: core.ErrRateLimit, RetryAfter: time.Second, Extra: map[string]any{"limit": "rpm"}}, &core.RateLimitState{Present: true, RequestsLimit: 5})
	if w.StatusCode() != 429 || w.Headers()[unifiedPrefix+"status"] != "allowed" || w.Headers()["anthropic-ratelimit-requests-limit"] != "" || w.Headers()["retry-after"] != "1" {
		t.Errorf("rpm denial: %d %v", w.StatusCode(), w.Headers())
	}
	// Without a view the hooks are the driver's own.
	if d.subscriptionHooks(&core.Request{}, nil) != &d.hooks {
		t.Error("hooks copied without a subscription")
	}
}
