// Package driver defines the provider driver interface and the registry.
// A driver owns everything dialect-specific: routes, authentication header,
// request parsing, success and error serialisation, rate-limit headers and
// the /models listing. The engine owns everything else.
package driver

import (
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Kind is the logical handler a route maps to.
type Kind string

const (
	KindChat        Kind = "chat"
	KindCompletions Kind = "completions"
	KindResponses   Kind = "responses"
	KindMessages    Kind = "messages"
	KindModels      Kind = "models"
	KindModel       Kind = "model" // single model: path has {id}
	KindCountTokens Kind = "count_tokens"
	KindKeyInfo     Kind = "key_info" // OpenRouter GET /api/v1/key
	KindUsage       Kind = "usage"    // OpenCode GET /zen/go/v1/usage and the Console
	KindEmbeddings  Kind = "embeddings"
	// KindGenerate is Gemini's POST models/{model}:{method}: the model and
	// the method (generateContent, streamGenerateContent, countTokens) are
	// in the path, which the driver reads from x-wwz-path.
	KindGenerate Kind = "generate"
	// KindResource is an auxiliary route the driver serves through Extra
	// and that needs the method, the path or the query (Gemini's paged
	// models listing and cachedContents).
	KindResource Kind = "resource"
	// KindRouteMiss is a request no route serves (unknown path or wrong
	// method) that the server hands to a RouteMisser (task 235).
	KindRouteMiss Kind = "route_miss"
)

// Request-header slots the server fills with what is not a header. Like
// the account slots, the NUL prefix keeps clients from sending them.
const (
	// QueryHeader carries the raw query string ("alt=sse&key=...").
	QueryHeader = "\x00wwz-query"
	// MethodHeader carries the HTTP method.
	MethodHeader = "\x00wwz-method"
	// OriginHeader carries the origin of wwz itself for the request,
	// <scheme>://<host><external prefix><mount prefix> (ADR 0073), which
	// the server computes after routing and always overwrites.
	OriginHeader = "\x00wwz-origin"
)

// Origin is the origin of wwz the server stored in req: the base of every
// absolute URL a driver returns pointing to its own API or site (ADR
// 0073). Empty outside the server: the URL comes out relative.
func Origin(req *core.Request) string {
	if req == nil {
		return ""
	}
	return req.Header(OriginHeader)
}

// OriginOf is Origin over the headers h.
func OriginOf(h Headers) string {
	if h == nil {
		return ""
	}
	return h.Get(OriginHeader)
}

// Query returns the parsed query string the server stored in h.
func Query(h Headers) url.Values {
	if h == nil {
		return url.Values{}
	}
	v, _ := url.ParseQuery(h.Get(QueryHeader))
	return v
}

// ExtraBeforeAuth marks a ParseRequest error the provider raises before it
// looks at the credential (Gemini's JSON transcoding: syntax, unknown field,
// wrong type), so it wins over an invalid or missing key even on a driver
// whose Conventions put the key first.
const ExtraBeforeAuth = "wwz:before_auth"

// ExtraAfterModel marks a ParseRequest error the provider raises only
// after it resolved the model (Gemini's backend checks, task 235): its
// value is the requested model name, and the engine answers the unknown
// model's error first when the name resolves to none.
const ExtraAfterModel = "wwz:after_model"

// AfterModel returns the model name e carries in ExtraAfterModel.
func AfterModel(e *core.Error) (string, bool) {
	if e == nil {
		return "", false
	}
	name, ok := e.Extra[ExtraAfterModel].(string)
	return name, ok
}

// BeforeAuth reports whether e carries ExtraBeforeAuth.
func BeforeAuth(e *core.Error) bool {
	if e == nil {
		return false
	}
	b, _ := e.Extra[ExtraBeforeAuth].(bool)
	return b
}

// Route is a path the driver serves. Path may end with "/*" to match a
// trailing segment (used for /v1/models/{id}).
type Route struct {
	Method string
	Path   string
	Kind   Kind
}

// Headers gives read access to request headers by lower-cased name.
type Headers interface {
	Get(name string) string
}

// MapHeaders is a Headers over a map.
type MapHeaders map[string]string

func (m MapHeaders) Get(name string) string { return m[name] }

// ModelInfo is what /models exposes.
type ModelInfo struct {
	Cfg *config.ModelConfig
	// Catalog is the catalog the listing serves (CatalogRouter, ADR
	// 0072); "" without one. Pricing gives the model's price in it.
	Catalog string
}

// LimitPolicy declares which entity levels the dialect limits.
type LimitPolicy struct {
	// Entities in evaluation order, e.g. ip, key, account, project.
	Entities []string
	// TPMReserve: "max_tokens" (OpenAI) or "none" (Anthropic OTPM counts generated only).
	TPMReserve string
	// ExcludeCacheRead: cache_read tokens do not count (Anthropic ITPM).
	ExcludeCacheRead bool
	// ResetFormat: "go_duration" | "rfc3339" | "seconds".
	ResetFormat string
}

// Credential is what the driver extracted from the request.
type Credential struct {
	Key       string // the presented key, empty when absent
	Scheme    string // bearer | x-api-key | none
	Workspace string
	Version   string   // anthropic-version etc.
	Missing   []string // required headers that were missing
	// Anonymous marks a credential that stands for no account (OpenCode's
	// public, ADR 0072): the engine never looks it up in keys[], even with
	// server.auth: none, and the request has no key, project nor account.
	// Only a driver with BillingDecider can have one; it decides what the
	// credential is refused on. Key keeps the value as presented.
	Anonymous bool
	// KeyLabel is set by the engine when a KeyResolver found that Key is
	// a token standing for the keys[] entry with this label (OpenCode's
	// Console access token, ADR 0072): the engine authenticates that
	// entry, and Key keeps the token as presented.
	KeyLabel string
}

// KeyLookup finds a keys[] entry of the current configuration by label;
// nil when there is none.
type KeyLookup func(label string) *config.KeyConfig

// KeyResolver is implemented by drivers whose provider issues credentials
// that stand for a key (OpenCode's Console access tokens, signed with the
// value of a keys[] entry; ADR 0072, item 17). It is optional: the engine
// finds it with As and asks it only about a presented key that matches no
// keys[] entry. ResolveKey returns the label of the entry token stands for
// at now; false when token is none of the driver's.
type KeyResolver interface {
	ResolveKey(token string, keys KeyLookup, now time.Time) (label string, ok bool)
}

// PublicRouter is implemented by drivers with auxiliary routes the
// provider answers without a credential (OpenCode's Console device flow;
// spec 006 names it PublicRoute): the engine serves them through Extra
// even when the key is missing or invalid. It is optional: without it
// every auxiliary route needs a key when auth is on.
type PublicRouter interface {
	PublicRoute(route Route) bool
}

// IsPublicRoute reports whether d answers route without a credential.
func IsPublicRoute(d Driver, route Route) bool {
	p, ok := As[PublicRouter](d)
	return ok && p.PublicRoute(route)
}

// Account is who a request belongs to, as the engine resolved it: the key
// as presented and the account and project of the matching keys[] entry
// (ADR 0049). Account and Project are "" when the key matches no entry
// (auth: none, unknown or refused key). Drivers derive the account headers
// of the real provider (openai-organization, anthropic-organization-id...)
// from it and mask Key in the 401. Where Account and Project come from is
// the engine's business, so a new source (config accounts[]/projects[])
// changes the engine only.
type Account struct {
	Key     string
	Account string
	Project string
}

// The request-header slots that carry the Account. The NUL prefix is not a
// valid HTTP header name, so a client cannot send them.
const (
	accountKeyHeader     = "\x00wwz-account-key"
	accountAccountHeader = "\x00wwz-account"
	accountProjectHeader = "\x00wwz-project"
)

// SetAccount stores a in h, replacing whatever was there.
func SetAccount(h MapHeaders, a Account) {
	if h == nil {
		return
	}
	h[accountKeyHeader] = a.Key
	h[accountAccountHeader] = a.Account
	h[accountProjectHeader] = a.Project
}

// AccountOf reads the Account the engine stored in the request headers;
// the zero Account when req is nil or the engine stored none.
func AccountOf(req *core.Request) Account {
	if req == nil {
		return Account{}
	}
	return Account{
		Key:     req.Header(accountKeyHeader),
		Account: req.Header(accountAccountHeader),
		Project: req.Header(accountProjectHeader),
	}
}

// The request-header slots that carry the route of the request, for the
// answers of auxiliary routes that share a Kind (task 250).
const (
	routeMethodHeader = "\x00wwz-route-method"
	routePathHeader   = "\x00wwz-route-path"
)

// SetRoute stores the method and path of the request in h.
func SetRoute(h MapHeaders, method, path string) {
	if h == nil {
		return
	}
	h[routeMethodHeader] = method
	h[routePathHeader] = path
}

// RouteOf reads the route SetRoute stored in h; empty when none.
func RouteOf(h Headers) (method, path string) {
	if h == nil {
		return "", ""
	}
	return h.Get(routeMethodHeader), h.Get(routePathHeader)
}

// RouteOfRequest is RouteOf over the headers of req (nil-safe).
func RouteOfRequest(req *core.Request) (method, path string) {
	if req == nil {
		return "", ""
	}
	return req.Header(routeMethodHeader), req.Header(routePathHeader)
}

// Driver is implemented by each dialect.
type Driver interface {
	Name() string
	// Protocol is the model protocol name used in config (openai, anthropic...).
	Protocol() string
	Routes() []Route
	// Sniff reports whether a request on a shared path (e.g. /v1/models at
	// the root) looks like this dialect.
	Sniff(h Headers) bool
	Authenticate(h Headers) Credential
	ParseRequest(kind Kind, body []byte, h Headers) (*core.Request, *core.Error)
	WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error
	StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter
	WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState)
	// ListModels and GetModel answer the models routes. req carries the
	// request headers and the Account the engine resolved (no body fields),
	// so the account headers match the other routes of the same key.
	ListModels(w core.ResponseWriter, req *core.Request, models []ModelInfo)
	GetModel(w core.ResponseWriter, req *core.Request, m *ModelInfo, id string)
	LimitPolicy() LimitPolicy
	// Extra serves dialect-specific auxiliary routes (key info, usage...).
	Extra(kind Kind, w core.ResponseWriter, h Headers, body []byte, ctx ExtraContext)
}

