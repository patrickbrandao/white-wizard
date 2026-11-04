// Package openrouter is the OpenRouter driver: OpenAI Chat Completions under
// /api/v1 with OpenRouter's error envelope, usage.cost, keep-alive comments
// and credit-based limits.
package openrouter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// providerName is the provider field: the upstream that served the request.
const providerName = "WhiteWizard"

func init() {
	driver.Register("openrouter", func(opts map[string]string) (driver.Driver, error) {
		d := &Driver{}
		field, err := oai.GatewayReasoningField(opts["reasoning_field"], "reasoning")
		if err != nil {
			return nil, fmt.Errorf("openrouter: %w", err)
		}
		d.oai = oai.Hooks{
			Name: "openrouter", IDPrefix: "gen", ReasoningField: field, ReasoningDetails: true,
			FinalChunkWithChoice: true, NativeFinishReason: true, NativeFinish: nativeFinish, NoRoleChunk: true,
			ErrorBody: d.errorBody, StreamErrorBody: d.streamError, SuccessHeaders: chatHeaders,
			UsageExtras: chatUsageExtras, TopExtras: func(*core.Request, *core.Response) map[string]any {
				return map[string]any{"provider": providerName}
			},
			Shape:       shapeChat,
			BodyFormat:  oai.Format{RawHTML: true, Prefix: bodyPrefix},
			ChunkFormat: oai.Format{RawHTML: true},
		}
		d.anth = anth.DefaultHooks()
		d.anth.Name = "openrouter"
		d.anth.RequireVersion = false
		d.anth.ErrorBody = d.anthErrorBody
		d.anth.RateLimitHeaders = nil
		d.anth.SuccessHeaders = messagesHeaders
		d.anth.UsageExtras = messagesUsageExtras
		d.anth.Shape = shapeMessages
		d.anth.ToolChoice = toolChoiceRules
		return d, nil
	})
}

// Driver implements driver.Driver for OpenRouter.
type Driver struct {
	oai  oai.Hooks
	anth anth.Hooks
}

func (d *Driver) Name() string     { return "openrouter" }
func (d *Driver) Protocol() string { return "openrouter" }

func (d *Driver) Routes() []driver.Route {
	return []driver.Route{
		{Method: "POST", Path: "/api/v1/chat/completions", Kind: driver.KindChat},
		{Method: "POST", Path: "/api/v1/completions", Kind: driver.KindCompletions},
		{Method: "POST", Path: "/api/v1/messages", Kind: driver.KindMessages},
		{Method: "POST", Path: responsesPath, Kind: driver.KindResponses},
		{Method: "GET", Path: "/api/v1/models", Kind: driver.KindModels},
		{Method: "GET", Path: "/api/v1/models/*", Kind: driver.KindModel},
		{Method: "GET", Path: modelPath + "*", Kind: driver.KindModel},
		{Method: "GET", Path: keyPath, Kind: driver.KindKeyInfo},
		{Method: "GET", Path: authKeyPath, Kind: driver.KindKeyInfo},
		{Method: "GET", Path: creditsPath, Kind: driver.KindKeyInfo},
		{Method: "GET", Path: keysPath, Kind: driver.KindKeyInfo},
		{Method: "POST", Path: keysPath, Kind: driver.KindKeyInfo},
	}
}

func (d *Driver) Sniff(h driver.Headers) bool {
	auth := h.Get("authorization")
	return strings.HasPrefix(strings.TrimPrefix(auth, "Bearer "), "sk-or-")
}

// Authenticate reads Authorization: Bearer, else x-api-key: the real
// accepts both on every route (conferred on 2026-10-31, task 174).
func (d *Driver) Authenticate(h driver.Headers) driver.Credential {
	auth := h.Get("authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		if k := strings.TrimSpace(auth[7:]); k != "" {
			return driver.Credential{Key: k, Scheme: "bearer"}
		}
	}
	if k := strings.TrimSpace(h.Get("x-api-key")); k != "" {
		return driver.Credential{Key: k, Scheme: "x-api-key"}
	}
	return driver.Credential{Scheme: "none"}
}

