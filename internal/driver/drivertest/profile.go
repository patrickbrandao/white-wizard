package drivertest

import (
	"regexp"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Envelope is the shape of an error body.
type Envelope string

const (
	// EnvAny only requires a JSON object (or the HTML 502 page).
	EnvAny Envelope = ""
	// EnvOpenAI is {"error": {"message", "type", "param", "code"}}.
	EnvOpenAI Envelope = "openai"
	// EnvAnthropic is {"type": "error", "error": {"type", "message"}, "request_id"}.
	EnvAnthropic Envelope = "anthropic"
	// EnvOpenCode is the OpenCode gateway (spec 006 16.7): {"error": {"type",
	// "message"}} for upstream failures on Chat Completions, {"type":
	// "error", "error": {"type", "message"}[, "metadata"]} for its own errors
	// and on Messages, and {"model"} for a Messages body without max_tokens.
	EnvOpenCode Envelope = "opencode"
	// EnvOpenRouter is {"error": {"code": <status>, "message", "metadata": {"error_type"}}}.
	EnvOpenRouter Envelope = "openrouter"
	// EnvAzure is Azure OpenAI: {"error": {"code", "message"}} for its own
	// errors, plus type and param on what the model backend refuses.
	EnvAzure Envelope = "azure"
)

// RateLimitStyle is the family of rate-limit headers a surface sends on
// success, stream start and error (spec 006 headers row, spec 010).
type RateLimitStyle string

const (
	// RLUnchecked skips the check (no profile).
	RLUnchecked RateLimitStyle = ""
	// RLOpenAI is x-ratelimit-{limit,remaining,reset}-{requests,tokens}, Go durations.
	RLOpenAI RateLimitStyle = "openai"
	// RLAnthropic is anthropic-ratelimit-*, RFC 3339 resets, token remaining rounded to 1000.
	RLAnthropic RateLimitStyle = "anthropic"
	// RLNone forbids both families (OpenCode, OpenRouter).
	RLNone RateLimitStyle = "none"
)

// Profile is what a provider is expected to do, per spec 006.
type Profile struct {
	// Gemini is the profile of the Gemini dialect of a driver that speaks
	// several (task 283; nil = the same profile). The gemini subsuite runs
	// on the generate routes only, with this profile's Routes, Errors, IDs
	// and Gemini* flags.
	Gemini *Profile
	// Vertex (on the Gemini profile of a gateway): the generate routes
	// answer in Vertex AI's form, not the Gemini API's (vertex.go).
	Vertex bool
	// Routes the driver must declare (subset check).
	Routes []driver.Route
	// Credential presents a valid key in the provider's canonical way
	// (lower-cased header names); Key is the key it carries.
	Credential map[string]string
	Key        string
	// AltCredential is another accepted way to send Key (Anthropic Bearer).
	AltCredential map[string]string
	// RequiredHeaders must be listed in Credential.Missing when absent.
	RequiredHeaders []string
	// Foreign are other providers' canonical credentials Sniff must reject.
	Foreign []map[string]string
	// Models is the /models shape: openai, anthropic, opencode, openrouter.
	Models string
	// ModelNotFoundStatus is GetModel's status for an unknown id.
	ModelNotFoundStatus int
	// ModelNotFoundFields are JSON paths GetModel's error body must carry.
	ModelNotFoundFields map[string]any
	// ModelName is the provider's name for models[].id: the id of /models
	// and the model of every response, through driver.ModelNamer (OpenRouter
	// slug, ADR 0038). nil = the driver names models by id and must not
	// implement driver.ModelNamer.
	ModelName func(id string) string
	// Policy is the expected LimitPolicy (Entities nil = only sanity checks).
	Policy driver.LimitPolicy
	// Surfaces by inference kind (chat, completions, messages).
	Surfaces map[driver.Kind]Surface
	// Responses is the Responses API of the provider (nil = only the
	// dialect-neutral checks of responses.go).
	Responses *ResponsesProfile
	// ModelGetEmpty: GET /models/{id} answers 404 with an empty body for any
	// id, listed or not (OpenCode, conferred 2026-10-31).
	ModelGetEmpty bool
	// Billing are the decisions of a driver with driver.BillingDecider
	// (interface (d) of spec 005, ADR 0072): the billing source of a
	// request, or the refusal before any count as WriteError renders it on
	// each surface. A driver that lacks the interface fails them.
	Billing []BillingCase
	// PublicRoutes are the auxiliary routes the provider answers without a
	// credential (driver.PublicRouter); every other route needs one.
	PublicRoutes []driver.Route
	// KnownBugs: subtest path -> reason (task id), see KnownBug.
	KnownBugs map[string]string
}

// Surface is the behaviour of one inference endpoint family.
type Surface struct {
	Envelope  Envelope
	RateLimit RateLimitStyle
	// ReasoningField of the chat message/delta: reasoning_content,
	// reasoning or hidden (the text is never serialised, OpenAI's own Chat
	// Completions, ADR 0022). "" accepts any of the three.
	ReasoningField string
	// ReasoningDetails: next to the reasoning text, message and deltas carry
	// reasoning_details[] with reasoning.text entries (OpenRouter, spec 006);
	// false requires the array to be absent.
	ReasoningDetails bool
	// KeepAlive is the SSE comment sent before the first chunk.
	KeepAlive string
	// UsageInFinish: usage rides on the closing chunk (empty delta and
	// finish_reason), with or without include_usage (OpenCode, spec 006 16.7).
	UsageInFinish bool
	// ReasoningByDefault: a request that names no reasoning field asks for
	// the model's default reasoning (OpenCode upstreams, conferred).
	ReasoningByDefault bool
	// NoSignature: thinking blocks carry no signature (OpenCode Messages).
	NoSignature bool
	// UsageAlways: the usage chunk closes every stream, with or without
	// stream_options.include_usage (OpenRouter, spec 006 16.7).
	UsageAlways bool
	// ReasoningNull: without reasoning the message carries reasoning: null
	// (OpenRouter) instead of no field.
	ReasoningNull bool
	// RequestIDFormat is the form of request_id in the Anthropic envelope
	// (nil = "req_…"); OpenRouter's Messages route uses the generation id.
	RequestIDFormat *regexp.Regexp
	// AuthRequestIDNull: the 401 has request_id null and no request-id
	// header (OpenRouter Messages).
	AuthRequestIDNull bool
	// Accepted are cases of testdata/invalid.json the real provider accepts
	// (case name -> where it was conferred).
	Accepted map[string]string
	// FinalChunkWithChoice: the usage chunk carries one empty-delta choice
	// repeating finish_reason (OpenRouter) instead of choices: [].
	FinalChunkWithChoice bool
	// NativeFinishReason: choices carry native_finish_reason (OpenRouter).
	NativeFinishReason bool
	// RefusalDetails: a refusal carries stop_details {type, category,
	// explanation} with the last two null (Anthropic).
	RefusalDetails bool
	// UsageCost: usage.cost is present (OpenRouter, OpenCode).
	UsageCost bool
	// ReasoningObject: the chat body accepts reasoning {effort, exclude}.
	ReasoningObject bool
	// StreamErrorFinishReason: the mid-stream error chunk carries
	// choices[0].finish_reason "error" (OpenRouter).
	StreamErrorFinishReason bool
	// RequestIDHeader: responses carry request-id equal to the body's
	// request_id (Anthropic dialect).
	RequestIDHeader bool
	// RateLimitAlways: the limit headers go out even without limiter state,
	// with the default tier's values (OpenAI, ADR 0049).
	RateLimitAlways bool
	// SuccessHeaders must be present on every 200, body and stream start
	// (OpenAI's account and version headers, spec 006 16.7).
	SuccessHeaders []string
	// ServiceTier is the service_tier of chat bodies and chunks ("" = unchecked).
	ServiceTier string
	// Annotations: the chat message carries annotations: [] (OpenAI).
	Annotations bool
	// ChunkUsageNull: with include_usage every chunk before the usage chunk
	// carries usage: null (OpenAI).
	ChunkUsageNull bool
	// Obfuscation: every chunk carries a non-empty obfuscation string
	// (OpenAI); false requires it absent.
	Obfuscation bool
	// AuthErrorText: the 401 of an invalid key is text/plain, with status
	// in the body and the same body base64 in x-error-json (OpenAI).
	AuthErrorText bool
	// ContentFilter: Azure's content filter annotations: the stream opens
	// with a prompt_filter_results event, the body carries
	// prompt_filter_results and every choice content_filter_results.
	ContentFilter bool
	// Errors are provider-specific WriteError expectations.
	Errors []ErrorCase
	// RetryAfter are exact retry-after expectations (seconds).
	RetryAfter []RetryAfterCase
	// IDs are the driver.IDMinter formats for this kind (nil = unchecked).
	IDs *IDFormat
}

// ErrorCase is one WriteError expectation.
type ErrorCase struct {
	Name string
	// Err is copied before each call (drivers may mutate Extra).
	Err    core.Error
	Status int
	// Fields: dotted JSON path -> expected value (string, int, bool, nil).
	Fields map[string]any
	// Headers that must be present: name -> value ("*" = any).
	Headers map[string]string
	// NoHeaders must be absent.
	NoHeaders []string
	// WithRateLimit passes a populated RateLimitState.
	WithRateLimit bool
	// Catalog, Account and ReqHeaders shape the request WriteError sees:
	// the catalog the engine resolved for the route (driver.SetCatalog),
	// the account (driver.SetAccount) and plain request headers.
	Catalog    string
	Account    driver.Account
	ReqHeaders map[string]string
	// Body, when set, is the whole error body (compared as JSON).
	Body string
	// Absent are dotted JSON paths that must not be in the body.
	Absent []string
	// Patterns: dotted JSON path -> regular expression its string matches.
	Patterns map[string]string
	// RetryAfterMidnight: retry-after is the whole seconds to the next
	// 00:00 UTC (the daily reset of OpenCode's free models).
	RetryAfterMidnight bool
	// NextMonth are dotted JSON paths whose RFC 3339 instant is the 1st of
	// the next month, 00:00 UTC (the reset of OpenCode's budgets).
	NextMonth []string
}

// BillingCase is one decision of a driver.BillingDecider, asked on the
// route of each surface under Base with the request headers Headers (the
// credential is what Authenticate extracts from them).
type BillingCase struct {
	Name string
	// Base is the path prefix of the route (relative to the mount
	// prefix); the suite picks the surface's route under it and its
	// catalog (driver.RouteCatalog).
	Base    string
	Headers map[string]string
	// Key is the keys[] entry of the credential (nil = none), Account its
	// accounts[] entry, Plan the tier's plan ("" = the driver's default).
	Key     *config.KeyConfig
	Account *config.AccountConfig
	Plan    string
	// Model is the effective model of the request (its pricing must be
	// declared to be free, see config.Pricing.Declare).
	Model config.ModelConfig
	// Want is the decision when the request is accepted (Status 0).
	Want driver.Billing
	// Status, Bodies and RetryAfterMidnight are the refusal: its status,
	// its whole body per surface (compared as JSON) and its retry-after.
	Status             int
	Bodies             map[driver.Kind]string
	RetryAfterMidnight bool
}

// RetryAfterCase pins the retry-after value for an error.
type RetryAfterCase struct {
	Name string
	Err  core.Error
	Want string
}

// IDFormat holds the expected id shapes.
type IDFormat struct {
	Response *regexp.Regexp
	ToolCall *regexp.Regexp
	Request  *regexp.Regexp
}

// Canonical credentials of the four providers (spec 006).
var (
	credOpenAI     = map[string]string{"authorization": "Bearer sk-proj-wwztest"}
	credAnthropic  = map[string]string{"x-api-key": "sk-ant-api03-wwztest", "anthropic-version": "2023-06-01"}
	credOpenCode   = map[string]string{"authorization": "Bearer oc_sk_wwztest"}
	credOpenRouter = map[string]string{"authorization": "Bearer sk-or-v1-wwztest"}
)

// Id shapes measured on real responses (spec 090, task 010).
var (
	reChatcmpl   = regexp.MustCompile(`^chatcmpl-[0-9A-Za-z]{29}$`)
	reCmpl       = regexp.MustCompile(`^cmpl-[0-9A-Za-z]{29}$`)
	reCall       = regexp.MustCompile(`^call_[0-9A-Za-z]{24}$`)
	reReqHex     = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	reMsg        = regexp.MustCompile(`^msg_01[0-9A-Za-z]{22}$`)
	reToolu      = regexp.MustCompile(`^toolu_01[0-9A-Za-z]{22}$`)
	reReqAnth    = regexp.MustCompile(`^req_01[0-9A-Za-z]{22}$`)
	reGen        = regexp.MustCompile(`^gen-[0-9]{10}-[0-9A-Za-z]{20}$`)
	idsOpenAI    = &IDFormat{Response: reChatcmpl, ToolCall: reCall, Request: reReqHex}
	idsAnthropic = &IDFormat{Response: reMsg, ToolCall: reToolu, Request: reReqAnth}
)

func routes(method, kind string, paths ...string) []driver.Route {
	out := make([]driver.Route, 0, len(paths))
	for _, p := range paths {
		out = append(out, driver.Route{Method: method, Path: p, Kind: driver.Kind(kind)})
	}
	return out
}

func concat(rs ...[]driver.Route) []driver.Route {
	var out []driver.Route
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}

func lim(l string) map[string]any { return map[string]any{"limit": l} }

// ProfileFor returns the built-in profile of a driver name (openai,
// anthropic, opencode, openrouter, gemini). Each call returns a fresh copy.
func ProfileFor(name string) (Profile, bool) {
	switch name {
	case "openai":
		return openAIProfile(), true
	case "anthropic":
		return anthropicProfile(), true
	case "opencode":
		return openCodeProfile(), true
	case "openrouter":
		return openRouterProfile(), true
	case "gemini":
		return geminiProfile(), true
	case "azure":
		return azureProfile(), true
	}
	return Profile{}, false
}

// --- OpenAI (spec 006 §16.1) ---

func openAIErrors() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad", Param: "messages"}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error", "error.param": "messages", "error.message": "bad"}},
		{Name: "context_length", Err: core.Error{Kind: core.ErrContextLength, Message: "too long"}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error", "error.code": "context_length_exceeded"}},
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.type": "invalid_request_error", "error.code": "invalid_api_key"}},
		{Name: "geo_blocked", Err: core.Error{Kind: core.ErrGeoBlocked}, Status: 403, Fields: map[string]any{"error.type": "request_forbidden", "error.code": "unsupported_country_region_territory"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission, Message: "no"}, Status: 403},
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 404, Fields: map[string]any{"error.type": "invalid_request_error", "error.code": "model_not_found"}},
		{Name: "request_too_large", Err: core.Error{Kind: core.ErrRequestTooLarge, Message: "big"}, Status: 413, Fields: map[string]any{"error.type": "invalid_request_error"}},
		{Name: "rate_limit_rpm", Err: core.Error{Kind: core.ErrRateLimit, Message: "slow", RetryAfter: 2 * time.Second, Extra: lim("rpm")}, Status: 429, Fields: map[string]any{"error.type": "requests", "error.code": "rate_limit_exceeded"}, Headers: map[string]string{"retry-after": "*", "retry-after-ms": "2000", "x-ratelimit-limit-requests": "100"}, WithRateLimit: true},
		{Name: "rate_limit_tpm", Err: core.Error{Kind: core.ErrRateLimit, Message: "slow", RetryAfter: 2 * time.Second, Extra: lim("tpm")}, Status: 429, Fields: map[string]any{"error.type": "tokens", "error.code": "rate_limit_exceeded"}, Headers: map[string]string{"retry-after": "*"}},
		{Name: "slow_down", Err: core.Error{Kind: core.ErrSlowDown, Message: "ramp", RetryAfter: 2 * time.Second}, Status: 429, Fields: map[string]any{"error.type": "rate_limit_error", "error.code": "slow_down"}},
		// Quota/billing: permanent until a human acts, no Retry-After (spec 006).
		{Name: "quota", Err: core.Error{Kind: core.ErrQuota, RetryAfter: 2 * time.Second}, Status: 429, Fields: map[string]any{"error.type": "insufficient_quota", "error.code": "insufficient_quota"}, NoHeaders: []string{"retry-after"}},
		{Name: "quota_credits", Err: core.Error{Kind: core.ErrQuota, Extra: lim("credits")}, Status: 429, Fields: map[string]any{"error.type": "insufficient_quota", "error.code": "credit_balance_exhausted"}},
		{Name: "spend_limit", Err: core.Error{Kind: core.ErrSpendLimit, Message: "cap"}, Status: 429, Fields: map[string]any{"error.type": "insufficient_quota", "error.code": "organization_spend_limit_exceeded"}},
		{Name: "budget", Err: core.Error{Kind: core.ErrBudget, Message: "cap"}, Status: 429, Fields: map[string]any{"error.type": "insufficient_quota", "error.code": "project_spend_limit_exceeded"}},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.type": "server_error", "error.code": nil}},
		{Name: "bad_gateway", Err: core.Error{Kind: core.ErrBadGateway}, Status: 502},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Status: 503, Fields: map[string]any{"error.type": "service_unavailable_error", "error.code": "server_is_overloaded"}, Headers: map[string]string{"retry-after": "*"}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 504},
	}
}