// RouteMiss describes a request no route of the driver serves.
type RouteMiss struct {
	Method string
	// Path is relative to the driver's mount prefix.
	Path string
	// Allow lists the methods the path is served with; empty when the
	// path is unknown (404), else the method is wrong (405).
	Allow []string
	// Kind is the kind of the path's route when Allow is not empty.
	Kind Kind
}

// RouteMisser is implemented by drivers that answer unknown routes and
// wrong methods as their provider does (task 235). It is optional: the
// server answers the others in OpenAI's shape. authErr is the engine's
// refusal of the credential (nil when the key is valid or auth is off);
// AccountOf(req).Key tells a presented key from a missing one.
type RouteMisser interface {
	WriteRouteMiss(w core.ResponseWriter, req *core.Request, miss RouteMiss, authErr *core.Error)
}

// Models is what the core knows about a requested model, for the
// validators of the providers that check the model inside the body checks.
// The core resolves the name as the engine does (id, alias or the driver's
// slug, an optional -scenario- suffix), in the driver's catalog, speaking
// the route's dialect and not past retires_at. Without configuration it
// knows every model.
type Models interface {
	// KnownModel reports whether name, on a kind route, is served.
	KnownModel(kind Kind, name string) bool
	// Model is KnownModel and, for an unknown model, whether it exists but
	// is past retires_at.
	Model(kind Kind, name string) (known, retired bool)
}