func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	switch kind {
	case driver.KindChat:
		req, err := oai.ParseChat(lenientChat(body), h, d.Name())
		if req != nil {
			req.Raw = body
		}
		return req, err
	case driver.KindCompletions:
		return oai.ParseCompletions(body, d.Name())
	case driver.KindResponses:
		return d.parseResponses(body)
	case driver.KindMessages:
		req, err := anth.ParseMessages(&d.anth, lenientMessages(body), h, d.Name())
		if req != nil {
			req.Raw = body
		}
		return req, err
	}
	return nil, &core.Error{Kind: core.ErrNotFound, Message: "Not found"}
}

// lenientChat adapts a Chat Completions body to what the gateway accepts
// and the shared parser does not (spec 006 16.7, task 161 fixtures): the
// first of models when model is missing, and max_tokens,
// max_completion_tokens, n or seed of the wrong type ignored. Anything
// else is returned as is.
func lenientChat(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	changed := false
	var model string
	if json.Unmarshal(m["model"], &model) != nil || model == "" {
		var models []string
		if json.Unmarshal(m["models"], &models) == nil && len(models) > 0 && models[0] != "" {
			m["model"], _ = json.Marshal(models[0])
			changed = true
		}
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "n", "seed"} {
		var n int64
		if v, ok := m[k]; ok && string(v) != "null" && json.Unmarshal(v, &n) != nil {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, _ := json.Marshal(m) // a map of decoded raw messages always marshals
	return out
}

// lenientMessages: OpenRouter's Messages route accepts a body without
// max_tokens (the shared parser requires it); the answer is then bounded
// by the model only.
func lenientMessages(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["max_tokens"]; ok {
		return body
	}
	m["max_tokens"] = json.RawMessage("0")
	out, _ := json.Marshal(m) // a map of decoded raw messages always marshals
	return out
}

func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if req.Endpoint == core.EndpointMessages {
		return anth.WriteMessage(&d.anth, w, req, resp, nil)
	}
	if isResponses(req) {
		return writeResponses(w, req, resp)
	}
	return oai.WriteChat(&d.oai, w, req, resp, nil)
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	if req.Endpoint == core.EndpointMessages {
		return anth.NewStream(&d.anth, w, req, nil)
	}
	if isResponses(req) {
		return newRespStream(d, w, req)
	}
	return newStream(&d.oai, w, req, nil)
}

// chatHeaders are the headers of a Chat Completions 200: the generation id
// (the body's id) and the CORS list; the stream's Content-Type has no
// charset.
func chatHeaders(req *core.Request, resp *core.Response, _ *core.RateLimitState) map[string]string {
	h := map[string]string{"x-generation-id": resp.ID, "Access-Control-Expose-Headers": exposeHeaders, "Access-Control-Allow-Origin": allowOrigin}
	if req != nil && req.Stream {
		h["Content-Type"] = "text/event-stream"
	}
	return h
}

// messagesHeaders are the headers of a Messages 200: request-id and
// x-generation-id both carry the generation id (not the message id).
func messagesHeaders(req *core.Request, _ *core.Response, _ *core.RateLimitState) map[string]string {
	id := generationID(req)
	h := map[string]string{"request-id": id, "x-generation-id": id, "Access-Control-Expose-Headers": exposeHeaders, "Access-Control-Allow-Origin": allowOrigin}
	if req != nil && req.Stream {
		h["Content-Type"] = "text/event-stream"
	}
	return h
}

// genSeq makes the generation ids minted outside a request unique.
var genSeq atomic.Uint64

// generationID is the request's generation id ("gen-…", minted by
// RequestID), or a fresh one for an answer without a request.
func generationID(req *core.Request) string {
	if req != nil && strings.HasPrefix(req.RequestID, "gen-") {
		return req.RequestID
	}
	now := time.Now()
	return ids.OpenRouterGen(ids.FromToken(strconv.FormatInt(now.UnixNano(), 36)+"-"+strconv.FormatUint(genSeq.Add(1), 36)), now)
}

func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if req != nil && req.Endpoint == core.EndpointMessages {
		if req.RequestID == "" || !strings.HasPrefix(req.RequestID, "gen-") {
			req.RequestID = generationID(nil) // the body and the headers share it
		}
		anth.WriteError(&d.anth, w, req, e, nil)
		return
	}
	if e.Kind == core.ErrRateLimit && rl != nil && rl.Present {
		// Work on a copy: e (and its Extra) belongs to the caller.
		c := *e
		c.Extra = make(map[string]any, len(e.Extra)+3)
		for k, v := range e.Extra {
			c.Extra[k] = v
		}
		e = &c
		e.Extra["header:X-RateLimit-Limit"] = strconv.Itoa(rl.RequestsLimit)
		e.Extra["header:X-RateLimit-Remaining"] = strconv.Itoa(max(0, rl.RequestsRemaining))
		e.Extra["header:X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(rl.RequestsReset).UnixMilli(), 10)
		if l, _ := e.Extra["limit"].(string); freeModelsPerMinuteError(e, l) {
			e.Extra["header:X-RateLimit-Reset"] = freeModelsMinuteReset(time.Now())
		}
	}
	oai.WriteError(&d.oai, w, req, e, nil)
}

// openRouterRateLimitMessage is OpenRouter's 429 text: its own free-model
// limits name the bucket (free-models-per-min, free-models-per-day); a 429
// passed through from the provider says the model is rate-limited upstream.
func openRouterRateLimitMessage(req *core.Request, limit string, upstream bool) string {
	switch {
	case limit == "rpm" || limit == "rps":
		return "Rate limit exceeded: free-models-per-min."
	case limit == "rpd":
		return "Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day"
	case upstream && req != nil && req.Model != "":
		return req.Model + " is temporarily rate-limited upstream. Please retry shortly, or add your own key to accumulate your rate limits: https://openrouter.ai/settings/integrations"
	}
	return "Rate limit exceeded"
}

var errorTypes = map[core.ErrorKind]string{
	core.ErrInvalidRequest: "invalid_request", core.ErrContextLength: "context_length_exceeded", core.ErrInvalidAPIKey: "authentication",
	core.ErrPermission: "permission_denied", core.ErrModelNotFound: "not_found", core.ErrNotFound: "not_found", core.ErrRequestTooLarge: "payload_too_large",
	core.ErrRateLimit: "rate_limit_exceeded", core.ErrSlowDown: "rate_limit_exceeded", core.ErrQuota: "payment_required", core.ErrSpendLimit: "permission_denied",
	core.ErrBudget: "permission_denied", core.ErrServerError: "server", core.ErrBadGateway: "provider_unavailable", core.ErrOverloaded: "provider_overloaded",
	core.ErrGatewayTimeout: "timeout", core.ErrContentFilter: "content_policy_violation", core.ErrGeoBlocked: "permission_denied",
	core.ErrAccountSuspended: "permission_denied", core.ErrAccountBanned: "authentication", core.ErrMethodNotAllowed: "invalid_request",
}

// invalidModel reports whether e is the engine's miss on the model field (an
// ErrModelNotFound without a text of its own): OpenRouter answers it with
// 400 "<model> is not a valid model ID", with or without an author in the
// name (checked against the real API, task 163).
func invalidModel(req *core.Request, e *core.Error) bool {
	return e.Kind == core.ErrModelNotFound && e.Message == "" && e.Status == 0 && req != nil && req.Model != ""
}

func invalidModelMessage(req *core.Request) string { return req.Model + " is not a valid model ID" }

// variantMiss is the real 404 text of the engine's miss on a model variant
// (config.VariantMiss, task 186): ":free" of a model without a free variant
// names the paid slug as asked; ":FREE" of it, or a model served only as
// free asked without the suffix, has no endpoints.
func variantMiss(e *core.Error) (string, bool) {
	miss, ok := e.Extra[config.ExtraModelVariant].(*config.VariantMiss)
	if !ok || e.Kind != core.ErrModelNotFound {
		return "", false
	}
	if miss.Free {
		return "This model is unavailable for free. The paid version is available now - use this slug instead: " + miss.Name, true
	}
	return "No endpoints found for " + miss.Name + ".", true
}

// jsonString is s as a JSON string literal.
func jsonString(s string) string {
	b, _ := jsonFormat.Marshal(s)
	return string(b)
}

// realKey is the form of a real OpenRouter key: sk-or-v1- and 64 hex.
var realKey = regexp.MustCompile(`^sk-or-v1-[0-9a-f]{64}$`)

// presentedKey is the key of the request: the one the engine resolved, or
// what the headers carry.
func presentedKey(req *core.Request) string {
	if k := driver.AccountOf(req).Key; k != "" {
		return k
	}
	if req == nil {
		return ""
	}
	if auth := req.Header("authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		if k := strings.TrimSpace(auth[7:]); k != "" {
			return k
		}
	}
	return strings.TrimSpace(req.Header("x-api-key"))
}

