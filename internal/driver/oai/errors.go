package oai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// OpenAIErrorBody maps an error to OpenAI's envelope, rendered in the real
// byte layout (the body is a []byte).
func OpenAIErrorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	var (
		status int
		typ    string
		code   any
	)
	msg := e.Message
	hdr := map[string]string{}
	limit := ""
	if e.Extra != nil {
		if s, ok := e.Extra["limit"].(string); ok {
			limit = s
		}
	}
	switch e.Kind {
	case core.ErrInvalidRequest:
		status, typ = 400, "invalid_request_error"
	case core.ErrContextLength:
		status, typ, code = 400, "invalid_request_error", "context_length_exceeded"
	case core.ErrContentFilter:
		status, typ, code = 400, "invalid_request_error", "content_filter"
	case core.ErrInvalidAPIKey:
		status, typ, code = 401, "invalid_request_error", "invalid_api_key"
		if msg == "" {
			msg = "Incorrect API key provided. You can find your API key at https://platform.openai.com/account/api-keys."
			if k := presentedKey(req); k != "" {
				msg = "Incorrect API key provided: " + MaskKey(k) + ". You can find your API key at https://platform.openai.com/account/api-keys."
			}
		}
	case core.ErrPermission:
		status, typ, code = 403, "invalid_request_error", "permission_denied"
	case core.ErrGeoBlocked:
		status, typ, code = 403, "request_forbidden", "unsupported_country_region_territory"
		if msg == "" {
			msg = "Country, region, or territory not supported"
		}
	case core.ErrAccountSuspended:
		status, typ, code = 403, "invalid_request_error", "account_suspended"
	case core.ErrAccountBanned:
		status, typ, code = 403, "invalid_request_error", "account_deactivated"
		if msg == "" {
			msg = "Your account has been deactivated. Please check your email for more information."
		}
	case core.ErrModelNotFound:
		status, typ, code = 404, "invalid_request_error", "model_not_found"
		if msg == "" && deprecatedModel(req, e) {
			msg = "The model `" + req.Model + "` has been deprecated, learn more here: https://platform.openai.com/docs/deprecations"
		}
		if msg == "" {
			msg = fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", req.Model)
		}
	case core.ErrNotFound:
		status, typ = 404, "invalid_request_error"
	case core.ErrMethodNotAllowed:
		status, typ = 405, "invalid_request_error"
	case core.ErrRequestTooLarge:
		status, typ, code = 413, "invalid_request_error", "request_too_large"
	case core.ErrRateLimit:
		status, code = 429, "rate_limit_exceeded"
		typ = "requests"
		if strings.Contains(limit, "tpm") || strings.Contains(limit, "tokens") || strings.Contains(limit, "tpd") {
			typ = "tokens"
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(CeilSeconds(e.RetryAfter), 10)
			hdr["retry-after-ms"] = strconv.FormatInt(e.RetryAfter.Milliseconds(), 10)
		}
	case core.ErrSlowDown:
		status, typ, code = 429, "rate_limit_error", "slow_down"
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(CeilSeconds(e.RetryAfter), 10)
		}
	case core.ErrQuota:
		status, typ, code = 429, "insufficient_quota", "insufficient_quota"
		if msg == "" {
			msg = "You exceeded your current quota, please check your plan and billing details. For more information on this error, read the docs: https://platform.openai.com/docs/guides/error-codes/api-errors."
		}
		if limit == "credits" {
			code = "credit_balance_exhausted"
		}
	case core.ErrSpendLimit:
		status, typ, code = 429, "insufficient_quota", "organization_spend_limit_exceeded"
	case core.ErrBudget:
		status, typ, code = 429, "insufficient_quota", "project_spend_limit_exceeded"
	case core.ErrServerError:
		status, typ = 500, "server_error"
		if msg == "" {
			msg = "The server had an error while processing your request. Sorry about that!"
		}
	case core.ErrBadGateway:
		return 502, []byte("<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center><hr><center>cloudflare</center></body></html>"), "text/html", hdr
	case core.ErrOverloaded:
		status, typ, code = 503, "service_unavailable_error", "server_is_overloaded"
		if msg == "" {
			msg = "The server is currently overloaded with other requests. Please retry your request after a brief wait."
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(CeilSeconds(e.RetryAfter), 10)
		}
	case core.ErrGatewayTimeout:
		status, typ = 504, "server_error"
		if msg == "" {
			msg = "Gateway timeout"
		}
	default:
		status, typ = 500, "server_error"
	}
	if msg == "" {
		msg = openAIDefaultMessage(req, e, typ)
	}
	if e.Status > 0 {
		status = e.Status
	}
	if e.Code != "" {
		code = e.Code
	}
	if e.Kind == core.ErrInvalidAPIKey && status == 401 && msg == NoKeyMessage {
		// No key at all: the edge answers, in its layout, with the long
		// text and code null, without x-error-json (fixtures
		// auth-order/nokey_*, ADR 0053).
		b, _ := edgeFormat.Marshal(openAIEnvelope(noKeyText, typ, e.Param, nil))
		return status, b, "application/json; charset=utf-8", hdr
	}
	if e.Kind == core.ErrInvalidAPIKey && status == 401 {
		// The key check answers in text/plain, with status in the body and
		// the body again, base64, in x-error-json (spec 006 16.7).
		env := O("error", O("message", msg, "type", typ, "code", code, "param", nilIfEmpty(e.Param)), "status", status)
		b, _ := apiFormat.Marshal(env)
		hdr["x-openai-authorization-error"] = "401"
		hdr["x-openai-ide-error-code"] = fmt.Sprint(code)
		hdr["x-error-json"] = base64.StdEncoding.EncodeToString(b)
		return status, b, "text/plain", hdr
	}
	env := openAIEnvelope(msg, typ, e.Param, code)
	if edgeError(e, msg) && !apiLayout(req, e) {
		b, _ := edgeFormat.Marshal(env)
		return status, b, "application/json; charset=utf-8", hdr
	}
	b, _ := apiFormat.Marshal(env)
	return status, b, "application/json", hdr
}