// RequestValidator is implemented by drivers that refuse an invalid body
// with the real provider's answer (spec 004, ADRs 0029 and 0042), in the
// order the provider checks. It is optional: the core finds it with As and
// runs it inside ParseRequest, before the driver's own parse; a driver
// without it does not validate. A nil error lets the driver parse.
type RequestValidator interface {
	ValidateRequest(kind Kind, body []byte, h Headers, models Models) *core.Error
}

// RawReply is a complete HTTP answer a validator sends byte for byte
// (status, content type, "" sends none, and body): the gateways answer some
// validation failures with bodies the driver's error envelope does not
// produce.
type RawReply struct {
	Status      int
	ContentType string
	Body        []byte
}

// rawReplyKey marks, in core.Error.Extra, a reply written byte for byte.
const rawReplyKey = "wwz_raw_reply"

// RawError is an invalid-request error answered with r.
func RawError(msg string, r RawReply) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Status: r.Status, Message: msg, Extra: map[string]any{rawReplyKey: r}}
}

// RawReplyOf returns the raw reply e carries, if any.
func RawReplyOf(e *core.Error) (RawReply, bool) {
	if e == nil {
		return RawReply{}, false
	}
	r, ok := e.Extra[rawReplyKey].(RawReply)
	return r, ok
}