// noKeyMessage is the engine's text for a request without a key.
const noKeyMessage = "You didn't provide an API key."

// authMessage is OpenRouter's 401 text, by the key presented (spec 006
// 16.7, fixtures auth-order/): none, a key outside the sk-or-v1-<64 hex>
// form, or a well-formed key of no account. Other texts of the engine
// (expired key, key of another driver) are kept.
func authMessage(req *core.Request, e *core.Error) string {
	if e.Message != "" && e.Message != noKeyMessage {
		return e.Message
	}
	switch k := presentedKey(req); {
	case k == "" || e.Message == noKeyMessage:
		return "No cookie auth credentials found"
	case e.Kind == core.ErrAccountBanned || realKey.MatchString(k):
		return "User not found."
	}
	return "Missing Authentication header"
}

func (d *Driver) errorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	hdr := map[string]string{"Access-Control-Expose-Headers": exposeHeaders, "Access-Control-Allow-Origin": allowOrigin}
	if r, ok := e.Extra[extraRawReply].(rawReply); ok {
		// The checks of the Responses route (responses.go).
		if r.generation {
			hdr["x-generation-id"] = generationID(req)
		}
		return r.status, []byte(r.body), "application/json", hdr
	}
	if invalidModel(req, e) {
		// The gateway's own envelope: no metadata, user_id of the account.
		hdr["x-generation-id"] = generationID(req)
		body := `{"error":{"message":` + jsonString(invalidModelMessage(req)) + `,"code":400},"user_id":"user_wwz"}`
		return 400, json.RawMessage(body), "application/json", hdr
	}
	if msg, ok := variantMiss(e); ok {
		// Conferred: chat_free_no_variant and the other variant
		// fixtures (task 186); no metadata, as the 400 above.
		hdr["x-generation-id"] = generationID(req)
		body := `{"error":{"message":` + jsonString(msg) + `,"code":404},"user_id":"user_wwz"}`
		return 404, json.RawMessage(body), "application/json", hdr
	}
	if (e.Kind == core.ErrInvalidAPIKey || e.Kind == core.ErrAccountBanned) && e.Status == 0 {
		// 401: message and code only (no metadata, no user_id, no
		// generation id).
		body := `{"error":{"message":` + jsonString(authMessage(req, e)) + `,"code":401}}`
		keyErrorHeaders(req, hdr)
		return 401, json.RawMessage(body), "application/json", hdr
	}
	hdr["x-generation-id"] = generationID(req)
	status := 500
	msg := e.Message
	meta := oai.Obj{}
	limit := ""
	if e.Extra != nil {
		if s, ok := e.Extra["limit"].(string); ok {
			limit = s
		}
	}
	switch e.Kind {
	case core.ErrInvalidRequest, core.ErrContextLength:
		status = 400
	case core.ErrInvalidAPIKey, core.ErrAccountBanned:
		status = 401
		msg = authMessage(req, e)
	case core.ErrQuota:
		status = 402
		switch limit {
		case "credits":
			meta.Set("limit_source", "openrouter_credits")
			meta.Set("reason", "insufficient_credits")
			if msg == "" {
				msg = "Insufficient credits. Add more using https://openrouter.ai/settings/credits"
			}
		case "inflight":
			meta.Set("limit_source", "openrouter_in_flight_budget")
			meta.Set("reason", "in_flight_budget_exhausted")
			hdr["retry-after"] = "2"
		default:
			meta.Set("limit_source", "openrouter_key_limit")
			if msg == "" {
				msg = "Key limit exceeded"
			}
		}
		meta.Set("remedy_hint", "Add credits or raise the key limit, then retry.")
	case core.ErrSpendLimit, core.ErrBudget:
		status = 403
		source := "openrouter_workspace_budget"
		if e.Kind == core.ErrBudget {
			source = "openrouter_guardrail_budget"
		}
		meta.Set("limit_source", source)
		meta.Set("scope", "workspace")
		if msg == "" {
			msg = "Budget limit exceeded (monthly limit). Resets at the start of the next period."
		}
	case core.ErrPermission, core.ErrGeoBlocked, core.ErrAccountSuspended:
		status = 403
	case core.ErrContentFilter:
		status = 403
		meta.Set("reasons", []any{"content_policy"})
		meta.Set("flagged_input", "...")
		meta.Set("provider_name", providerName)
	case core.ErrModelNotFound, core.ErrNotFound:
		status = 404
		if msg == "" {
			msg = fmt.Sprintf("No endpoints found for %s.", req.Model)
		}
	case core.ErrMethodNotAllowed:
		status = 405
	case core.ErrRequestTooLarge:
		status = 413
	case core.ErrRateLimit, core.ErrSlowDown:
		if b, ok := freeModelsPerMinuteBody(e, limit, hdr); ok {
			return 429, b, "application/json", hdr
		}
		status = 429
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(e.RetryAfter), 10)
		}
		upstream := limit != "" && limit != "rps" && limit != "rpm" && limit != "rpd"
		if upstream {
			meta.Set("provider_name", providerName)
			meta.Set("provider_code", 429)
		}
		if msg == "" {
			msg = openRouterRateLimitMessage(req, limit, upstream)
		}
	case core.ErrServerError:
		status = 500
		if msg == "" {
			msg = "Internal Server Error"
		}
	case core.ErrBadGateway:
		status = 502
		meta.Set("provider_name", providerName)
		meta.Set("raw", oai.O("error", "upstream returned an invalid response"))
		if msg == "" {
			msg = "Provider returned error"
		}
	case core.ErrOverloaded:
		status = 503
		if msg == "" {
			msg = "No available providers for this model and request configuration."
		}
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(e.RetryAfter), 10)
		}
	case core.ErrGatewayTimeout:
		status = 408
		if msg == "" {
			msg = "Request timed out"
		}
	}
	if msg == "" {
		msg = openRouterDefaultMessage(req, e, limit)
	}
	if e.Status > 0 {
		status = e.Status
	}
	et := errorTypes[e.Kind]
	if et == "" {
		et = "unmapped"
	}
	if e.Code != "" {
		et = e.Code
	}
	meta.Set("error_type", et)
	// error{message, code, metadata} and user_id (spec 006 16.7); the
	// metadata fields of these errors were not conferred.
	body := oai.O("error", oai.O("message", msg, "code", status, "metadata", meta), "user_id", "user_wwz")
	return status, body, "application/json", hdr
}