func openAIRetryAfter() []RetryAfterCase {
	return []RetryAfterCase{
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Want: "2"},
		{Name: "rate_limit_fraction_rounds_up", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond}, Want: "2"},
		{Name: "slow_down", Err: core.Error{Kind: core.ErrSlowDown, RetryAfter: 5 * time.Second}, Want: "5"},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Want: "3"},
	}
}

func openAIProfile() Profile {
	chat := Surface{
		// ADR 0022: OpenAI hides the reasoning text; only usage counts it.
		Envelope: EnvOpenAI, RateLimit: RLOpenAI, ReasoningField: "hidden",
		Errors: openAIErrors(), RetryAfter: openAIRetryAfter(), IDs: idsOpenAI,
		// Spec 006 16.7: headers on every success, the chat and chunk
		// fields of the real, the 401 in text/plain.
		RateLimitAlways: true, SuccessHeaders: []string{"openai-organization", "openai-project", "openai-processing-ms", "openai-version"},
		ServiceTier: "default", Annotations: true, ChunkUsageNull: true, Obfuscation: true, AuthErrorText: true,
	}
	cmpl := chat
	cmpl.IDs = &IDFormat{Response: reCmpl, ToolCall: reCall, Request: reReqHex}
	cmpl.Annotations = false // text_completion has no message
	return Profile{
		Routes: concat(
			// The stored completions listing (task 315): a resource route,
			// listed so a wrong method on it fails (task 449).
			routes("GET", "resource", "/v1/chat/completions"),
			routes("POST", "chat", "/v1/chat/completions"),
			routes("POST", "completions", "/v1/completions"),
			routes("GET", "models", "/v1/models"),
			routes("GET", "model", "/v1/models/*"),
		),
		Credential: credOpenAI, Key: "sk-proj-wwztest",
		Foreign:             []map[string]string{credAnthropic, credOpenCode, credOpenRouter},
		Models:              "openai",
		ModelNotFoundStatus: 404,
		Policy:              driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "max_tokens", ResetFormat: "go_duration"},
		Surfaces:            map[driver.Kind]Surface{driver.KindChat: chat, driver.KindCompletions: cmpl},
		Responses:           &ResponsesProfile{EventLines: true, ID: reResp50},
	}
}

