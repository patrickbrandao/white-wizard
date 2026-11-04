package gemini

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Error envelope: google.rpc.Status, {"error": {"code", "message",
// "status"[, "details"]}} in prettyPrint (spec 006 16.5). The texts marked
// "observed" were conferred on 2026-10-31 (task 164); the others come from
// the documentation or public reports and are "não conferido".

// Extra keys the driver puts on its own errors.
const (
	// extraViolations carries the BadRequest field violations.
	extraViolations = "gemini:violations"
	// extraBackend marks an error raised after the model was resolved: the
	// real answer carries x-gemini-service-tier (observed).
	extraBackend = "gemini:backend"
	// extraNoRoute marks a path Google serves nothing on: 404 with an empty
	// text/html body (observed).
	extraNoRoute = "gemini:no_route"
	// extraMessage overrides the message of the rendered status.
	extraMessage = "gemini:message"
)

// Observed texts.
const (
	msgKeyInvalid   = "API key not valid. Please pass a valid API key."
	msgNoKey        = "Method doesn't allow unregistered callers (callers without established identity). Please use API Key or other form of API consumer identity to call this API."
	msgBearer       = "Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project."
	msgModelGet     = "Model is not found: models/%s for api version %s"
	msgModelMethod  = "models/%s is not found for API version %s, or is not supported for %s. Call ModelService.ListModels to see the list of available models and their supported methods."
	msgCacheMissing = "CachedContent not found (or permission denied)"
)

// Texts from the documentation or public reports (não conferido).
const (
	msgKeyExpired = "API key expired. Please renew the API key."
	msgQuota      = "You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://ai.google.dev/gemini-api/docs/rate-limits."
	msgInternal   = "An internal error has occurred. Please retry or report in https://developers.generativeai.google/guide/troubleshooting"
	msgOverloaded = "The model is overloaded. Please try again later."
	msgDeadline   = "Deadline expired before operation could complete."
	msgGeo        = "User location is not supported for the API use."
	msgTooLarge   = "Request payload size exceeds the limit: 20971520 bytes."
	msgSuspended  = "Permission denied: Consumer 'api_key:%s' has been suspended."
	msgPermission = "The caller does not have permission"
	msgContext    = "The input token count exceeds the maximum number of tokens allowed 1048576."
)

// violation is one google.rpc.BadRequest field violation.
type violation struct {
	field string // snake_case path; "" for a top-level field
	desc  string
}

// invalid is a 400 INVALID_ARGUMENT with msg and no details.
func invalid(msg string) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg}
}

// badRequest is a 400 whose details list the violations; the message is
// their descriptions joined by newlines (observed). The JSON transcoder
// raises it before the key is looked at.
func badRequest(vs []violation) *core.Error {
	descs := make([]string, len(vs))
	for i, v := range vs {
		descs[i] = v.desc
	}
	return &core.Error{
		Kind: core.ErrInvalidRequest, Message: strings.Join(descs, "\n"),
		Extra: map[string]any{extraViolations: vs, driver.ExtraBeforeAuth: true},
	}
}

// noRoute is the 404 of a path Google serves nothing on.
func noRoute() *core.Error {
	return &core.Error{Kind: core.ErrNotFound, Message: "Not Found", Extra: map[string]any{extraNoRoute: true, driver.ExtraBeforeAuth: true}}
}

// backend marks e as raised after the model was resolved.
func backend(e *core.Error) *core.Error {
	if e.Extra == nil {
		e.Extra = map[string]any{}
	}
	e.Extra[extraBackend] = true
	return e
}

// rpcError is a rendered google.rpc.Status.
type rpcError struct {
	code    int
	status  string
	message string
	details []any
	headers [][2]string
}

func (r rpcError) obj() oai.Obj {
	e := oai.O("code", r.code, "message", r.message, "status", r.status)
	if len(r.details) > 0 {
		e = append(e, oai.Field{Key: "details", Val: r.details})
	}
	return oai.O("error", e)
}

func errorInfo(reason string, metadata oai.Obj, domain bool) oai.Obj {
	o := oai.O("@type", "type.googleapis.com/google.rpc.ErrorInfo", "reason", reason)
	if domain {
		o = append(o, oai.Field{Key: "domain", Val: "googleapis.com"})
	}
	return append(o, oai.Field{Key: "metadata", Val: metadata})
}

func localized(msg string) oai.Obj {
	return oai.O("@type", "type.googleapis.com/google.rpc.LocalizedMessage", "locale", "en-US", "message", msg)
}

const service = "generativelanguage.googleapis.com"