// openRouterDefaultMessage is OpenRouter's text for an error raised without
// one (engine limits, chaos): real responses always carry a message.
func openRouterDefaultMessage(req *core.Request, e *core.Error, limit string) string {
	model := req.Model
	if model == "" {
		model = "this model"
	}
	switch e.Kind {
	case core.ErrInvalidRequest:
		return "Invalid request parameters"
	case core.ErrContextLength:
		return "This endpoint's maximum context length is 128000 tokens. However, you requested about 131072 tokens. Please reduce the length of either one, or use the \"middle-out\" transform to compress your prompt automatically."
	case core.ErrQuota:
		if limit == "inflight" {
			return "Too many paid requests in flight for your current balance. Wait for them to finish or add credits, then retry."
		}
	case core.ErrPermission:
		return "Your API key does not have permission to access this resource."
	case core.ErrGeoBlocked:
		return "This model is not available in your region."
	case core.ErrAccountSuspended:
		return "Your account has been suspended."
	case core.ErrContentFilter:
		return fmt.Sprintf("%s requires moderation on WhiteWizard. Your input was flagged for \"content_policy\". No credits were charged.", model)
	case core.ErrMethodNotAllowed:
		return "Method Not Allowed"
	case core.ErrRequestTooLarge:
		return "Payload Too Large"
	}
	return "Internal Server Error"
}