// --- Anthropic (spec 006 §16.2) ---

func anthropicErrors() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad"}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error", "error.message": "bad"}},
		{Name: "context_length", Err: core.Error{Kind: core.ErrContextLength}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error"}},
		// Customer spend limit: 400, not 429 (spec 006).
		{Name: "spend_limit", Err: core.Error{Kind: core.ErrSpendLimit}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error"}},
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.type": "authentication_error"}},
		{Name: "account_banned", Err: core.Error{Kind: core.ErrAccountBanned}, Status: 401, Fields: map[string]any{"error.type": "authentication_error"}},
		{Name: "quota_credits", Err: core.Error{Kind: core.ErrQuota, Extra: lim("credits")}, Status: 402, Fields: map[string]any{"error.type": "billing_error"}},
		// Tier spend cap: 429 rate_limit_error without retry-after, details.error_code.
		{Name: "quota_spend_cap", Err: core.Error{Kind: core.ErrQuota, RetryAfter: 2 * time.Second}, Status: 429, Fields: map[string]any{"error.type": "rate_limit_error", "error.details.error_code": "enforced_spend_limit_reached"}, NoHeaders: []string{"retry-after"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission, Message: "no"}, Status: 403, Fields: map[string]any{"error.type": "permission_error"}},
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 404, Fields: map[string]any{"error.type": "not_found_error"}},
		{Name: "request_too_large", Err: core.Error{Kind: core.ErrRequestTooLarge, Message: "big"}, Status: 413, Fields: map[string]any{"error.type": "request_too_large"}},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second, Extra: lim("rpm")}, Status: 429, Fields: map[string]any{"error.type": "rate_limit_error"}, Headers: map[string]string{"retry-after": "*", "anthropic-ratelimit-requests-limit": "100"}, WithRateLimit: true},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.type": "api_error"}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 504, Fields: map[string]any{"error.type": "timeout_error"}},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Status: 529, Fields: map[string]any{"error.type": "overloaded_error"}, Headers: map[string]string{"retry-after": "*"}},
	}
}