// Declarer is implemented by drivers that declare to the configuration
// their catalog dialects, drivers[].options, models[].extra keys and the
// other facts internal/config checks (config.DriverDecl, spec 005). It is
// optional: a driver without it has every model and accepts any option.
// Register publishes the declaration, so the configuration knows no driver
// by name (R15).
type Declarer interface {
	Declaration() config.DriverDecl
}

// CacheStyle is the prompt cache of a real provider family the engine
// simulates (spec 007 "Cache de prompt", ADR 0046); the engine owns the
// sizes and TTLs of each style.
type CacheStyle uint8

const (
	// CacheNone: no prompt cache.
	CacheNone CacheStyle = iota
	// CacheAutomatic: automatic prefixes from 1 024 tokens in steps of 128,
	// 5–10 min unused and at most 1 h, longer with the request's retention
	// options (OpenAI).
	CacheAutomatic
	// CacheBreakpoints: cache_control breakpoints from 512 tokens, 5 min or
	// 1 h (Anthropic).
	CacheBreakpoints
	// CacheImplicit: implicit prefixes from 4 096 tokens at the template's
	// exact boundaries, 5 min (Gemini).
	CacheImplicit
)

// PromptTemplate is a dialect's own prompt template (Gemini's, measured
// with countTokens): the prompt count and the size of each part, which the
// prompt cache lays out block by block.
type PromptTemplate interface {
	CountPrompt(tk generate.Tokenizer, req *core.Request) generate.PromptBreakdown
	// Declaration, Call and Response are the cost of a tool declaration,
	// of a tool call and of a tool result.
	Declaration(tk generate.Tokenizer, t core.ToolDef) int
	Call(tk generate.Tokenizer, name, args string) int
	Response(tk generate.Tokenizer, name, response string) int
	// ContentOverhead is the cost of a turn besides its text.
	ContentOverhead() int
	// MediaTokens is the cost of images and audio parts.
	MediaTokens(images, audio int) int
}