// streamError is the mid-stream error chunk: the error at the top level
// plus a choice with finish_reason "error" (not conferred).
func (d *Driver) streamError(e *core.Error) any {
	_, body, _, _ := d.errorBody(&core.Request{}, e)
	var errObj any
	switch b := body.(type) {
	case oai.Obj:
		errObj, _ = b.Get("error")
	case json.RawMessage:
		var m struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(b, &m)
		errObj = m.Error
	}
	return oai.O(
		"error", errObj,
		"choices", []any{oai.O("index", 0, "delta", oai.O("content", ""), "finish_reason", "error", "native_finish_reason", "error")},
	)
}

// anthErrorBody is the Messages envelope of the gateway: error{type,
// message, error_type} and request_id, the generation id (null on 401).
func (d *Driver) anthErrorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	hdr := map[string]string{"Access-Control-Expose-Headers": exposeHeaders, "Access-Control-Allow-Origin": allowOrigin}
	if r, ok := e.Extra[extraRawReply].(rawReply); ok {
		// The gateway's own validation (tool_choice.type, task 342).
		return r.status, []byte(r.body), "application/json", hdr
	}
	if e.Extra[extraRelayed] == true {
		// A refusal of Anthropic relayed (tool-choice/, task 342).
		hdr["request-id"], hdr["x-generation-id"] = req.RequestID, req.RequestID
		return 400, json.RawMessage(relayedBody(req, e.Message)), "application/json", hdr
	}
	if invalidModel(req, e) {
		// Real /api/v1/messages: error_type inside error, request_id the
		// generation id (task 163).
		hdr["request-id"], hdr["x-generation-id"] = req.RequestID, req.RequestID
		body := `{"type":"error","error":{"type":"invalid_request_error","message":` + jsonString(invalidModelMessage(req)) +
			`,"error_type":"invalid_request"},"request_id":` + jsonString(req.RequestID) + `}`
		return 400, json.RawMessage(body), "application/json", hdr
	}
	if msg, ok := variantMiss(e); ok {
		// Conferred: messages_free_no_variant, messages_free_only_base
		// (task 186).
		hdr["request-id"], hdr["x-generation-id"] = req.RequestID, req.RequestID
		body := `{"type":"error","error":{"type":"not_found_error","message":` + jsonString(msg) +
			`,"error_type":"not_found"},"request_id":` + jsonString(req.RequestID) + `}`
		return 404, json.RawMessage(body), "application/json", hdr
	}
	status, body, ct, extra := anth.AnthropicErrorBody(req, e)
	for k, v := range extra {
		hdr[k] = v
	}
	m, ok := body.(map[string]any)
	if !ok {
		return status, body, ct, hdr
	}
	inner, _ := m["error"].(map[string]any) // AnthropicErrorBody always sets it
	if e.Kind == core.ErrQuota {
		inner = map[string]any{"type": "billing_error", "message": "Insufficient credits"}
		status = 402
	}
	inner["error_type"] = errorTypes[e.Kind]
	m["error"] = inner
	m["request_id"] = req.RequestID
	if (e.Kind == core.ErrInvalidAPIKey || e.Kind == core.ErrAccountBanned) && e.Status == 0 {
		// Conferred: badkey_messages_invalid_json and the nokey and
		// well-formed variants (task 174).
		inner["message"] = authMessage(req, e)
		m["request_id"] = nil
		return 401, m, ct, hdr
	}
	hdr["request-id"], hdr["x-generation-id"] = req.RequestID, req.RequestID
	return status, m, ct, hdr
}

