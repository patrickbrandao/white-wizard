package opencode

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// usageTime is the resetsAt layout: RFC 3339 with milliseconds (observed
// "2026-11-02T00:00:00.000Z").
const usageTime = "2006-01-02T15:04:05.000Z"

// The 401 of GET <base>/v1/usage, without a key or with an invalid one
// (conferred on 2026-10-31; usage_zen_badkey.json): the gateway's own
// envelope, not the upstream one of the inference routes.
const (
	usageAuthType    = "AuthError"
	usageAuthMessage = "Unauthorized"
)

// isUsageRoute reports whether req is GET <base>/v1/usage, by the route the
// engine stored (driver.SetRoute) before it handed the error over.
func isUsageRoute(req *core.Request) bool {
	_, path := driver.RouteOfRequest(req)
	return strings.HasSuffix(path, "/v1/usage")
}

// quota is the use of each quota window, in percent, whether the window
// denies, and the time of the oldest spend of the rolling window (zero
// without spend).
type quota struct {
	rolling, weekly, monthly int
	limited                  [3]bool // rolling, weekly, monthly
	rollingSince             time.Time
}

// quotaOf is the account's use of its quota_usd windows (tasks 254 and
// 296), from the store's view of the account: the rolling window is the
// quota spend of the last 5 h, the weekly one the spend since Monday 00:00
// UTC and the monthly one the spend of the UTC month. It is the view of a
// context without the billing view (QuotaOwner); every status reads ok.
func quotaOf(ctx driver.ExtraContext) quota {
	q, b := ctx.Limits.QuotaUSD, ctx.AccountBalance
	if q == nil || q.MonthlyUSD <= 0 || b == nil {
		return quota{}
	}
	return quota{
		rolling:      percentOf(b.Quota5hUSD, q.MonthlyUSD*q.Rolling5hPct/100),
		weekly:       percentOf(b.SpendWeeklyUSD, q.MonthlyUSD*q.WeeklyPct/100),
		monthly:      percentOf(b.SpendUSD, q.MonthlyUSD),
		rollingSince: b.Quota5hSince,
	}
}

// ownerQuota is the use of the quota owner's windows (ADR 0072, items 11
// and 13): the owner (the account, or the member in Go Teams) and each of
// its per-model quotas; in each window the largest percentage, and the
// window denies (status rate-limited) when any of them is used up, as
// quota_5h, quota_weekly and quota_monthly deny. The rolling resetsAt
// follows the quota with the largest rolling use (the owner on a tie).
func ownerQuota(ctx driver.ExtraContext) quota {
	views := []struct {
		q config.QuotaUSD
		b *driver.Balance
	}{{ctx.QuotaOwner.Quota, ctx.QuotaOwner.State}}
	for _, m := range ctx.ModelQuotas {
		views = append(views, struct {
			q config.QuotaUSD
			b *driver.Balance
		}{m.Quota, m.State})
	}
	var out quota
	best := -1
	for _, v := range views {
		if v.q.MonthlyUSD <= 0 || v.b == nil {
			continue
		}
		week, month := quotaWeekMonth(v.b)
		wins := [3]struct{ used, limit float64 }{
			{v.b.Quota5hUSD, v.q.MonthlyUSD * v.q.Rolling5hPct / 100},
			{week, v.q.MonthlyUSD * v.q.WeeklyPct / 100},
			{month, v.q.MonthlyUSD},
		}
		pct := [3]*int{&out.rolling, &out.weekly, &out.monthly}
		for i, w := range wins {
			p := percentOf(w.used, w.limit)
			*pct[i] = max(*pct[i], p)
			if w.limit > 0 && w.used >= w.limit {
				out.limited[i] = true
			}
			if i == 0 && p > best {
				best, out.rollingSince = p, v.b.Quota5hSince
			}
		}
	}
	return out
}