// EngineDecl are the real provider's habits the engine applies around a
// driver outside its writers (R15: the engine knows no driver by name).
// The zero value is OpenAI's chat template, the simulated tokenizer, no
// prompt cache and the content filter as the driver's error.
type EngineDecl struct {
	// Text is the prose generator (and tokenizer) of the models under
	// tokenizer auto without a BPE of their own (Gemini's calibration,
	// task 265); nil keeps the simulated one.
	Text *generate.Generator
	// Template is the dialect's prompt template; nil is the chat template
	// of generate.CountPrompt.
	Template PromptTemplate
	// ClaudeTokens: a Claude 4.7 or later counts its output as it counts
	// the prompt (task 342).
	ClaudeTokens bool
	// PromptCache is the cache style of model on endpoint ep; nil is
	// CacheNone.
	PromptCache func(model *config.ModelConfig, ep core.Endpoint) CacheStyle
	// ContentFilterAsFinish: the content filter is a 200 that ends in
	// finish_reason content_filter / stop_reason refusal (specs 006, 009);
	// otherwise the driver writes the content_filter error.
	ContentFilterAsFinish bool
	// RefusalEmpty: a refusal before any output has no content, no
	// reasoning and no output tokens (Anthropic: content [] and
	// output_tokens 0, "Refusals and fallback"); otherwise the refusal
	// carries its text (OpenAI's message.refusal).
	RefusalEmpty bool
	// MalformedFrame is the dialect's SSE frame with cut JSON of the
	// malformed fault; nil is a chat completion chunk.
	MalformedFrame []byte
}

// EngineDeclarer is implemented by drivers that declare an EngineDecl. It
// is optional: a driver without it gets the zero EngineDecl.
type EngineDeclarer interface {
	EngineDeclaration() EngineDecl
}

// EngineDeclOf is what d declares to the engine: its own (As), else, for
// wrappers that only keep Name(), the declaration of the registered driver
// of that name; the zero EngineDecl when there is none.
func EngineDeclOf(d Driver) EngineDecl {
	if d == nil {
		return EngineDecl{}
	}
	if x, ok := As[EngineDeclarer](d); ok {
		return x.EngineDeclaration()
	}
	name := d.Name()
	regMu.RLock()
	decl, cached := engineDecls[name]
	f, ok := factories[name]
	regMu.RUnlock()
	if cached || !ok {
		return decl
	}
	if r, err := f(nil); err == nil && r != nil {
		if x, ok := As[EngineDeclarer](r); ok {
			decl = x.EngineDeclaration()
		}
	}
	regMu.Lock()
	engineDecls[name] = decl
	regMu.Unlock()
	return decl
}

// Unwrapper is implemented by wrappers that embed a Driver (the server's
// validation), so As finds the optional interfaces of the wrapped one.
type Unwrapper interface {
	Unwrap() Driver
}

// As finds T on d or on the drivers d wraps (Unwrapper), outermost first.
func As[T any](d Driver) (T, bool) {
	for d != nil {
		if t, ok := d.(T); ok {
			return t, true
		}
		u, ok := d.(Unwrapper)
		if !ok {
			break
		}
		d = u.Unwrap()
	}
	var zero T
	return zero, false
}

// IDMinter is implemented by drivers whose identifiers follow the real
// provider's format (chatcmpl-, msg_01, toolu_01, gen-, req_). It is optional:
// the engine falls back to its own opaque ids for drivers without it.
type IDMinter interface {
	// RequestID shapes the request id (x-request-id / request-id header and
	// request_id in error bodies) from a token that is unique per request.
	RequestID(kind Kind, unique string) string
	// ResponseID returns the id of the response object. rng is seeded like
	// the rest of the response, so the id is reproducible under a fixed seed.
	ResponseID(kind Kind, rng *generate.RNG, now time.Time) string
	// ToolCallID returns the id of one tool call.
	ToolCallID(kind Kind, rng *generate.RNG) string
}

// ModelNamer is implemented by drivers whose provider names models
// differently from models[].id (OpenRouter's author/model slug, ADR 0038).
// It is optional: without it the engine resolves the requested name with
// config.FindModel and answers with models[].id. Metrics, events, limits and
// scenarios keep using models[].id either way.
type ModelNamer interface {
	// ModelName is the name the response's model field carries for m.
	ModelName(m *config.ModelConfig) string
	// UnprefixModel strips the provider's prefix from a requested name; the
	// engine tries it only when the exact name matches no id or alias.
	UnprefixModel(name string) (string, bool)
}