func (d *Driver) GetModel(w core.ResponseWriter, _ *core.Request, m *driver.ModelInfo, id string) {
	if m == nil {
		// Checked against the real API (task 163): an {author}/{slug} that
		// names no model is "Model not found: <id>"; anything else under the
		// route (no author, extra segments) and /api/v1/models/{...} are the
		// unknown-route answer.
		// A variant suffix is named in lower case (":FREE" →
		// "Model not found: <author>/<slug>:free", task 186).
		// The unknown-route answer carries cache-control (fixture
		// routes/miss/model_unknown_sub, task 369).
		if strings.Count(id, "/") != 1 {
			writeRaw(w, 404, notFoundBody, "Cache-Control", missCacheControl)
			return
		}
		if base, suffix, ok := strings.Cut(id, ":"); ok {
			id = base + ":" + strings.ToLower(suffix)
		}
		writeRaw(w, 404, `{"error":{"message":`+jsonString("Model not found: "+id)+`,"code":404}}`)
		return
	}
	writeObj(w, 200, oai.O("data", modelJSON(*m)), "Cache-Control", oneCacheControl)
}

// modelPath is the real single-model route, GET /api/v1/model/{author}/{slug}
// (ADR 0040). /api/v1/models/{...} serves no model (ADR 0046): it answers
// like the real API answers an unknown route.
const modelPath = "/api/v1/model/"

