package opencode

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Errors as the real gateway answers them (spec 006 16.3 and 16.7,
// conferred on 2026-10-31, task 175; testdata/real/opencode/shape/):
//
//   - upstream failures: {"error":{"type":"server_error","message":
//     "Upstream request failed: <msg>"}} on Chat Completions and
//     {"type":"error","error":{"type":"api_error","message":...}} on
//     Messages (401 invalid or missing key, 400 unknown model);
//   - the gateway's own errors: {"type":"error","error":{"type":"<Name>",
//     "message":...}} on both routes (MissingSessionID, conferred), with
//     metadata for the quota and budget errors (the official client's
//     fixtures, not conferred).
//
// Every answer carries x-opencode-log-id and no request id.

// upstreamPrefix starts the text of every upstream failure.
const upstreamPrefix = "Upstream request failed: "

// Texts conferred on the real gateway.
const (
	invalidCredential = "Invalid credential"       //nolint:gosec // G101: an error text, not a credential
	missingCredential = "Go requires a credential" //nolint:gosec // G101: an error text, not a credential
	modelUnavailable  = "Model is unavailable."
	missingSession    = "Request is missing x-opencode-session and cannot be routed efficiently. Please see https://opencode.ai/v2/docs/console/go/#where-can-i-use-it"
)

// noKeyMessage is the engine's text for a request without a key.
const noKeyMessage = "You didn't provide an API key."

// extraZenModel marks the 400 of a Messages body without max_tokens: the
// gateway answers {"model": "<model>"} with x-zen-model (conferred).
const extraZenModel = "opencode_zen_model"

// gatewayTypes are the error types the gateway raises itself; they use the
// {type: "error", error, metadata?} envelope on both routes.
var gatewayTypes = map[string]bool{
	"MissingSessionID": true, "GoUsageLimitError": true, "FreeUsageLimitError": true,
	"BudgetLimitError": true, "CreditLimitExceeded": true, typeAuth: true, typeForbidden: true,
}

// presentedKey is the key of the request: the one the engine resolved, or
// what Authorization carries.
func presentedKey(req *core.Request) string {
	if k := driver.AccountOf(req).Key; k != "" {
		return k
	}
	if req == nil {
		return ""
	}
	if auth := req.Header("authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if isGoogle(req) {
		return req.Header("x-goog-api-key")
	}
	if req.Endpoint == core.EndpointMessages {
		return req.Header("x-api-key")
	}
	return ""
}

// missingKey reports whether e is the refusal of a missing credential (no
// key presented, or public).
func missingKey(req *core.Request, e *core.Error) bool {
	return e.Message == noKeyMessage || (e.Message == "" && e.Kind == core.ErrInvalidAPIKey && presentedKey(req) == "")
}

// payAsYouGo reports whether req came through a pay-as-you-go base (the
// catalog the engine resolved for the route).
func payAsYouGo(req *core.Request) bool {
	return driver.CatalogOfRequest(req) == catalogPAYG
}

// workspaceID is metadata.workspace of GoUsageLimitError: "wrk_" and 26
// Crockford base32 characters of the sha256 of the account id (the key
// when it has no account); the format is not conferred (ADR 0072, item 12).
func workspaceID(req *core.Request) string {
	id := driver.AccountOf(req).Account
	if id == "" {
		id = presentedKey(req)
	}
	return workspaceOf(id)
}

// workspaceOf is the workspace id of account id (workspaceID); the
// Console's org_id and api/orgs show the same.
func workspaceOf(id string) string {
	return "wrk_" + hashID(id)
}

// hashID is 26 Crockford base32 characters of the sha256 of s: the body of
// the wwz's wrk_, usr_ and rul_ ids (formats not conferred).
func hashID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return crockford.EncodeToString(sum[:])[:26]
}

// crockford is Crockford's base32 alphabet, upper case, without padding
// (the alphabet of ULIDs).
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// nextMonth is 00:00 UTC of the 1st of the month after now: the reset of
// the budgets (resetsAt of BudgetLimitError).
func nextMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// budgetCoverage is metadata.coverage of BudgetLimitError: models for a
// budget of spend_monthly_models_usd (the model_spend entity) and for one
// raised without an entity (chaos), all for spend_monthly_usd.
func budgetCoverage(e *core.Error) string {
	if k, ok := e.Extra["entity_kind"].(string); ok && k != "" && k != "model_spend" {
		return "all"
	}
	return "models"
}

