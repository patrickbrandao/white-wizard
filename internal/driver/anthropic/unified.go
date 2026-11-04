package anthropic

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
)

// The answers to a subscription (OAuth) token (spec 006 16.2.1): the
// anthropic-ratelimit-unified-* of the account's quota_usd windows in the
// place of the anthropic-ratelimit-{requests,tokens,input-tokens,
// output-tokens}-*, and the quota's 429 as the subscription's. Names and
// values are the client's; presence, order and the exact format are not
// conferred.

// The windows' names in representative-claim.
const (
	claimFiveHour = "five_hour"
	claimSevenDay = "seven_day"
)

// msgQuota is the 429 of an exhausted subscription window (not conferred).
const msgQuota = "This request would exceed your account's rate limit. Please try again later."

// unifiedPrefix starts every subscription header.
const unifiedPrefix = "anthropic-ratelimit-unified-"

// unifiedHeaders are the subscription headers of v. claim names the window
// that refused the request ("" when it was not refused by the quota): the
// status is then rejected and the claim is that window; otherwise
// allowed_warning past 0.9 of the 5 h window or 0.75 of the week, else
// allowed, and the claim is the window of the larger use (five_hour on a
// tie). reset is the claim's.
func unifiedHeaders(v driver.SubscriptionView, claim string) map[string]string {
	u5, u7 := v.FiveHour.Utilization(), v.Weekly.Utilization()
	status := "allowed"
	switch {
	case claim != "":
		status = "rejected"
	case u5 > 0.9 || u7 > 0.75:
		status = "allowed_warning"
	}
	if claim == "" {
		claim = claimFiveHour
		if u7 > u5 {
			claim = claimSevenDay
		}
	}
	reset := v.FiveHour.Reset
	if claim == claimSevenDay {
		reset = v.Weekly.Reset
	}
	return map[string]string{
		unifiedPrefix + "status":                  status,
		unifiedPrefix + "5h-utilization":          utilization(u5),
		unifiedPrefix + "5h-reset":                unixSeconds(v.FiveHour.Reset),
		unifiedPrefix + "7d-utilization":          utilization(u7),
		unifiedPrefix + "7d-reset":                unixSeconds(v.Weekly.Reset),
		unifiedPrefix + "representative-claim":    claim,
		unifiedPrefix + "reset":                   unixSeconds(reset),
		unifiedPrefix + "overage-status":          "rejected",
		unifiedPrefix + "overage-disabled-reason": "org_level_disabled",
	}
}

// utilization is a fraction in short decimal form: two places, no
// trailing zeros ("0", "0.42", "1.05").
func utilization(u float64) string {
	return strconv.FormatFloat(math.Round(u*100)/100, 'f', -1, 64)
}

func unixSeconds(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// quotaClaim is the window of a quota denial e (the engine's ErrQuota of
// limit quota_5h, quota_weekly or quota_monthly); "" for any other error.
// The month has no window of its own in a subscription: it is the week's.
func quotaClaim(e *core.Error) string {
	if e == nil || e.Kind != core.ErrQuota {
		return ""
	}
	limit, _ := e.Extra["limit"].(string)
	switch {
	case limit == "quota_5h":
		return claimFiveHour
	case strings.HasPrefix(limit, "quota_"):
		return claimSevenDay
	}
	return ""
}

// subscriptionHooks are the driver's hooks for req: as they are when the
// engine stored no subscription view in it, else with the success headers
// (body and stream) carrying the account and the unified headers instead
// of the anthropic-ratelimit-*, and, for an error e, the unified headers
// in the place of the limit headers and the quota's denial as the
// subscription's 429 (rate_limit_error, retry-after, status rejected).
func (d *Driver) subscriptionHooks(req *core.Request, e *core.Error) *anth.Hooks {
	v, ok := driver.SubscriptionOf(req)
	if !ok {
		return &d.hooks
	}
	h := d.hooks
	claim := quotaClaim(e)
	uh := unifiedHeaders(v, claim)
	h.SuccessHeaders = func(req *core.Request, _ *core.Response, _ *core.RateLimitState) map[string]string {
		m := anth.AnthropicAccountHeaders(req)
		for k, v := range uh {
			m[k] = v
		}
		return m
	}
	h.RateLimitHeaders = func(*core.RateLimitState) map[string]string { return uh }
	if claim != "" {
		h.RateLimitHeaders = nil // the error body's headers carry them
		h.ErrorBody = func(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
			status, body, ct, hdr := anth.AnthropicErrorBody(req, &core.Error{Kind: core.ErrRateLimit, Message: msgQuota, RetryAfter: e.RetryAfter})
			for k, v := range uh {
				hdr[k] = v
			}
			return status, body, ct, hdr
		}
	}
	return &h
}
