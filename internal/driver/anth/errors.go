package anth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// AnthropicErrorBody maps an error to Anthropic's envelope. The body is a
// map so gateways can add fields; WriteError orders it like the real
// (type, error{type, message, …}, request_id).
func AnthropicErrorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	status, typ := 500, "api_error"
	msg := e.Message
	hdr := map[string]string{}
	var details map[string]any
	limit := ""
	if e.Extra != nil {
		if s, ok := e.Extra["limit"].(string); ok {
			limit = s
		}
	}
	switch e.Kind {
	case core.ErrInvalidRequest, core.ErrContentFilter, core.ErrMissingSession:
		status, typ = 400, "invalid_request_error"
	case core.ErrContextLength:
		status, typ = 400, "invalid_request_error"
		if msg == "" {
			msg = "prompt is too long: 213456 tokens > 200000 maximum"
		}
	case core.ErrSpendLimit:
		status, typ = 400, "invalid_request_error"
		if msg == "" {
			msg = "You have reached your specified API usage limits. You will regain access on the first of next month at 00:00 UTC."
		}
	case core.ErrInvalidAPIKey:
		status, typ = 401, "authentication_error"
		switch msg {
		case "":
			msg = "invalid x-api-key"
		case oai.NoKeyMessage:
			// Neither x-api-key nor Authorization (auth-order/nokey_*).
			msg = "x-api-key header is required"
		}
	case core.ErrAccountBanned:
		status, typ = 401, "authentication_error"
		if msg == "" {
			msg = "This API key has been disabled. Please contact support."
		}
	case core.ErrPermission, core.ErrGeoBlocked, core.ErrAccountSuspended:
		status, typ = 403, "permission_error"
	case core.ErrForgedRequest:
		// The strict client verification (spec 020, ADR 0074): wwz's own
		// type, so the router under test tells it from the real 403.
		status, typ = 403, "forged_request_error"
		reason, _ := e.Extra[core.ExtraVerificationReason].(string)
		hdr["x-wwz-client-verification"] = "failed; reason=" + reason
	case core.ErrModelNotFound, core.ErrNotFound:
		status, typ = 404, "not_found_error"
		if msg == "" && (e.Kind == core.ErrModelNotFound || req.Model != "") {
			msg = fmt.Sprintf("model: %s", req.Model)
		}
	case core.ErrMethodNotAllowed:
		status, typ = 405, "invalid_request_error"
	case core.ErrRequestTooLarge:
		status, typ = 413, "request_too_large"
	case core.ErrRateLimit, core.ErrSlowDown:
		status, typ = 429, "rate_limit_error"
		if msg == "" {
			msg = anthropicRateLimitMessage(req, e, limit)
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(ceilSeconds(e.RetryAfter), 10)
		}
	case core.ErrQuota:
		if limit == "credits" {
			status, typ = 402, "billing_error"
		} else {
			// Tier spend cap: 429 without retry-after, details.error_code.
			status, typ = 429, "rate_limit_error"
			details = map[string]any{"error_code": "enforced_spend_limit_reached"}
			if msg == "" {
				msg = "You have reached your API usage limits. You will regain access on the first of next month at 00:00 UTC."
			}
		}
	case core.ErrBudget:
		status, typ = 429, "rate_limit_error"
	case core.ErrServerError:
		status, typ = 500, "api_error"
		if msg == "" {
			msg = "Internal server error"
		}
	case core.ErrBadGateway:
		return 502, []byte("<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center><hr><center>cloudflare</center></body></html>"), "text/html", hdr
	case core.ErrOverloaded:
		status, typ = 529, "overloaded_error"
		if msg == "" {
			msg = "Overloaded"
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(ceilSeconds(e.RetryAfter), 10)
		}
	case core.ErrGatewayTimeout:
		status, typ = 504, "timeout_error"
		if msg == "" {
			msg = "Request timed out"
		}
	}
	if msg == "" {
		msg = anthropicDefaultMessage(e, limit)
	}
	if e.Status > 0 {
		status = e.Status
	}
	if e.Code != "" {
		typ = e.Code
	}
	errObj := map[string]any{"type": typ, "message": msg}
	if details != nil {
		errObj["details"] = details
	}
	body := map[string]any{"type": "error", "error": errObj, "request_id": reqID(req)}
	return status, body, "application/json", hdr
}

// anthropicDefaultMessage is Anthropic's text for an error raised without
// one (engine limits, chaos): real responses always carry a message.
func anthropicDefaultMessage(e *core.Error, limit string) string {
	switch e.Kind {
	case core.ErrInvalidRequest, core.ErrMissingSession:
		return "Invalid request"
	case core.ErrContentFilter:
		return "Output blocked by content filtering policy"
	case core.ErrPermission:
		return "Your API key does not have permission to use the specified resource."
	case core.ErrGeoBlocked:
		return "Request not allowed"
	case core.ErrAccountSuspended:
		return "This organization has been disabled."
	case core.ErrNotFound:
		return "Not Found"
	case core.ErrMethodNotAllowed:
		return "Method Not Allowed"
	case core.ErrForgedRequest:
		return "Client verification failed: the client is forging requests."
	case core.ErrRequestTooLarge:
		return "Request exceeds the maximum allowed number of bytes."
	case core.ErrQuota:
		if limit == "credits" {
			return "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."
		}
	case core.ErrBudget:
		return "You have reached your specified workspace API usage limits. You will regain access on the first of next month at 00:00 UTC."
	}
	return "Internal server error"
}

