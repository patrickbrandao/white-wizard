package driver

import (
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Subscription accounts (ADR 0074; spec 005, "Assinatura e verificação do
// cliente", interface (c)): a provider whose clients authenticate with a
// signed token of a subscription (Anthropic's OAuth for Claude Code) refuses
// that token for more than one reason and reports the subscription's quota
// windows on the answers. Both interfaces are optional and found with As;
// the engine asks them nothing about a driver without them.

// TokenChecker is implemented by drivers whose tokens (KeyResolver) are
// refused with more than the invalid-key error: expired, without the
// provider's beta, without the route's scope, presented in the wrong header
// (Anthropic's OAuth tokens, spec 006 16.2.1). The engine asks it about
// every presented key when server.auth is on, after the key was looked up:
// cred.KeyLabel is set when the KeyResolver accepted the token. A non-nil
// error is the refusal of the credential on a route of kind; nil keeps the
// engine's verdict.
type TokenChecker interface {
	CheckToken(kind Kind, cred Credential, h Headers, keys KeyLookup, now time.Time) *core.Error
}

// SubscriptionReporter is implemented by drivers whose answers to a
// subscription's token carry its quota windows (anthropic-ratelimit-
// unified-*). Subscription reports whether the answer to a request of kind
// presenting cred does; the engine then reads the windows from the store
// and stores them in the request (SetSubscription) before the limits are
// checked, so the success and the 429 both see them.
type SubscriptionReporter interface {
	Subscription(kind Kind, cred Credential) bool
}

// QuotaWindow is one window of a subscription quota: what was debited in
// it, its cap (0 without one) and when it resets.
type QuotaWindow struct {
	SpentUSD float64
	LimitUSD float64
	Reset    time.Time
}

// Utilization is the window's debit over its cap, as a fraction (it may
// pass 1); 0 without a cap.
func (q QuotaWindow) Utilization() float64 {
	if q.LimitUSD <= 0 {
		return 0
	}
	return q.SpentUSD / q.LimitUSD
}

// SubscriptionView is the 5 h and weekly windows of the quota_usd of an
// account (spec 010): the rolling 5 h window and the UTC week from Monday
// 00:00, wwz's subscription windows.
type SubscriptionView struct {
	FiveHour QuotaWindow
	Weekly   QuotaWindow
}

// NewSubscriptionView is the view of quota q (nil: no quota, utilization 0)
// as the store's state b reports it (nil: nothing debited) at now. The 5 h
// window resets 5 h after its oldest debit, or 5 h from now without one;
// the week on the next Monday 00:00 UTC.
func NewSubscriptionView(q *config.QuotaUSD, b *Balance, now time.Time) SubscriptionView {
	v := SubscriptionView{
		FiveHour: QuotaWindow{Reset: now.Add(5 * time.Hour)},
		Weekly:   QuotaWindow{Reset: NextWeek(now)},
	}
	if q != nil && q.MonthlyUSD > 0 {
		v.FiveHour.LimitUSD = q.MonthlyUSD * q.Rolling5hPct / 100
		v.Weekly.LimitUSD = q.MonthlyUSD * q.WeeklyPct / 100
	}
	if b != nil {
		v.FiveHour.SpentUSD, v.Weekly.SpentUSD = b.Quota5hUSD, b.QuotaWeeklyUSD
		if !b.Quota5hSince.IsZero() {
			v.FiveHour.Reset = b.Quota5hSince.Add(5 * time.Hour)
		}
	}
	return v
}

// NextWeek is the next Monday 00:00 UTC after now: when the store's weekly
// quota window turns.
func NextWeek(now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	back := (int(day.Weekday()) + 6) % 7 // days since Monday
	return day.AddDate(0, 0, 7-back)
}

// subscriptionHeader is the request-header slot of the SubscriptionView;
// like the account slots, the NUL prefix keeps clients from sending it.
const subscriptionHeader = "\x00wwz-subscription"

// SetSubscription stores v in h, replacing whatever was there.
func SetSubscription(h MapHeaders, v SubscriptionView) {
	if h == nil {
		return
	}
	f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
	h[subscriptionHeader] = strings.Join([]string{
		f(v.FiveHour.SpentUSD), f(v.FiveHour.LimitUSD), strconv.FormatInt(v.FiveHour.Reset.UnixNano(), 10),
		f(v.Weekly.SpentUSD), f(v.Weekly.LimitUSD), strconv.FormatInt(v.Weekly.Reset.UnixNano(), 10),
	}, "|")
}

// SubscriptionOf reads the SubscriptionView the engine stored in req; false
// when it stored none (the request is not a subscription's).
func SubscriptionOf(req *core.Request) (SubscriptionView, bool) {
	if req == nil {
		return SubscriptionView{}, false
	}
	f := strings.Split(req.Header(subscriptionHeader), "|")
	if len(f) != 6 {
		return SubscriptionView{}, false
	}
	window := func(spent, limit, reset string) QuotaWindow {
		w := QuotaWindow{}
		w.SpentUSD, _ = strconv.ParseFloat(spent, 64)
		w.LimitUSD, _ = strconv.ParseFloat(limit, 64)
		ns, _ := strconv.ParseInt(reset, 10, 64)
		w.Reset = time.Unix(0, ns)
		return w
	}
	return SubscriptionView{FiveHour: window(f[0], f[1], f[2]), Weekly: window(f[3], f[4], f[5])}, true
}
