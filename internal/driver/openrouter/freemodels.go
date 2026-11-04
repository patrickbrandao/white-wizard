package openrouter

import (
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// freeModelsPerMinuteMessage is the text of the per-minute cap on :free
// models, trailing space included (conferred on 2026-10-31, task 262).
const freeModelsPerMinuteMessage = "Rate limit exceeded: free-models-per-min. "

// freeModelsPerMinuteHint is the remedy_hint of that 429.
const freeModelsPerMinuteHint = "Slow down requests to free models, or retry after the per-minute window resets."

// freeModelsPerMinuteError reports whether e is the per-minute cap of the
// engine's free-models unit (Extra "free_models", task 262).
func freeModelsPerMinuteError(e *core.Error, limit string) bool {
	free, _ := e.Extra["free_models"].(bool)
	return free && (limit == "rpm" || limit == "rps")
}

// freeModelsMinuteReset is X-RateLimit-Reset of the per-minute cap: the
// start of the minute after now, in milliseconds (the real value was a
// minute boundary).
func freeModelsMinuteReset(now time.Time) string {
	return strconv.FormatInt(now.Truncate(time.Minute).Add(time.Minute).UnixMilli(), 10)
}

// freeModelsPerMinuteBody is the 429 of the account's per-minute cap on
// :free models, as the real API answered it on 2026-10-31 (task 262): the
// X-RateLimit-* headers repeated in metadata.headers, limit_source
// openrouter_free_tier_per_minute, remedy_hint, provider_name null and the
// user_id; no error_type and no Retry-After. The per-day cap keeps the
// generic form (not conferred). hdr receives the X-RateLimit-* headers.
func freeModelsPerMinuteBody(e *core.Error, limit string, hdr map[string]string) (oai.Obj, bool) {
	if !freeModelsPerMinuteError(e, limit) {
		return nil, false
	}
	lim, _ := e.Extra["header:X-RateLimit-Limit"].(string)
	if lim == "" {
		lim = strconv.Itoa(freeModelsPerMinute)
	}
	rem, _ := e.Extra["header:X-RateLimit-Remaining"].(string)
	if rem == "" {
		rem = "0"
	}
	reset, _ := e.Extra["header:X-RateLimit-Reset"].(string)
	if reset == "" {
		reset = freeModelsMinuteReset(time.Now())
	}
	hdr["X-RateLimit-Limit"], hdr["X-RateLimit-Remaining"], hdr["X-RateLimit-Reset"] = lim, rem, reset
	meta := oai.O(
		"headers", oai.O("X-RateLimit-Limit", lim, "X-RateLimit-Remaining", rem, "X-RateLimit-Reset", reset),
		"limit_source", "openrouter_free_tier_per_minute",
		"remedy_hint", freeModelsPerMinuteHint,
		"provider_name", nil,
	)
	return oai.O("error", oai.O("message", freeModelsPerMinuteMessage, "code", 429, "metadata", meta), "user_id", "user_wwz"), true
}