func anthropicRetryAfter() []RetryAfterCase {
	return []RetryAfterCase{
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Want: "2"},
		{Name: "rate_limit_fraction_rounds_up", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond}, Want: "2"},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Want: "3"},
	}
}

func anthropicProfile() Profile {
	// /api/hello, Claude Code's probe (ADR 0074): 200 on every method,
	// with any key or none (conferred on 2026-11-03, task 449).
	var hello []driver.Route
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		hello = concat(hello, routes(m, "resource", "/api/hello"))
	}
	return Profile{
		Routes: concat(
			routes("POST", "messages", "/v1/messages"),
			routes("POST", "count_tokens", "/v1/messages/count_tokens"),
			routes("GET", "models", "/v1/models"),
			routes("GET", "model", "/v1/models/*"),
			hello,
		),
		PublicRoutes: hello,
		Credential:   credAnthropic, Key: "sk-ant-api03-wwztest",
		AltCredential:       map[string]string{"authorization": "Bearer sk-ant-api03-wwztest", "anthropic-version": "2023-06-01"},
		RequiredHeaders:     []string{"anthropic-version"},
		Foreign:             []map[string]string{credOpenAI, credOpenCode, credOpenRouter},
		Models:              "anthropic",
		ModelNotFoundStatus: 404,
		Policy:              driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "none", ExcludeCacheRead: true, ResetFormat: "rfc3339"},
		Surfaces: map[driver.Kind]Surface{driver.KindMessages: {
			Envelope: EnvAnthropic, RateLimit: RLAnthropic, RequestIDHeader: true,
			// Spec 006 16.7: the 12 anthropic-ratelimit-* and the account on
			// every Messages 200, even without limiter state (ADR 0049).
			RefusalDetails: true, RateLimitAlways: true, SuccessHeaders: []string{"anthropic-organization-id", "anthropic-workspace-id"},
			Errors: anthropicErrors(), RetryAfter: anthropicRetryAfter(), IDs: idsAnthropic,
		}},
	}
}

// --- OpenCode Go (spec 006 §16.3) ---