// openAIEnvelope is {"error": {message, type, param, code}}.
func openAIEnvelope(msg, typ, param string, code any) Obj {
	return O("error", O("message", msg, "type", typ, "param", nilIfEmpty(param), "code", code))
}

// The two byte layouts of OpenAI's errors (testdata/real/openai): the API
// answers body validation and the key check indented with 2 spaces and no
// final newline; the edge in front of it answers unknown models and routes
// and unparsable bodies indented with 4 spaces, a final newline and
// "; charset=utf-8". The layout of the other kinds (limits, 5xx) is not
// conferred; they keep the API's.
var (
	apiFormat  = Format{Indent: "  ", RawHTML: true}
	edgeFormat = Format{Indent: "    ", Newline: true, RawHTML: true}
)

// ExtraModelRetired is the core.Error Extra key the engine sets (true) when
// the model asked for exists but is past models[].retires_at.
const ExtraModelRetired = "model_retired"

// deprecatedModel reports whether e is the Responses API's answer to a
// retired model: 404 model_not_found "has been deprecated", in the API's
// layout (deprecated_model). Chat Completions keeps the edge's "does not
// exist" (not conferred for retired models).
func deprecatedModel(req *core.Request, e *core.Error) bool {
	retired, _ := e.Extra[ExtraModelRetired].(bool)
	return retired && e.Kind == core.ErrModelNotFound && req.Endpoint == core.EndpointResponses
}

// ExtraAPILayout is the core.Error Extra key (true) of an error the API
// answers itself where Chat Completions has the edge answer: the 404s of
// the Responses API (unknown_model, conversation_not_found).
const ExtraAPILayout = "wwz:api_layout"

// apiLayout reports whether e is rendered in the API's layout even though
// its kind is the edge's.
func apiLayout(req *core.Request, e *core.Error) bool {
	flag, _ := e.Extra[ExtraAPILayout].(bool)
	return flag || deprecatedModel(req, e)
}

// MissingModelMessage is the edge's answer to a body without model.
const MissingModelMessage = "you must provide a model parameter"

// edgeError reports whether the edge, not the API, answers e: unknown
// model or route, unparsable body, body without model, request too large.
func edgeError(e *core.Error, msg string) bool {
	switch e.Kind {
	case core.ErrModelNotFound, core.ErrNotFound, core.ErrMethodNotAllowed, core.ErrRequestTooLarge:
		return true
	case core.ErrInvalidRequest:
		return msg == MissingModelMessage || strings.HasPrefix(msg, "We could not parse the JSON body")
	}
	return false
}

// NoKeyMessage is the engine's text for a request without a key.
const NoKeyMessage = "You didn't provide an API key."

// noKeyText is OpenAI's 401 without a key (fixtures auth-order/nokey_*).
const noKeyText = "You didn't provide an API key. You need to provide your API key in an Authorization header using Bearer auth (i.e. Authorization: Bearer YOUR_KEY), or as the password field (with blank username) if you're accessing the API from your browser and are prompted for a username and password. You can obtain an API key from https://platform.openai.com/account/api-keys."

// MaskKey is how the 401 shows the presented key: the first 8 and the last
// 4 characters around one asterisk per hidden character
// (sk-invalid-wwz-drift -> sk-inval********rift,
// sk-invalid-wwz-0000000000000000 -> sk-inval*******************0000;
// fixtures auth-order/badkey_*). The rule for keys of 12 characters or
// fewer is not conferred: they keep half in front, a quarter at the end and
// 8 asterisks.
func MaskKey(k string) string {
	if len(k) > 12 {
		return k[:8] + strings.Repeat("*", len(k)-12) + k[len(k)-4:]
	}
	pre, suf := len(k)/2, len(k)/4
	return k[:pre] + "********" + k[len(k)-suf:]
}