// errorParts is the status, type, message, metadata and extra headers of
// e, independent of the route.
func (d *Driver) errorParts(req *core.Request, e *core.Error) (int, string, string, oai.Obj, map[string]string) {
	status, typ, msg := 500, "server_error", e.Message
	hdr := map[string]string{"x-opencode-log-id": newLogID(time.Now())}
	var meta oai.Obj
	limit := ""
	if s, ok := e.Extra["limit"].(string); ok {
		limit = s
	}
	switch e.Kind {
	case core.ErrMissingSession:
		status, typ, msg = 400, "MissingSessionID", missingSession
	case core.ErrInvalidRequest, core.ErrContextLength, core.ErrContentFilter:
		status, typ = 400, "invalid_request_error"
	case core.ErrInvalidAPIKey, core.ErrAccountBanned:
		status = 401
		missing := e.Kind == core.ErrInvalidAPIKey && missingKey(req, e)
		switch {
		case missing:
			msg = upstreamPrefix + missingCredential
		case msg == "" || e.Kind == core.ErrAccountBanned:
			msg = upstreamPrefix + invalidCredential
		default:
			msg = upstreamPrefix + msg
		}
		switch {
		case isUsageRoute(req):
			// /v1/usage: the gateway's AuthError, with or without a key.
			typ, msg, hdr = usageAuthType, usageAuthMessage, modelsHeaderMap()
		case req != nil && req.Endpoint == "":
			// /v1/models with an invalid key: the text without the
			// upstream prefix, with the listing's headers (conferred).
			msg = strings.TrimPrefix(msg, upstreamPrefix)
			hdr = modelsHeaderMap()
		case missing && payAsYouGo(req):
			// The pay-as-you-go bases: the gateway's AuthError (conferred
			// with public and a paid model, free/public_paid_model.json;
			// without any credential, not conferred).
			typ, msg = typeAuth, missingAPIKey
		}
	case core.ErrModelNotFound:
		status, msg = 400, upstreamPrefix+modelUnavailable
	case core.ErrNotFound:
		status, typ = 404, "not_found_error"
	case core.ErrMethodNotAllowed:
		status, typ = 405, "invalid_request_error"
	case core.ErrPermission, core.ErrGeoBlocked, core.ErrAccountSuspended:
		status, typ = 403, "permission_error"
	case core.ErrRequestTooLarge:
		status, typ = 413, "request_too_large"
	case core.ErrQuota:
		switch limit {
		case "credits":
			status, msg = 402, upstreamPrefix+"Insufficient account funds"
		case "quota_5h", "quota_weekly", "quota_monthly":
			status, typ, msg = 429, "GoUsageLimitError", "Go usage limit exceeded"
			name := map[string]string{"quota_5h": "5 hour", "quota_weekly": "weekly", "quota_monthly": "monthly"}[limit]
			meta = oai.O("workspace", workspaceID(req), "limitName", name)
		default:
			status, typ = 402, "CreditLimitExceeded"
			if msg == "" {
				msg = "Credit limit exceeded."
			}
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(e.RetryAfter), 10)
		}
	case core.ErrBudget, core.ErrSpendLimit:
		status, typ = 429, "BudgetLimitError"
		if msg == "" {
			msg = "A budget policy for this model has been reached."
		}
		meta = oai.O("coverage", budgetCoverage(e), "resetsAt", nextMonth(time.Now()).Format(time.RFC3339))
	case core.ErrRateLimit, core.ErrSlowDown:
		if free, _ := e.Extra[extraFreeModels].(bool); free {
			// The daily count of the free models, and the gate of a
			// client outside OpenCode: the gateway's envelope without
			// metadata, retry-after to 00:00 UTC (conferred,
			// free/public_generic_client.json).
			status, typ, msg = 429, typeFreeUsage, freeUsageMessage
			hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(untilMidnight(time.Now())), 10)
			break
		}
		status, msg = 429, upstreamPrefix+"Rate limit exceeded."
		ra := e.RetryAfter
		if ra == 0 {
			ra = 45 * time.Second
		}
		hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(ra), 10)
	case core.ErrServerError:
		if msg == "" {
			msg = upstreamPrefix + "Internal error"
		}
	case core.ErrBadGateway:
		status, msg = 502, upstreamPrefix+"[502] Bad Gateway"
	case core.ErrOverloaded:
		status, msg = 529, upstreamPrefix+"Overloaded"
	case core.ErrGatewayTimeout:
		status, msg = 504, upstreamPrefix+"Gateway timeout"
	}
	if msg == "" {
		msg = openCodeDefaultMessage(e)
	}
	if e.Status > 0 {
		status = e.Status
	}
	if e.Code != "" {
		typ = e.Code
	}
	return status, typ, msg, meta, hdr
}