// openCodeErrors are the cases of a surface; upstream is the type of the
// upstream failures (server_error on Chat Completions, api_error on Messages).
func openCodeErrors(upstream string) []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad"}, Status: 400},
		// Conferred 2026-10-31 (task 175): 401 by key, 400 unknown model.
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.type": upstream, "error.message": "Upstream request failed: Go requires a credential"}, Headers: map[string]string{"x-opencode-log-id": "*"}},
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 400, Fields: map[string]any{"error.type": upstream, "error.message": "Upstream request failed: Model is unavailable."}, Headers: map[string]string{"x-opencode-log-id": "*"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission, Message: "no"}, Status: 403},
		{Name: "missing_session", Err: core.Error{Kind: core.ErrMissingSession}, Status: 400, Fields: map[string]any{"type": "error", "error.type": "MissingSessionID"}},
		{Name: "quota_5h", Err: core.Error{Kind: core.ErrQuota, RetryAfter: time.Hour, Extra: lim("quota_5h")}, Status: 429, Fields: map[string]any{"error.type": "GoUsageLimitError", "metadata.limitName": "5 hour"}, Patterns: map[string]string{"metadata.workspace": reWorkspace}, Headers: map[string]string{"retry-after": "*"}},
		{Name: "quota_weekly", Err: core.Error{Kind: core.ErrQuota, RetryAfter: time.Hour, Extra: lim("quota_weekly")}, Status: 429, Fields: map[string]any{"error.type": "GoUsageLimitError", "metadata.limitName": "weekly"}, Patterns: map[string]string{"metadata.workspace": reWorkspace}},
		{Name: "quota_monthly", Err: core.Error{Kind: core.ErrQuota, RetryAfter: time.Hour, Extra: lim("quota_monthly")}, Status: 429, Fields: map[string]any{"error.type": "GoUsageLimitError", "metadata.limitName": "monthly"}, Patterns: map[string]string{"metadata.workspace": reWorkspace}},
		{Name: "quota_credits", Err: core.Error{Kind: core.ErrQuota, Extra: lim("credits")}, Status: 402, Fields: map[string]any{"error.type": upstream, "error.message": "Upstream request failed: Insufficient account funds"}},
		{Name: "quota_credit_limit", Err: core.Error{Kind: core.ErrQuota}, Status: 402, Fields: map[string]any{"error.type": "CreditLimitExceeded"}},
		{Name: "budget", Err: core.Error{Kind: core.ErrBudget, RetryAfter: 2 * time.Second}, Status: 429, Fields: map[string]any{"error.type": "BudgetLimitError", "metadata.coverage": "models"}, NoHeaders: []string{"retry-after"}, NextMonth: []string{"metadata.resetsAt"}},
		// ADR 0072, item 14: the coverage follows the budget that denied
		// (spend_monthly_usd is all, spend_monthly_models_usd models);
		// resetsAt is the 1st of the next month, 00:00 UTC (not conferred).
		{Name: "budget_all", Err: core.Error{Kind: core.ErrBudget, Extra: map[string]any{"limit": "spend_monthly", "entity_kind": "account"}}, Status: 429, Fields: map[string]any{"type": "error", "error.type": "BudgetLimitError", "error.message": "A budget policy for this model has been reached.", "metadata.coverage": "all"}, NoHeaders: []string{"retry-after"}, NextMonth: []string{"metadata.resetsAt"}},
		{Name: "budget_models", Err: core.Error{Kind: core.ErrBudget, Extra: map[string]any{"limit": "spend_monthly", "entity_kind": "model_spend"}}, Status: 429, Fields: map[string]any{"type": "error", "error.type": "BudgetLimitError", "metadata.coverage": "models"}, NoHeaders: []string{"retry-after"}, NextMonth: []string{"metadata.resetsAt"}},
		// ADR 0072, item 13: a model quota (model_quota) denies with the
		// account's GoUsageLimitError and its workspace (format not
		// conferred).
		{Name: "quota_model", Err: core.Error{Kind: core.ErrQuota, RetryAfter: time.Hour, Extra: map[string]any{"limit": "quota_monthly", "entity_kind": "model_quota"}}, Account: driver.Account{Key: "oc_sk_wwztest", Account: "acme"}, Status: 429, Fields: map[string]any{"type": "error", "error.type": "GoUsageLimitError", "error.message": "Go usage limit exceeded", "metadata.limitName": "monthly"}, Patterns: map[string]string{"metadata.workspace": reWorkspace}, Headers: map[string]string{"retry-after": "3600", "x-opencode-log-id": "*"}},
		// ADR 0072, item 6: the daily count of the free models, in the
		// gateway's envelope without metadata on both routes, retry-after to
		// 00:00 UTC (conferred, free/public_generic_client.json and
		// public_messages.json).
		{Name: "free_usage", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second, Extra: map[string]any{"limit": "rpd", "entity_kind": "free_models", "free_models": true}}, Status: 429, Body: freeUsageBody, Absent: []string{"metadata"}, RetryAfterMidnight: true, Headers: map[string]string{"x-opencode-log-id": "*"}},
		// ADR 0072, item 4: a missing credential on the pay-as-you-go bases
		// is the gateway's AuthError (conferred with public,
		// free/public_paid_model.json); on the Go bases the upstream 401 of
		// Go (conferred, free/public_go_base.json).
		{Name: "missing_key_payg", Err: core.Error{Kind: core.ErrInvalidAPIKey, Message: noKeyText}, Catalog: "opencode", Status: 401, Body: missingKeyBody, Headers: map[string]string{"x-opencode-log-id": "*"}},
		{Name: "missing_key_go", Err: core.Error{Kind: core.ErrInvalidAPIKey, Message: noKeyText}, Catalog: "opencode-go", Status: 401, Body: openCodeUpstreamBody(upstream, "Go requires a credential"), Headers: map[string]string{"x-opencode-log-id": "*"}},
		// ADR 0072, item 9: a block (account_suspended without the
		// operator's text: the abuse ladder, keys[].status) is the
		// workspace's suspension (not conferred).
		{Name: "blocked_workspace", Err: core.Error{Kind: core.ErrAccountSuspended, RetryAfter: time.Hour, Extra: lim("block")}, Status: 403, Body: openCodeSuspendedBody(upstream), NoHeaders: []string{"retry-after", "retry-after-ms"}},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit}, Status: 429, Fields: map[string]any{"error.type": upstream, "error.message": "Upstream request failed: Rate limit exceeded."}, Headers: map[string]string{"retry-after": "*"}, WithRateLimit: true},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.type": upstream}},
		{Name: "bad_gateway", Err: core.Error{Kind: core.ErrBadGateway}, Status: 502, Fields: map[string]any{"error.message": "Upstream request failed: [502] Bad Gateway"}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 504},
	}
}

func openCodeRetryAfter() []RetryAfterCase {
	return []RetryAfterCase{
		// Spec 006: 429 "Rate limit exceeded." with retry-after: 45.
		{Name: "rate_limit_default", Err: core.Error{Kind: core.ErrRateLimit}, Want: "45"},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Want: "2"},
		{Name: "quota_5h", Err: core.Error{Kind: core.ErrQuota, RetryAfter: 3600 * time.Second, Extra: lim("quota_5h")}, Want: "3600"},
	}
}