// quotaWeekMonth is the quota debited in the week (from Monday 00:00 UTC)
// and in the UTC month: the windows quota_weekly and quota_monthly deny on.
func quotaWeekMonth(b *driver.Balance) (week, month float64) {
	return b.QuotaWeeklyUSD, b.QuotaMonthlyUSD
}

// writeUsageRoute answers GET <base>/v1/usage: 401 AuthError for public
// (conferred, free/public_usage.json), 403 ForbiddenError for a workspace
// with plan none (ADR 0072, item 9; not conferred), else the windows of
// the quota owner (ownerQuota) or, without the billing view, the
// account's (quotaOf).
func (d *Driver) writeUsageRoute(w core.ResponseWriter, h driver.Headers, ctx driver.ExtraContext, now time.Time) {
	switch {
	case d.Authenticate(h).Anonymous:
		writeUsageError(w, 401, usageAuthType, usageAuthMessage)
		return
	case ctx.Key != nil && ctx.Plan == planNone:
		writeUsageError(w, 403, typeForbidden, noGoMessage)
		return
	}
	q := quotaOf(ctx)
	if ctx.QuotaOwner != nil {
		q = ownerQuota(ctx)
	}
	writeUsage(w, now, q)
}

// writeUsageError is an error of the usage route: the gateway's envelope
// with the listing's headers.
func writeUsageError(w core.ResponseWriter, status int, typ, msg string) {
	b, _ := jsonFormat.Marshal(oai.O("type", "error", "error", oai.O("type", typ, "message", msg)))
	w.Status(status)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	modelsHeaders(w)
	_, _ = w.Write(b)
	_ = w.Flush()
}

// rollingReset is when the rolling window frees: 5 h after its oldest
// spend (the real suggests the first use + 5 h; not conferred), or 5 h
// from now when the window has no spend; in whole seconds, as the real
// prints it (".000Z" in usage_zen).
func rollingReset(now, since time.Time) time.Time {
	r := now.Add(5 * time.Hour)
	if s := since.Add(5 * time.Hour); !since.IsZero() && s.After(now) {
		r = s
	}
	return r.UTC().Truncate(time.Second)
}

// percentOf is used over limit as a whole percent between 0 and 100; 0
// when the window has no limit. Rounded to the nearest: the real prints
// whole numbers, its rounding is not conferred.
func percentOf(used, limit float64) int {
	if limit <= 0 || used <= 0 {
		return 0
	}
	return int(min(math.Round(used/limit*100), 100))
}

// writeUsage answers GET <base>/v1/usage (every base, with or without
// x-opencode-session, conferred on 2026-10-31): {usage: {rolling, weekly,
// monthly}}, each {status, percent, resetsAt}, with the /v1/models headers.
// The rolling window resets 5 h after its oldest spend (5 h ahead without
// spend), the weekly one on Monday 00:00 UTC
// (observed) and the monthly one at the start of the next UTC month, the
// window the wwz's quota_monthly counts (the real uses the subscription's
// anniversary).
func writeUsage(w core.ResponseWriter, now time.Time, q quota) {
	now = now.UTC()
	days := (8 - int(now.Weekday())) % 7
	if days == 0 {
		days = 7
	}
	monday := time.Date(now.Year(), now.Month(), now.Day()+days, 0, 0, 0, 0, time.UTC)
	month := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	win := func(pct int, limited bool, reset time.Time) oai.Obj {
		status := "ok"
		if limited {
			status = "rate-limited"
		}
		return oai.O("status", status, "percent", pct, "resetsAt", reset.Format(usageTime))
	}
	b, _ := jsonFormat.Marshal(oai.O("usage", oai.O(
		"rolling", win(q.rolling, q.limited[0], rollingReset(now, q.rollingSince)),
		"weekly", win(q.weekly, q.limited[1], monday),
		"monthly", win(q.monthly, q.limited[2], month),
	)))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	modelsHeaders(w)
	_, _ = w.Write(b)
	_ = w.Flush()
}