// errorBody is the Chat Completions envelope (oai.Hooks.ErrorBody).
func (d *Driver) errorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	if model, ok := e.Extra[extraZenModel].(string); ok {
		return zenModelError(model)
	}
	status, typ, msg, meta, hdr := d.errorParts(req, e)
	var body oai.Obj
	if gatewayTypes[typ] {
		body = oai.O("type", "error", "error", oai.O("type", typ, "message", msg))
		if meta != nil {
			body = append(body, oai.Field{Key: "metadata", Val: meta})
		}
	} else {
		body = oai.O("error", oai.O("type", typ, "message", msg))
	}
	b, _ := jsonFormat.Marshal(body)
	return status, b, "application/json", hdr
}

// anthErrorBody is the Messages envelope (anth.Hooks.ErrorBody): the
// Anthropic {type, error} with api_error for the upstream failures.
func (d *Driver) anthErrorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	if model, ok := e.Extra[extraZenModel].(string); ok {
		return zenModelError(model)
	}
	status, typ, msg, meta, hdr := d.errorParts(req, e)
	if typ == "server_error" {
		typ = "api_error"
	}
	if model, ok := e.Extra[extraUpstreamModel].(string); ok {
		hdr["x-zen-model"] = model
	}
	m := map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
	if meta != nil {
		m["metadata"] = metaMap(meta)
	}
	return status, m, "application/json", hdr
}

// metaMap turns metadata into the map form anth orders (sorted keys).
func metaMap(o oai.Obj) map[string]any {
	m := make(map[string]any, len(o))
	for _, f := range o {
		m[f.Key] = f.Val
	}
	return m
}

// zenModelError is the 400 of a Messages body without max_tokens.
func zenModelError(model string) (int, any, string, map[string]string) {
	b, _ := jsonFormat.Marshal(oai.O("model", model))
	return 400, b, "application/json", map[string]string{"x-opencode-log-id": newLogID(time.Now()), "x-zen-model": model}
}

// openCodeDefaultMessage is the gateway's text for an error raised without
// one (engine limits, chaos): its own errors are short sentences, upstream
// failures are prefixed "Upstream request failed:" (spec 006 §16.3).
func openCodeDefaultMessage(e *core.Error) string {
	switch e.Kind {
	case core.ErrInvalidRequest:
		return "Invalid request body"
	case core.ErrContextLength:
		return upstreamPrefix + "Input exceeds the model context window"
	case core.ErrContentFilter:
		return upstreamPrefix + "Content blocked by the provider safety filter"
	case core.ErrNotFound:
		return "Not Found"
	case core.ErrMethodNotAllowed:
		return "Method Not Allowed"
	case core.ErrPermission:
		return "This API key does not have access to the requested resource"
	case core.ErrGeoBlocked:
		return "OpenCode is not available in your region"
	case core.ErrAccountSuspended:
		return suspendedMessage
	case core.ErrRequestTooLarge:
		return "Request body too large"
	}
	return upstreamPrefix + "Internal error"
}

// streamError is the mid-stream error chunk: the route's envelope (not
// conferred).
func (d *Driver) streamError(e *core.Error) any {
	_, body, _, _ := d.errorBody(&core.Request{Endpoint: core.EndpointChat}, e)
	b, _ := body.([]byte)
	return json.RawMessage(b)
}

// jsonFormat is the gateway's byte layout: compact, < > & as they are.
var jsonFormat = oai.Format{RawHTML: true}