// Conventions are the provider habits the engine applies around a driver,
// copied from the real provider (spec 006 16.7, ADR 0049).
type Conventions struct {
	// RequestIDHeader is the response header that carries the request id
	// ("x-request-id" on OpenAI, "request-id" on Anthropic); "" means the
	// real provider sends none (OpenRouter, OpenCode).
	RequestIDHeader string
	// AuthFirst: an invalid or missing key wins over every error found in
	// the body (parse, validation, unknown model). When false the provider
	// checks the required headers, then the body and the model, and only
	// then the key (OpenCode Go: an unknown model is a 400 even with an
	// invalid key).
	AuthFirst bool
	// ModelVersion: the model field of every response is models[].version
	// when set (the dated snapshot), else models[].id, never the alias that
	// was asked for.
	ModelVersion bool
	// ModelsWithoutKey: GET /models answers the listing to a request that
	// presents no key; a presented invalid key is still refused (OpenCode
	// Go, conferred on 2026-10-31). Drivers whose listing ignores the key
	// altogether set ModelsPublic instead (OpenRouter).
	ModelsWithoutKey bool
	// ModelsPublic: GET /models answers the listing whatever the key, valid,
	// invalid or absent (OpenRouter GET /api/v1/models, ADR 0053).
	ModelsPublic bool
	// ModelIgnoresKey: GET /models/{id} answers without looking at the key,
	// valid, invalid or absent (OpenCode Go: 404 with an empty body for any
	// id, conferred on 2026-10-31).
	ModelIgnoresKey bool
}

// Conventioner is implemented by drivers that declare their Conventions.
// It is optional: drivers without it get DefaultConventions(Name()).
type Conventioner interface {
	Conventions() Conventions
}

// ConventionsOf returns d's conventions: its own when it implements
// Conventioner, else the defaults of its name. Wrappers that embed a Driver
// keep Name(), so they keep the defaults too. A nil driver gets OpenAI's.
func ConventionsOf(d Driver) Conventions {
	if d == nil {
		return DefaultConventions("")
	}
	if c, ok := d.(Conventioner); ok {
		return c.Conventions()
	}
	return DefaultConventions(d.Name())
}

// DefaultConventions are the conventions of each real provider, observed on
// 2026-10-31 (tmp/drift triage, spec 006 16.7). A name that is not a known
// provider behaves like OpenAI.
func DefaultConventions(name string) Conventions {
	switch name {
	case "anthropic":
		return Conventions{RequestIDHeader: "request-id", AuthFirst: true, ModelVersion: true}
	case "openrouter":
		return Conventions{AuthFirst: true, ModelsPublic: true}
	case "opencode":
		return Conventions{ModelsWithoutKey: true, ModelIgnoresKey: true}
	case "gemini":
		// The key before the body's semantics and the model; the JSON
		// transcoding errors still come first (ExtraBeforeAuth).
		return Conventions{AuthFirst: true}
	}
	return Conventions{RequestIDHeader: "x-request-id", AuthFirst: true, ModelVersion: true}
}

// ExtraContext gives auxiliary handlers what they need.
type ExtraContext struct {
	Key    *config.KeyConfig
	Limits config.Limits
	State  core.RateLimitState
	Models []ModelInfo
	// KeyBalance and AccountBalance are the store's view of the key and
	// of its account (spend, credits), read for the key-info routes; nil
	// when the engine did not read them.
	KeyBalance     *Balance
	AccountBalance *Balance

	// The billing view of a driver with BillingDecider (ADR 0072), filled
	// on its auxiliary routes for a request with a key: the account id,
	// the plan of its tier, whether the key is a service account, the
	// quota owner and its state, the quota of each model and the budgets.
	AccountID      string
	Plan           string
	ServiceAccount bool
	// QuotaOwner is the owner of the subscription quota (nil without
	// quota_usd); ModelQuotas the quota_usd.models of that owner.
	QuotaOwner  *QuotaView
	ModelQuotas []ModelQuotaView
	// Budgets are the spend_monthly_usd (Model "") and the
	// spend_monthly_models_usd of the account, project and key, in that
	// order and by model key within a level.
	Budgets []BudgetView

	// Now is the engine's clock at the start of the request (zero when
	// the caller set none: time.Now). Keys finds the keys[] entries of the
	// configuration by label (nil outside the engine).
	Now  time.Time
	Keys KeyLookup
}