// presentedKey is the bearer key of the request ("" when absent).
func presentedKey(req *core.Request) string {
	auth := req.Header("authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

// CeilSeconds is a retry-after value: d rounded up to whole seconds, at
// least 1 (45 s stays 45, 1.5 s becomes 2).
func CeilSeconds(d time.Duration) int64 {
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

// openAIDefaultMessage is OpenAI's text for an error raised without one
// (engine limits, chaos): real responses always carry a message. typ is the
// envelope type already chosen (requests or tokens for rate limits).
func openAIDefaultMessage(req *core.Request, e *core.Error, typ string) string {
	switch e.Kind {
	case core.ErrInvalidRequest:
		return "We could not parse the JSON body of your request. (HINT: This likely means you aren't using your HTTP library correctly. The OpenAI API expects a JSON payload, but what was sent was not valid JSON.)"
	case core.ErrContextLength:
		return "This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens. Please reduce the length of the messages."
	case core.ErrContentFilter:
		return "Your request was rejected as a result of our safety system. Your prompt may contain text that is not allowed by our safety system."
	case core.ErrPermission:
		return "You have insufficient permissions for this operation. Missing scopes: model.request. Check that you have the correct role in your organization (Reader, Writer, Owner) and project (Member, Owner), and if you're using a restricted API key, that it has the necessary scopes."
	case core.ErrAccountSuspended:
		return "Your organization has been suspended. Please contact us through our help center at help.openai.com if you have additional questions."
	case core.ErrNotFound, core.ErrMethodNotAllowed:
		if p := req.Header("x-wwz-path"); p != "" {
			return "Invalid URL (" + p + ")"
		}
		return "Invalid URL"
	case core.ErrRequestTooLarge:
		return "Request too large. The request body exceeds the maximum allowed size."
	case core.ErrRateLimit:
		unit, abbr := "requests per min", "RPM"
		limit, _ := e.Extra["limit"].(string)
		switch {
		case limit == "rpd":
			unit, abbr = "requests per day", "RPD"
		case limit == "tpd":
			unit, abbr = "tokens per day", "TPD"
		case typ == "tokens":
			unit, abbr = "tokens per min", "TPM"
		}
		msg := "Rate limit reached"
		if req.Model != "" {
			msg += " for " + req.Model
		}
		msg += fmt.Sprintf(" in organization %s on %s (%s)", OrganizationID(req), unit, abbr)
		if lim, used, asked, ok := LimitNumbers(e); ok {
			msg += fmt.Sprintf(": Limit %d, Used %d, Requested %d.", lim, used, asked)
		} else {
			msg += "."
		}
		if e.RetryAfter > 0 {
			msg += " Please try again in " + goDuration(e.RetryAfter) + "."
		}
		return msg + " Visit https://platform.openai.com/account/rate-limits to learn more."
	case core.ErrSlowDown:
		return "Your request rate is increasing too quickly. Please slow down and retry with exponential backoff."
	case core.ErrSpendLimit:
		return "You have exceeded your organization's spend limit. Increase the limit in your billing settings to continue."
	case core.ErrBudget:
		return "You have exceeded your project's spend limit. Increase the project budget in your project settings to continue."
	}
	return "The server had an error while processing your request. Sorry about that!"
}

// LimitNumbers reads the figures of a limits denial the engine attaches to
// the error (Extra limit_value, limit_used, limit_requested); ok is false
// when the denial carried none (chaos, rps, concurrency, daily limits).
func LimitNumbers(e *core.Error) (limit, used, requested int, ok bool) {
	if e == nil || e.Extra == nil {
		return 0, 0, 0, false
	}
	limit, ok = e.Extra["limit_value"].(int)
	if !ok || limit <= 0 {
		return 0, 0, 0, false
	}
	used, _ = e.Extra["limit_used"].(int)
	requested, _ = e.Extra["limit_requested"].(int)
	return limit, used, requested, true
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// OpenAIStreamError is the chunk carrying an error mid-stream.
// It is compact, like every chunk.
func OpenAIStreamError(e *core.Error) any {
	_, body, _, _ := OpenAIErrorBody(&core.Request{}, e)
	b, _ := body.([]byte)
	var c bytes.Buffer
	if json.Compact(&c, b) != nil { // the HTML 502 page
		return body
	}
	return json.RawMessage(c.Bytes())
}

func goDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "0s"
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return d.Round(time.Millisecond).String()
}