// ModelPathBySlug reports whether path is the real single-model route, whose
// {author}/{slug} must be a full slug (any case): no short id.
func (d *Driver) ModelPathBySlug(path string) bool {
	return strings.HasPrefix(path, modelPath)
}

// slugPrefix is the author part of the slug of a model whose id has none.
const slugPrefix = "white-wizard/"

// Slug is the OpenRouter name of models[].id (ADR 0038): the id itself when
// it already reads author/model, white-wizard/<id> otherwise. It is the id
// of /models and the model field of every response.
func Slug(id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	return slugPrefix + id
}

// ModelName implements driver.ModelNamer: responses carry the slug.
func (d *Driver) ModelName(m *config.ModelConfig) string { return Slug(m.ID) }

// UnprefixModel implements driver.ModelNamer: it strips white-wizard/. The
// engine resolves the model field with config.FindModelBySlug (task 163).
func (d *Driver) UnprefixModel(name string) (string, bool) {
	return strings.CutPrefix(name, slugPrefix)
}

// writeRaw sends body as is (its member order is the real API's), with
// the CORS list of every answer and the extra header pairs.
func writeRaw(w core.ResponseWriter, status int, b string, hdr ...string) {
	w.Status(status)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	w.Header("Access-Control-Expose-Headers", exposeHeaders)
	w.Header("Access-Control-Allow-Origin", allowOrigin)
	for i := 0; i+1 < len(hdr); i += 2 {
		w.Header(hdr[i], hdr[i+1])
	}
	_, _ = w.Write([]byte(b))
	_ = w.Flush()
}

func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "account"}, TPMReserve: "max_tokens", ResetFormat: "seconds"}
}