// QuotaView is the subscription quota of its owner and its state.
type QuotaView struct {
	// Kind is the entity kind of the owner (account, project or key) and
	// ID its id.
	Kind string
	ID   string
	// Quota is the owner's quota_usd, Models left out (ModelQuotas).
	Quota config.QuotaUSD
	// State is the store's view of the owner; nil when it could not be read.
	State *Balance
}

// ModelQuotaView is the quota of one model (quota_usd.models) of the
// quota owner: Model is the key as declared (models[].id or
// <catalog>/<id>) and Quota carries its monthly amount with the owner's
// window percentages.
type ModelQuotaView struct {
	Model string
	Quota config.QuotaUSD
	State *Balance
}

// BudgetView is one budget (spend_monthly_usd of coverage all, Model "", or
// one entry of spend_monthly_models_usd) of a level: Level is account,
// project or key and ID its id; SpentUSD the spend of the UTC month.
type BudgetView struct {
	Level    string
	ID       string
	Model    string
	LimitUSD float64
	SpentUSD float64
}

// Balance is the spend and the credit balance of an entity, as the store
// keeps them.
type Balance struct {
	// SpendUSD is the spend of the current UTC month.
	SpendUSD float64
	// CreditsUSD is the credit balance; nil when the store has none yet
	// (the configured credits_usd, when any, is the starting balance).
	CreditsUSD *float64
	// PurchasedUSD is the accumulated credit purchases (admin add_usd).
	PurchasedUSD float64
	// SpendDailyUSD and SpendWeeklyUSD are the spend of the current UTC
	// day and week (from Monday); SpentTotalUSD is the accumulated spend.
	SpendDailyUSD  float64
	SpendWeeklyUSD float64
	SpentTotalUSD  float64
	// FreeRequestsToday counts the requests of the current UTC day to
	// :free models.
	FreeRequestsToday int64
	// Quota5hUSD is the quota spend of the last 5 h and Quota5hSince the
	// time of its oldest spend (zero without spend), OpenCode's rolling
	// window.
	Quota5hUSD   float64
	Quota5hSince time.Time
	// QuotaWeeklyUSD and QuotaMonthlyUSD are the quota debit of the current
	// UTC week (from Monday 00:00) and month, OpenCode's weekly and monthly
	// windows; they follow the quota debit, not the spend (task 432).
	QuotaWeeklyUSD  float64
	QuotaMonthlyUSD float64
}

// Factory builds a driver from mount options.
type Factory func(opts map[string]string) (Driver, error)

var (
	regMu     sync.RWMutex
	factories = map[string]Factory{}
	// engineDecls caches EngineDeclOf's fallback by driver name.
	engineDecls = map[string]EngineDecl{}
)

// Register adds a driver factory; called from init(). It also publishes to
// internal/config what the driver declares (Declarer), read from a driver
// built with no options the first time the configuration needs it.
func Register(name string, f Factory) {
	regMu.Lock()
	factories[name] = f
	delete(engineDecls, name)
	regMu.Unlock()
	config.RegisterDriver(name, func() (config.DriverDecl, bool) { return declOf(f) })
}

// declOf is what the driver f builds declares; false when it declares
// nothing or cannot be built without options.
func declOf(f Factory) (config.DriverDecl, bool) {
	d, err := f(nil)
	if err != nil || d == nil {
		return config.DriverDecl{}, false
	}
	x, ok := As[Declarer](d)
	if !ok {
		return config.DriverDecl{}, false
	}
	return x.Declaration(), true
}

// New instantiates a registered driver.
func New(name string, opts map[string]string) (Driver, error) {
	regMu.RLock()
	f, ok := factories[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("driver %q is not registered (available: %v)", name, Names())
	}
	return f(opts)
}

// Names lists registered drivers.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