// Texts and bodies of OpenCode's own refusals (ADR 0072; spec 006 16.3).
const (
	// noKeyText is the engine's message for a request without a key.
	noKeyText = "You didn't provide an API key."
	// reWorkspace is metadata.workspace: wrk_ and 26 Crockford base32
	// characters (not conferred).
	reWorkspace = `^wrk_[0-9A-HJKMNP-TV-Z]{26}$`
	// freeUsageBody is the 429 of the free models (conferred,
	// free/public_generic_client.json).
	freeUsageBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`
	// missingKeyBody is the 401 of a missing credential on the
	// pay-as-you-go bases (conferred, free/public_paid_model.json).
	missingKeyBody = `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`
	// noGoBody is the 403 of a workspace without Go (not conferred).
	noGoBody = `{"type":"error","error":{"type":"ForbiddenError","message":"This workspace does not have an active OpenCode Go subscription"}}`
)

// openCodeUpstreamBody is an upstream failure of the route whose upstream
// type is upstream: bare on Chat Completions, with type error on Messages.
func openCodeUpstreamBody(upstream, msg string) string {
	if upstream == "api_error" {
		return `{"type":"error","error":{"type":"api_error","message":"Upstream request failed: ` + msg + `"}}`
	}
	return `{"error":{"type":"server_error","message":"Upstream request failed: ` + msg + `"}}`
}

// openCodeSuspendedBody is the block of a workspace on the route of
// upstream (ADR 0072, item 9; not conferred).
func openCodeSuspendedBody(upstream string) string {
	if upstream == "api_error" {
		return `{"type":"error","error":{"type":"permission_error","message":"This workspace has been suspended"}}`
	}
	return `{"error":{"type":"permission_error","message":"This workspace has been suspended"}}`
}

// openCodeBodies is the same body on both routes.
func openCodeBodies(body string) map[driver.Kind]string {
	return map[driver.Kind]string{driver.KindChat: body, driver.KindMessages: body}
}

// openCodeModel is a model of the billing cases: free declares a pricing
// with input_per_m 0 (ADR 0072, item 3), paid one with a price.
func openCodeModel(id string, free bool) config.ModelConfig {
	p := config.Pricing{InputPerM: 0.3, OutputPerM: 1.2}
	if free {
		p = config.Pricing{}.Declare()
	}
	return config.ModelConfig{ID: id, Pricing: p}
}

// openCodeBilling are the decisions of interface (d) (ADR 0072, items 3–12;
// spec 006 16.3; spec 010 "Origem da cobrança no opencode"), in the order
// credential → client of the driver.
func openCodeBilling() []BillingCase {
	const ua = "opencode/latest/2.2.5/wwz-drivertest" // only ever sent to the wwz
	public := map[string]string{"authorization": "Bearer public", "x-opencode-session": "ses_wwztest"}
	publicOC := map[string]string{"authorization": "Bearer public", "x-opencode-session": "ses_wwztest", "user-agent": ua}
	keyOC := map[string]string{"authorization": "Bearer oc_sk_wwztest", "x-opencode-session": "ses_wwztest", "user-agent": ua}
	key := &config.KeyConfig{Key: "oc_sk_wwztest", Label: "wwztest", Account: "acme"}
	svc := &config.KeyConfig{Key: "oc_sk_wwztest", Label: "wwztest", Account: "acme", ServiceAccount: true}
	on, off := true, false
	free, paid := openCodeModel("big-pickle", true), openCodeModel("deepseek-v4.1-flash", false)
	return []BillingCase{
		// Refusals (conferred with curl/8.7.1 and public, fixtures free/).
		{Name: "public_paid_payg", Base: "/inference", Headers: public, Model: paid, Status: 401, Bodies: openCodeBodies(missingKeyBody)},
		{Name: "public_paid_zen", Base: "/zen", Headers: public, Model: paid, Status: 401, Bodies: openCodeBodies(missingKeyBody)},
		{Name: "public_go_base", Base: "/inference/go", Headers: publicOC, Model: free, Status: 401, Bodies: map[driver.Kind]string{
			driver.KindChat: openCodeUpstreamBody("server_error", "Go requires a credential"), driver.KindMessages: openCodeUpstreamBody("api_error", "Go requires a credential"),
		}},
		{Name: "free_generic_client", Base: "/inference", Headers: public, Model: free, Status: 429, Bodies: openCodeBodies(freeUsageBody), RetryAfterMidnight: true},
		{Name: "free_generic_client_key", Base: "/inference/go", Headers: map[string]string{"authorization": "Bearer oc_sk_wwztest", "x-opencode-session": "ses_wwztest", "user-agent": "curl/8.7.1"}, Key: key, Model: free, Status: 429, Bodies: openCodeBodies(freeUsageBody), RetryAfterMidnight: true},
		{Name: "plan_none_go", Base: "/inference/go", Headers: keyOC, Key: key, Plan: "none", Model: paid, Status: 403, Bodies: openCodeBodies(noGoBody)},
		// Sources (ADR 0072, item 7): free with the daily count only on
		// the pay-as-you-go bases (default 200), balance there, go or
		// go-plus on the Go bases at 0.6 of the list (item 8), balance for
		// a service account (item 10), the member's quota and Extra Usage
		// in Go Teams (items 11 and 12).
		{Name: "free_payg", Base: "/inference", Headers: publicOC, Model: free, Want: driver.Billing{Source: driver.BillingFree, FreeDailyRequests: 200}},
		{Name: "free_go", Base: "/zen/go", Headers: keyOC, Key: key, Model: openCodeModel("beam-free", true), Want: driver.Billing{Source: driver.BillingFree}},
		{Name: "balance_payg", Base: "/zen", Headers: keyOC, Key: key, Model: paid, Want: driver.Billing{Source: driver.BillingBalance}},
		{Name: "go", Base: "/inference/go", Headers: keyOC, Key: key, Model: paid, Want: driver.Billing{Source: driver.BillingGo, AllowanceFactor: 0.6}},
		{Name: "go_plus", Base: "/zen/go", Headers: keyOC, Key: key, Plan: "go-plus", Model: paid, Want: driver.Billing{Source: driver.BillingGoPlus, AllowanceFactor: 0.6}},
		{Name: "go_gemini", Base: "/inference/go", Headers: keyOC, Key: key, Model: openCodeModel("gemini-4-argon", false), Want: driver.Billing{Source: driver.BillingGo, AllowanceFactor: 2}},
		{Name: "go_teams", Base: "/inference/go", Headers: keyOC, Key: key, Plan: "go-teams", Model: paid, Want: driver.Billing{Source: driver.BillingGo, AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember, ExtraUsage: true}},
		{Name: "go_teams_no_extra", Base: "/inference/go", Headers: keyOC, Key: key, Plan: "go-teams", Account: &config.AccountConfig{ID: "acme", ExtraUsage: &off}, Model: paid, Want: driver.Billing{Source: driver.BillingGo, AllowanceFactor: 0.6, QuotaOwner: driver.OwnerMember}},
		{Name: "go_extra_usage", Base: "/inference/go", Headers: keyOC, Key: key, Account: &config.AccountConfig{ID: "acme", ExtraUsage: &on}, Model: paid, Want: driver.Billing{Source: driver.BillingGo, AllowanceFactor: 0.6, ExtraUsage: true}},
		{Name: "service_account_go", Base: "/inference/go", Headers: keyOC, Key: svc, Model: paid, Want: driver.Billing{Source: driver.BillingBalance}},
	}
}

// OpenCode ids (conferred 2026-10-31, task 175).
var (
	reGw         = regexp.MustCompile(`^gw-[0-9a-f]{12}-[0-9a-f]{6}$`)
	reCallHex    = regexp.MustCompile(`^call_[0-9a-f]{24}$`)
	reHex32      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	idsOpenCode  = &IDFormat{Response: reGw, ToolCall: reCallHex, Request: reReqHex}
	idsOpenCodeM = &IDFormat{Response: reHex32, ToolCall: reToolu, Request: reReqAnth}
)

func openCodeProfile() Profile {
	base := Surface{
		Envelope: EnvOpenCode, RateLimit: RLNone, UsageCost: true, RetryAfter: openCodeRetryAfter(),
		ReasoningByDefault: true, SuccessHeaders: []string{"x-opencode-log-id", "x-zen-model"},
	}
	chat := base
	chat.ReasoningField = "reasoning_content"
	chat.UsageInFinish = true
	chat.Errors = openCodeErrors("server_error")
	chat.IDs = idsOpenCode
	msgs := base
	msgs.NoSignature = true
	msgs.Errors = openCodeErrors("api_error")
	msgs.IDs = idsOpenCodeM // no request-id header: OpenCode sends none (spec 006 16.7)
	var rs []driver.Route
	for _, b := range []string{"/inference", "/inference/go", "/zen", "/zen/go"} {
		rs = concat(rs,
			routes("POST", "chat", b+"/openai/v1/chat/completions", b+"/v1/chat/completions"),
			routes("POST", "messages", b+"/anthropic/v1/messages", b+"/v1/messages"),
			routes("GET", "models", b+"/v1/models"),
			routes("GET", "usage", b+"/v1/usage"),
		)
	}
	// The Console (ADR 0072, item 17; spec 004): the device flow answers
	// without a credential, api/* needs one.
	console := routes("POST", "usage", "/console/auth/device/code", "/console/auth/device/token")
	rs = concat(rs, console, routes("GET", "usage", "/console/api/user", "/console/api/orgs", "/console/api/v2/config", "/console/api/policies/current-budgets"))
	return Profile{
		Billing:      openCodeBilling(),
		PublicRoutes: console,
		Routes:       rs,
		Credential:   map[string]string{"authorization": "Bearer oc_sk_wwztest", "x-opencode-session": "ses_wwztest"}, Key: "oc_sk_wwztest",
		RequiredHeaders:     []string{"x-opencode-session"},
		Foreign:             []map[string]string{credOpenAI, credAnthropic, credOpenRouter},
		Models:              "opencode",
		ModelNotFoundStatus: 404,
		ModelGetEmpty:       true,
		Policy:              driver.LimitPolicy{Entities: []string{"ip", "key", "user", "project", "account"}, TPMReserve: "none", ResetFormat: "seconds"},
		Surfaces:            map[driver.Kind]Surface{driver.KindChat: chat, driver.KindMessages: msgs},
		Responses:           &ResponsesProfile{EventLines: true, Stateless: true, ID: reResp32},
		Gemini:              openCodeGemini(),
	}
}

// openCodeGemini is the profile of the /google routes of OpenCode Go.
func openCodeGemini() *Profile {
	var rs []driver.Route
	for _, b := range []string{"/inference", "/inference/go"} {
		rs = concat(rs, routes("POST", "generate", b+"/google/v1beta/models/*", b+"/google/v1/models/*"))
	}
	return &Profile{
		Routes: rs, Vertex: true,
		Credential: map[string]string{"x-goog-api-key": "oc_sk_wwztest", "x-opencode-session": "ses_wwztest"}, Key: "oc_sk_wwztest",
		Models:   "opencode",
		Policy:   driver.LimitPolicy{Entities: []string{"ip", "key", "user", "project", "account"}, TPMReserve: "none", ResetFormat: "seconds"},
		Surfaces: map[driver.Kind]Surface{driver.KindGenerate: {RateLimit: RLNone, IDs: idsGemini}},
	}
}

// --- OpenRouter (spec 006 §16.4) ---

func openRouterErrors() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad"}, Status: 400, Fields: map[string]any{"error.metadata.error_type": "invalid_request", "error.message": "bad"}},
		{Name: "context_length", Err: core.Error{Kind: core.ErrContextLength, Message: "long"}, Status: 400, Fields: map[string]any{"error.metadata.error_type": "context_length_exceeded"}},
		// 401: {error: {message, code}} by the key presented (none here), no
		// generation id (spec 006 16.7).
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.message": "No cookie auth credentials found", "error.code": 401}, NoHeaders: []string{"x-generation-id"}},
		{Name: "quota_credits", Err: core.Error{Kind: core.ErrQuota, Extra: lim("credits")}, Status: 402, Fields: map[string]any{"error.metadata.error_type": "payment_required", "error.metadata.limit_source": "openrouter_credits"}},
		{Name: "quota_inflight", Err: core.Error{Kind: core.ErrQuota, Extra: lim("inflight")}, Status: 402, Fields: map[string]any{"error.metadata.limit_source": "openrouter_in_flight_budget", "error.metadata.reason": "in_flight_budget_exhausted"}, Headers: map[string]string{"retry-after": "*"}},
		{Name: "quota_key_limit", Err: core.Error{Kind: core.ErrQuota}, Status: 402, Fields: map[string]any{"error.metadata.limit_source": "openrouter_key_limit"}},
		// Budgets/guardrails: 403 without Retry-After (spec 006).
		{Name: "budget", Err: core.Error{Kind: core.ErrBudget, RetryAfter: 2 * time.Second}, Status: 403, Fields: map[string]any{"error.metadata.limit_source": "openrouter_guardrail_budget"}, NoHeaders: []string{"retry-after"}},
		{Name: "spend_limit", Err: core.Error{Kind: core.ErrSpendLimit}, Status: 403, Fields: map[string]any{"error.metadata.limit_source": "openrouter_workspace_budget"}},
		{Name: "content_filter", Err: core.Error{Kind: core.ErrContentFilter}, Status: 403, Fields: map[string]any{"error.metadata.error_type": "content_policy_violation"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission, Message: "no"}, Status: 403, Fields: map[string]any{"error.metadata.error_type": "permission_denied"}},
		// The engine's miss on the model field: the real gateway's 400 (task 163).
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 400, Fields: map[string]any{"error.code": 400, "error.message": "gpt-x is not a valid model ID", "user_id": "user_wwz"}, Headers: map[string]string{"x-generation-id": "*"}},
		{Name: "model_no_access", Err: core.Error{Kind: core.ErrModelNotFound, Message: "no"}, Status: 404, Fields: map[string]any{"error.metadata.error_type": "not_found"}},
		{Name: "request_too_large", Err: core.Error{Kind: core.ErrRequestTooLarge}, Status: 413, Fields: map[string]any{"error.metadata.error_type": "payload_too_large"}},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second, Extra: lim("rpm")}, Status: 429, Fields: map[string]any{"error.metadata.error_type": "rate_limit_exceeded"}, Headers: map[string]string{"retry-after": "*", "X-RateLimit-Limit": "100", "X-RateLimit-Remaining": "99", "X-RateLimit-Reset": "*"}, WithRateLimit: true},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.metadata.error_type": "server"}},
		{Name: "bad_gateway", Err: core.Error{Kind: core.ErrBadGateway}, Status: 502, Fields: map[string]any{"error.metadata.error_type": "provider_unavailable"}},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded}, Status: 503, Fields: map[string]any{"error.metadata.error_type": "provider_overloaded"}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 408, Fields: map[string]any{"error.metadata.error_type": "timeout"}},
	}
}

func openRouterMessagesErrors() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad"}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error"}},
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.type": "authentication_error", "error.error_type": "authentication", "error.message": "No cookie auth credentials found", "request_id": nil}, NoHeaders: []string{"x-generation-id", "request-id"}},
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error", "error.message": "gpt-x is not a valid model ID", "error.error_type": "invalid_request"}, Headers: map[string]string{"x-generation-id": "*"}},
		{Name: "quota", Err: core.Error{Kind: core.ErrQuota}, Status: 402, Fields: map[string]any{"error.type": "billing_error"}},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Status: 429, Fields: map[string]any{"error.type": "rate_limit_error"}, Headers: map[string]string{"retry-after": "*"}},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded}, Status: 529, Fields: map[string]any{"error.type": "overloaded_error"}},
	}
}

// openRouterSlug is the slug of ADR 0038: author/model ids as they are,
// white-wizard/<id> otherwise.
func openRouterSlug(id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	return "white-wizard/" + id
}

func openRouterProfile() Profile {
	chat := Surface{
		// The PROCESSING comment only goes out while the first byte waits
		// (spec 006 16.7): none on these instant streams.
		Envelope: EnvOpenRouter, RateLimit: RLNone, ReasoningField: "reasoning", ReasoningDetails: true,
		FinalChunkWithChoice: true, NativeFinishReason: true, UsageAlways: true, ReasoningNull: true,
		UsageCost: true, ReasoningObject: true, StreamErrorFinishReason: true,
		SuccessHeaders: []string{"x-generation-id"},
		Errors:         openRouterErrors(),
		RetryAfter: []RetryAfterCase{
			{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Want: "2"},
			{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Want: "3"},
			{Name: "quota_inflight", Err: core.Error{Kind: core.ErrQuota, Extra: lim("inflight")}, Want: "2"},
		},
		IDs: &IDFormat{Response: reGen, ToolCall: reCall, Request: reGen},
	}
	cmpl := chat
	cmpl.ReasoningNull = false
	msgs := Surface{
		// request-id and x-generation-id carry the generation id, which is
		// the body's request_id (null on the 401; spec 006 16.7).
		Envelope: EnvAnthropic, RateLimit: RLNone, RequestIDHeader: true, RequestIDFormat: reGen, AuthRequestIDNull: true,
		UsageCost:      true,
		SuccessHeaders: []string{"x-generation-id", "request-id"},
		Accepted: map[string]string{ //nolint:gosec // G101: test case names, not credentials
			"missing_max_tokens":        "task 161, validation fixtures",
			"tool_choice_without_tools": "task 342, tool-choice/any_notools.json",
		},
		Errors: openRouterMessagesErrors(),
		RetryAfter: []RetryAfterCase{
			{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, Want: "2"},
		},
		IDs: &IDFormat{Response: reMsg, ToolCall: reToolu, Request: reGen},
	}
	return Profile{
		Routes: concat(
			routes("POST", "chat", "/api/v1/chat/completions"),
			routes("POST", "completions", "/api/v1/completions"),
			routes("POST", "messages", "/api/v1/messages"),
			routes("GET", "models", "/api/v1/models"),
			routes("GET", "model", "/api/v1/models/*"), // serves no model: real "Not Found" (ADR 0046)
			routes("GET", "model", "/api/v1/model/*"),  // {author}/{slug}, ADR 0040
			routes("GET", "key_info", "/api/v1/key"),
			routes("GET", "key_info", "/api/v1/auth/key"),
			routes("GET", "key_info", "/api/v1/credits"), // ADR 0059
			routes("GET", "key_info", "/api/v1/keys"),
			routes("POST", "key_info", "/api/v1/keys"),
		),
		Credential: credOpenRouter, Key: "sk-or-v1-wwztest",
		Foreign:             []map[string]string{credOpenAI, credAnthropic, credOpenCode},
		Models:              "openrouter",
		ModelNotFoundStatus: 404,
		// GetModel(nil, "nope"): an id without an author, the real "Not Found"
		// (checked in task 163).
		ModelNotFoundFields: map[string]any{"error.code": 404, "error.message": "Not Found"},
		ModelName:           openRouterSlug,
		Policy:              driver.LimitPolicy{Entities: []string{"ip", "key", "account"}, TPMReserve: "max_tokens", ResetFormat: "seconds"},
		Surfaces:            map[driver.Kind]Surface{driver.KindChat: chat, driver.KindCompletions: cmpl, driver.KindMessages: msgs},
		Responses:           &ResponsesProfile{Done: true, Stateless: true, ID: reGen},
	}
}