// keyError is the answer to a missing, unknown or refused key; which one
// depends on how the request carried its credential (observed): an API key
// that is not valid is a 400, no credential a 403, an OAuth bearer a 401.
func keyError(req *core.Request, e *core.Error) rpcError {
	h := reqHeaders(req)
	switch credentialOf(h).Scheme {
	case schemeBearer:
		r := routeOfReq(req)
		meta := oai.O("method", rpcMethod(r, h.Get(driver.MethodHeader)), "service", service)
		return rpcError{
			code: 401, status: "UNAUTHENTICATED", message: msgBearer,
			details: []any{errorInfo("CREDENTIALS_MISSING", meta, false)},
			headers: [][2]string{{"WWW-Authenticate", ""}},
		}
	case schemeNone:
		return rpcError{code: 403, status: "PERMISSION_DENIED", message: msgNoKey}
	}
	msg, reason := msgKeyInvalid, "API_KEY_INVALID"
	if strings.Contains(e.Message, "expired") {
		msg, reason = msgKeyExpired, "API_KEY_EXPIRED"
	}
	return rpcError{
		code: 400, status: "INVALID_ARGUMENT", message: msg,
		details: []any{errorInfo(reason, oai.O("service", service), true), localized(msg)},
	}
}

// quotaError is the 429 RESOURCE_EXHAUSTED with QuotaFailure, Help and
// RetryInfo (public report gemini-cli#8437; não conferido).
func quotaError(req *core.Request, e *core.Error) rpcError {
	metric, id := "generate_content_paid_tier_requests", "GenerateRequestsPerMinutePerProjectPerModel"
	switch l, _ := e.Extra["limit"].(string); l {
	case "tpm", "itpm", "otpm", "tokens":
		metric, id = "generate_content_paid_tier_input_token_count", "GenerateContentPaidTierInputTokensPerModelPerMinute"
	case "rpd", "daily":
		id = "GenerateRequestsPerDayPerProjectPerModel"
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	if model == "" {
		model = routeOfReq(req).model
	}
	details := []any{
		oai.O("@type", "type.googleapis.com/google.rpc.QuotaFailure", "violations", []any{oai.O(
			"quotaMetric", service+"/"+metric, "quotaId", id,
			"quotaDimensions", oai.O("location", "global", "model", model),
		)}),
		oai.O("@type", "type.googleapis.com/google.rpc.Help", "links", []any{oai.O(
			"description", "Learn more about Gemini API quotas", "url", "https://ai.google.dev/gemini-api/docs/rate-limits",
		)}),
	}
	if e.RetryAfter > 0 {
		secs := int((e.RetryAfter + time.Second - 1) / time.Second)
		details = append(details, oai.O("@type", "type.googleapis.com/google.rpc.RetryInfo", "retryDelay", strconv.Itoa(secs)+"s"))
	}
	return rpcError{code: 429, status: "RESOURCE_EXHAUSTED", message: msgQuota, details: details}
}

// modelNotFound is the 404 of a model the request names: the text names the
// method of the route (observed for generateContent and countTokens).
func modelNotFound(req *core.Request) rpcError {
	r := routeOfReq(req)
	name := r.model
	if req != nil && req.Model != "" {
		name = req.Model
	}
	method := r.method
	if method == methodStream {
		method = methodGenerate // the stream answers like generateContent (observed)
	}
	if method == "" {
		method = methodGenerate
	}
	return rpcError{code: 404, status: "NOT_FOUND", message: fmt.Sprintf(msgModelMethod, name, r.version, method)}
}

// render maps a core.Error to Google's status.
func render(req *core.Request, e *core.Error) rpcError {
	msg := e.Message
	or := func(def string) string {
		if msg != "" {
			return msg
		}
		return def
	}
	var r rpcError
	switch e.Kind {
	case core.ErrInvalidRequest, core.ErrMissingSession:
		r = rpcError{code: 400, status: "INVALID_ARGUMENT", message: or("Request contains an invalid argument.")}
		if vs, ok := e.Extra[extraViolations].([]violation); ok {
			list := make([]any, len(vs))
			for i, v := range vs {
				fv := oai.Obj{}
				if v.field != "" {
					fv = append(fv, oai.Field{Key: "field", Val: v.field})
				}
				list[i] = append(fv, oai.Field{Key: "description", Val: v.desc})
			}
			r.details = []any{oai.O("@type", "type.googleapis.com/google.rpc.BadRequest", "fieldViolations", list)}
		}
	case core.ErrContextLength:
		r = rpcError{code: 400, status: "INVALID_ARGUMENT", message: msgContext}
	case core.ErrRequestTooLarge:
		r = rpcError{code: 400, status: "INVALID_ARGUMENT", message: msgTooLarge}
	case core.ErrGeoBlocked:
		r = rpcError{code: 400, status: "FAILED_PRECONDITION", message: msgGeo}
	case core.ErrInvalidAPIKey:
		r = keyError(req, e)
	case core.ErrAccountBanned, core.ErrAccountSuspended:
		key := driver.AccountOf(req).Key
		r = rpcError{
			code: 403, status: "PERMISSION_DENIED", message: fmt.Sprintf(msgSuspended, key),
			details: []any{errorInfo("CONSUMER_SUSPENDED", oai.O("consumer", "projects/0", "service", service), true)},
		}
	case core.ErrPermission, core.ErrContentFilter:
		r = rpcError{code: 403, status: "PERMISSION_DENIED", message: or(msgPermission)}
	case core.ErrModelNotFound:
		r = modelNotFound(req)
	case core.ErrRateLimit, core.ErrQuota, core.ErrSlowDown, core.ErrSpendLimit, core.ErrBudget:
		r = quotaError(req, e)
	case core.ErrOverloaded:
		r = rpcError{code: 503, status: "UNAVAILABLE", message: msgOverloaded}
	case core.ErrGatewayTimeout:
		r = rpcError{code: 504, status: "DEADLINE_EXCEEDED", message: msgDeadline}
	default: // server_error, bad_gateway and anything new
		r = rpcError{code: 500, status: "INTERNAL", message: msgInternal}
	}
	if e.Status != 0 {
		r.code = e.Status
	}
	if m, ok := e.Extra[extraMessage].(string); ok {
		r.message = m
	}
	return r
}

// WriteError answers e in the Gemini envelope. On a stream route the body
// is the same prettyPrint JSON, under text/event-stream with alt=sse
// (observed: no "data:" frame) or as a one-element JSON array without it.
func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, _ *core.RateLimitState) {
	if e == nil {
		e = &core.Error{Kind: core.ErrServerError}
	}
	if b, ct, ok := e.RawBody(); ok {
		w.Status(statusOf(e, 500))
		w.Header("Content-Type", ct)
		w.Header("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
		_ = w.Flush()
		return
	}
	if e.Extra[extraNoRoute] == true || e.Kind == core.ErrNotFound || e.Kind == core.ErrMethodNotAllowed || unservedCredential(req, e) {
		w.Status(404)
		w.Header("Content-Type", "text/html")
		w.Header("Content-Length", "0")
		w.Header("Server", "scaffolding on HTTPServer2")
		securityHeaders(w)
		serverTiming(w, req)
		_ = w.Flush()
		return
	}
	if e.Kind == core.ErrBadGateway {
		writeBadGateway(w)
		return
	}
	if e.Kind == core.ErrContentFilter {
		writeBlocked(w, req)
		return
	}
	r := render(req, e)
	body := pretty(r.obj())
	ct := contentJSON
	if rt := routeOfReq(req); rt.method == methodStream {
		if sse(reqHeaders(req)) {
			ct = contentSSE
		} else {
			body = append(append([]byte("["), body...), ']')
		}
	}
	w.Status(r.code)
	if e.Extra[extraBackend] == true {
		w.Header("X-Gemini-Service-Tier", "standard")
	}
	for _, kv := range r.headers {
		w.Header(kv[0], kv[1])
	}
	w.Header("Content-Type", ct)
	w.Header("Content-Length", strconv.Itoa(len(body)))
	commonHeaders(w, req)
	_, _ = w.Write(body)
	_ = w.Flush()
}

// unservedCredential reports a credential error (bad, missing, suspended
// key; OAuth bearer) on a route Google serves nothing on: the real answers
// the empty 404 there before it looks at the credential (observed, task
// 265). The engine checks the key before it hands the route to Extra.
func unservedCredential(req *core.Request, e *core.Error) bool {
	switch e.Kind {
	case core.ErrInvalidAPIKey, core.ErrAccountBanned, core.ErrAccountSuspended:
	default:
		return false
	}
	method := reqHeaders(req).Get(driver.MethodHeader)
	return method != "" && !served(routeOfReq(req), method)
}

// statusOf is e.Status or def.
func statusOf(e *core.Error, def int) int {
	if e.Status != 0 {
		return e.Status
	}
	return def
}

// badGatewayPage is the front end's 502 page (Google's error page layout;
// não conferido).
const badGatewayPage = `<!DOCTYPE html>
<html lang=en>
  <meta charset=utf-8>
  <meta name=viewport content="initial-scale=1, minimum-scale=1, width=device-width">
  <title>Error 502 (Server Error)!!1</title>
  <a href=//www.google.com/><span id=logo aria-label=Google></span></a>
  <p><b>502.</b> <ins>That’s an error.</ins>
  <p>The server encountered a temporary error and could not complete your request.<p>Please try again in 30 seconds.  <ins>That’s all we know.</ins>
`

func writeBadGateway(w core.ResponseWriter) {
	w.Status(502)
	w.Header("Content-Type", "text/html; charset=UTF-8")
	w.Header("Content-Length", strconv.Itoa(len(badGatewayPage)))
	securityHeaders(w)
	_, _ = w.Write([]byte(badGatewayPage))
	_ = w.Flush()
}