// anthropicRateLimitMessage is Anthropic's 429 text, naming the account's
// organization (the same id as anthropic-organization-id). With the
// figures of a limits denial it names the exceeded limit ("of 50 requests
// per minute"); without them (chaos, rps, concurrency) it is the generic
// sentence.
func anthropicRateLimitMessage(req *core.Request, e *core.Error, limit string) string {
	const rest = ". For details, refer to: https://docs.claude.com/en/api/rate-limits. You can see the response headers for current usage. Please reduce the prompt length or the maximum tokens requested, or try again later. You may also contact sales at https://www.anthropic.com/contact-sales to discuss your options for a rate limit increase."
	base := "This request would exceed the rate limit for your organization (" + OrganizationID(req) + ")"
	n, _, _, ok := oai.LimitNumbers(e)
	if !ok || e.Kind != core.ErrRateLimit {
		return base + rest
	}
	unit := "requests per minute"
	switch limit {
	case "tpm", "itpm":
		unit = "input tokens per minute"
	case "otpm":
		unit = "output tokens per minute"
	}
	return base + " of " + thousands(n) + " " + unit + rest
}

// thousands formats a positive n with comma separators (40000 -> "40,000").
func thousands(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func ceilSeconds(d time.Duration) int64 {
	s := d.Seconds()
	n := int64(s)
	if float64(n) < s {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

// jsonFormat is the layout of every Anthropic body: compact, with <, >
// and & as they are (observed "model: a<b>&c").
var jsonFormat = oai.Format{RawHTML: true}

// envelope orders an error map like the real: type, error, request_id,
// then any other key sorted; inside error, type and message first.
func envelope(m map[string]any) oai.Obj {
	o := ordered(m, "type", "error", "request_id")
	if inner, ok := m["error"].(map[string]any); ok {
		o.Set("error", ordered(inner, "type", "message"))
	}
	return o
}

// ordered is m with the listed keys first, in that order, then the rest in
// sorted order; nested maps keep their keys sorted.
func ordered(m map[string]any, first ...string) oai.Obj {
	o := make(oai.Obj, 0, len(m))
	seen := map[string]bool{}
	for _, k := range first {
		if v, ok := m[k]; ok {
			o = append(o, oai.Field{Key: k, Val: plain(v)})
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		o = append(o, oai.Field{Key: k, Val: plain(m[k])})
	}
	return o
}

func plain(v any) any {
	if m, ok := v.(map[string]any); ok {
		return ordered(m)
	}
	return v
}

// errorBytes renders an ErrorBody body: maps as the ordered envelope,
// bytes verbatim, anything else as JSON.
func errorBytes(body any) []byte {
	switch b := body.(type) {
	case []byte:
		return b
	case json.RawMessage:
		return b
	case map[string]any:
		out, _ := jsonFormat.Marshal(envelope(b))
		return out
	}
	out, err := jsonFormat.Marshal(body)
	if err != nil {
		out = []byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`)
	}
	return out
}

// WriteError renders an error through the hooks. With API set it carries
// what the real sends: request-id, x-should-retry, the account headers on
// every error past the key check (none on 401, which has Cache-Control:
// no-store instead).
func WriteError(h *Hooks, w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if req == nil {
		req = &core.Request{}
	}
	if req.RequestID == "" {
		req.RequestID = NewRequestID() // the header and the body share it
	}
	status, body, ct, hdr := h.ErrorBody(req, e)
	// A chaos/next raw body replaces the envelope; status and headers stay.
	b, ct := e.RenderBody(errorBytes(body), ct)
	w.Status(status)
	w.Header("Content-Type", ct)
	w.Header("Content-Length", strconv.Itoa(len(b)))
	if name := h.requestIDHeader(); name != "" {
		w.Header(name, reqID(req))
	}
	if h.API {
		w.Header("x-should-retry", shouldRetry(status))
		if status == 401 {
			w.Header("Cache-Control", "no-store")
		} else if h.AccountHeaders != nil && e.Kind != core.ErrForgedRequest {
			// A forged client is refused before its credential is
			// looked at: no account to report (spec 020).
			for k, v := range h.AccountHeaders(req) {
				w.Header(k, v)
			}
		}
	}
	if rl != nil && rl.Present && h.RateLimitHeaders != nil {
		for k, v := range h.RateLimitHeaders(rl) {
			w.Header(k, v)
		}
	}
	for k, v := range hdr {
		w.Header(k, v)
	}
	for k, v := range e.Extra {
		if strings.HasPrefix(k, "header:") {
			if s, ok := v.(string); ok {
				w.Header(strings.TrimPrefix(k, "header:"), s)
			}
		}
	}
	_, _ = w.Write(b)
	_ = w.Flush()
}
