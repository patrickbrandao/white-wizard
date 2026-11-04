// Package engine orchestrates one request: authenticate, resolve the model,
// pick a scenario, enforce limits, inject chaos, generate the response,
// schedule latency and stream it through the driver's writer.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/latency"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

// Streamer is implemented by writers that can switch to a streaming body.
// The engine calls Stream with the function that produces the body; the
// transport decides when to run it (fasthttp runs it after headers go out).
type Streamer interface {
	Stream(fn func(core.ResponseWriter))
}

// Input is one HTTP request handed to the engine by the server.
type Input struct {
	Driver    driver.Driver
	Kind      driver.Kind
	Method    string
	Path      string
	Headers   driver.MapHeaders
	Body      []byte
	ClientIP  string
	PathParam string
	// Miss is the route miss of a KindRouteMiss request.
	Miss driver.RouteMiss
}

// Metrics bundles the engine's instruments.
type Metrics struct {
	Registry       *metrics.Registry
	Requests       *metrics.Counter
	InFlight       *metrics.Gauge
	Streams        *metrics.Gauge
	ErrorsInjected *metrics.Counter
	RateLimited    *metrics.Counter
	TTFT           *metrics.Histogram
	ITL            *metrics.Histogram
	E2E            *metrics.Histogram
	Tokens         *metrics.Counter
	ToolCalls      *metrics.Counter
	ScenarioSteps  *metrics.Counter
	Overhead       *metrics.Histogram
	Blocked        *metrics.Counter
	QueueDepth     *metrics.Gauge
	// ClientVerificationFailures counts the refusals of the strict client
	// verification by driver and reason (spec 013, spec 020).
	ClientVerificationFailures *metrics.Counter

	// vLLM mirror (metrics.vllm_mirror, spec 013): nil until MirrorVLLM.
	VLLMRunning *metrics.Gauge
	VLLMWaiting *metrics.Gauge
	VLLMTTFT    *metrics.Histogram

	buckets []float64
}

// NewMetrics registers the instruments.
func NewMetrics(reg *metrics.Registry, buckets []float64) *Metrics {
	return &Metrics{
		Registry:       reg,
		Requests:       reg.NewCounter("wwz_requests_total", "Requests by driver, model, endpoint and status", "driver", "model", "endpoint", "status"),
		InFlight:       reg.NewGauge("wwz_requests_in_flight", "Requests being served", "driver"),
		Streams:        reg.NewGauge("wwz_streams_open", "Streaming responses open", "driver"),
		ErrorsInjected: reg.NewCounter("wwz_errors_injected_total", "Injected errors by kind and phase", "driver", "kind", "phase"),
		RateLimited:    reg.NewCounter("wwz_rate_limited_total", "Requests rejected by limits", "driver", "limit"),
		TTFT:           reg.NewHistogram("wwz_ttft_seconds", "Simulated time to first token", buckets, "driver", "model"),
		ITL:            reg.NewHistogram("wwz_itl_seconds", "Simulated inter-token latency", buckets, "driver", "model"),
		E2E:            reg.NewHistogram("wwz_e2e_seconds", "End-to-end request duration", buckets, "driver", "model"),
		Tokens:         reg.NewCounter("wwz_tokens_total", "Tokens by kind", "driver", "model", "kind"),
		ToolCalls:      reg.NewCounter("wwz_tool_calls_total", "Tool calls emitted", "driver", "tool"),
		ScenarioSteps:  reg.NewCounter("wwz_scenario_steps_total", "Scenario steps served", "scenario", "step"),
		Overhead:       reg.NewHistogram("wwz_overhead_seconds", "Engine processing time excluding simulated latency", []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1}, "driver"),
		Blocked:        reg.NewCounter("wwz_blocked_total", "Requests refused for blocked or banned entities", "driver", "status"),
		QueueDepth:     reg.NewGauge("wwz_queue_depth", "Requests waiting for a slot (model concurrency full or on_limit: queue)", "driver"),
		ClientVerificationFailures: reg.NewCounter("wwz_client_verification_failures_total",
			"Requests refused by the strict client verification (client_verification, spec 020)", "driver", "reason"),
		buckets: buckets,
	}
}

// MirrorVLLM registers the vllm:* mirror (metrics.vllm_mirror): requests
// running and waiting per model and the TTFT histogram, under vLLM's names
// and its model_name label. Call it once, before serving (read at boot).
func (m *Metrics) MirrorVLLM() {
	m.VLLMRunning = m.Registry.NewGauge("vllm:num_requests_running", "Number of requests running (wwz mirror)", "model_name")
	m.VLLMWaiting = m.Registry.NewGauge("vllm:num_requests_waiting", "Number of requests waiting (wwz mirror)", "model_name")
	m.VLLMTTFT = m.Registry.NewHistogram("vllm:time_to_first_token_seconds", "Time to first token in seconds (wwz mirror)", m.buckets, "model_name")
}

// waiting marks one request of drv/model as waiting for a slot; the returned
// func undoes it.
func (m *Metrics) waiting(drv, model string) func() {
	m.QueueDepth.Inc(drv)
	if m.VLLMWaiting != nil {
		m.VLLMWaiting.Inc(model)
	}
	return func() {
		m.QueueDepth.Dec(drv)
		if m.VLLMWaiting != nil {
			m.VLLMWaiting.Dec(model)
		}
	}
}

// occupy accounts a request admitted into the model's latency tracker: it
// waits when the model's concurrency was already full (it entered inside
// max_queue), otherwise it runs. The returned func leaves the tracker and
// undoes the accounting.
func (m *Metrics) occupy(tr *latency.Tracker, drv, model string, queued bool) func() {
	if queued {
		done := m.waiting(drv, model)
		return func() { tr.Leave(); done() }
	}
	if m.VLLMRunning == nil {
		return tr.Leave
	}
	m.VLLMRunning.Inc(model)
	return func() { tr.Leave(); m.VLLMRunning.Dec(model) }
}

// Engine is the request pipeline.
type Engine struct {
	cfg       atomic.Pointer[config.Config]
	Store     limits.Store
	Injector  *chaos.Injector
	Scenarios *scenario.Registry
	M         *Metrics
	Log       *slog.Logger
	trackers  sync.Map // model -> *latency.Tracker
	counter   atomic.Uint64
	started   time.Time
	podName   string
	// Recorder keeps the last requests for /admin/requests (nil = off).
	Recorder *Recorder
	// Resilience tracks client retry behaviour per X-Test-Id.
	Resilience *Resilience
	// Now is the clock of the chaos decision (errors.outages windows and
	// admin chaos events); nil means time.Now. Tests inject it.
	Now func() time.Time
	// promoter assigns account tiers and promotes them (promote.go).
	promoter *limits.Promoter
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// New creates an engine.
func New(cfg *config.Config, store limits.Store, reg *scenario.Registry, m *Metrics, log *slog.Logger) *Engine {
	e := &Engine{Store: store, Injector: chaos.New(), Scenarios: reg, M: m, Log: log, started: time.Now()}
	e.cfg.Store(cfg)
	e.Resilience = NewResilience()
	limits.ShareCachedContents(store) // Gemini cachedContents across instances (task 265)
	return e
}

// Config returns the current configuration.
func (e *Engine) Config() *config.Config { return e.cfg.Load() }

// SetConfig swaps the configuration atomically.
func (e *Engine) SetConfig(c *config.Config) { e.cfg.Store(c) }

// SetPodName sets the x-wwz-pod header value.
func (e *Engine) SetPodName(n string) { e.podName = n }

// Started reports the process start time.
func (e *Engine) Started() time.Time { return e.started }

func (e *Engine) tracker(model string) *latency.Tracker {
	// Load first: LoadOrStore would allocate a Tracker on every request.
	if v, ok := e.trackers.Load(model); ok {
		return v.(*latency.Tracker)
	}
	v, _ := e.trackers.LoadOrStore(model, &latency.Tracker{})
	return v.(*latency.Tracker)
}

// Models returns the catalog of the driver named driverName (ADR 0048).
func (e *Engine) Models(driverName string) []driver.ModelInfo {
	cfg := e.Config()
	out := make([]driver.ModelInfo, 0, len(cfg.Models))
	for i := range cfg.Models {
		if cfg.Models[i].ModelSupports(driverName) {
			out = append(out, driver.ModelInfo{Cfg: &cfg.Models[i]})
		}
	}
	return out
}

// resolveModel finds the requested model: the exact id or alias, or, for
// drivers with a provider slug (driver.ModelNamer, ADR 0038), the slug as
// the provider resolves it (lookupModel). Failing that, a name with
// "-scenario-" resolves by the part before the last "-scenario-" (ADR 0040,
// spec 011 model_suffix). It also returns the name the request goes on with:
// the id or alias that matched, as configured, suffix
// included, which scenarios match on (model_suffix, match.model, seed).
// Labels use models[].id, never this name.
//
// A miss on a provider's model variant (OpenRouter ":free" of a model
// without one, task 186) comes back as the error the driver answers with.
func resolveModel(cfg *config.Config, drv driver.Driver, name string) (*config.ModelConfig, string, *core.Error) {
	m, short, miss := lookupModel(cfg, drv, name)
	if m != nil {
		return m, short, nil
	}
	if i := strings.LastIndex(name, scenarioSuffix); i > 0 {
		if m, short, _ := lookupModel(cfg, drv, name[:i]); m != nil {
			return m, short + name[i:], nil
		}
	}
	if miss != nil {
		return nil, name, &core.Error{Kind: core.ErrModelNotFound, Status: 404, Extra: map[string]any{config.ExtraModelVariant: miss}}
	}
	return nil, name, nil
}

// servable reports whether a resolved model answers on a kind route of drv:
// it exists, is in the driver's catalog, speaks the route's dialect (except
// on OpenRouter and OpenCode, which serve their catalog on every route; ADRs
// 0048 and 0062), is in the route's catalog cat (models[].catalogs, ADR
// 0072; "" has every model) and is not past retires_at (ADR 0049).
func (e *Engine) servable(m *config.ModelConfig, drv driver.Driver, kind driver.Kind, cat string) bool {
	return Servable(m, drv.Protocol(), kind, e.now()) && m.InCatalog(cat)
}

// Servable is servable for the early validation of internal/server, which
// must refuse a model where the engine would (ADR 0053: a retired model is
// unknown at every step).
func Servable(m *config.ModelConfig, protocol string, kind driver.Kind, now time.Time) bool {
	return m != nil && m.ServedOn(protocol, Dialect(kind)) && !m.Retired(now)
}

// ResolveModel is resolveModel for the early validation of internal/server.
func ResolveModel(cfg *config.Config, drv driver.Driver, name string) (*config.ModelConfig, *core.Error) {
	m, _, err := resolveModel(cfg, drv, name)
	return m, err
}

// Dialect is the wire dialect of a route kind (ADR 0048): Messages and
// count_tokens are anthropic; Gemini's models/{m}:{method} is gemini; chat,
// completions, responses and embeddings are openai.
func Dialect(kind driver.Kind) string {
	switch kind {
	case driver.KindMessages, driver.KindCountTokens:
		return config.DialectAnthropic
	case driver.KindGenerate:
		return config.DialectGemini
	default:
		return config.DialectOpenAI
	}
}

// missingError is the answer to a required header the request lacks; the
// OpenCode session header has its own error (spec 006).
func missingError(cred driver.Credential) *core.Error {
	if cred.Missing[0] == "x-opencode-session" {
		return &core.Error{Kind: core.ErrMissingSession}
	}
	// Anthropic's text, with the colon (auth-order/validkey_no_version_*).
	return &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("%s: header is required", cred.Missing[0])}
}

// scenarioSuffix separates a model from the scenario it pins (model_suffix).
const scenarioSuffix = "-scenario-"

// lookupModel is the id or alias of name. A driver that names models with a
// provider slug (driver.ModelNamer, ADR 0038) resolves the slug instead, the
// way the provider does (config.FindModelBySlug, task 163). It returns the
// model and the id or alias as matched; the slug lookup also reads the
// provider's variant suffixes (config.FindModelVariant, task 186) and
// reports a miss on one.
func lookupModel(cfg *config.Config, drv driver.Driver, name string) (*config.ModelConfig, string, *config.VariantMiss) {
	if n, ok := drv.(driver.ModelNamer); ok {
		if m, matched, miss := cfg.FindModelVariant(name, drv.Protocol(), func(id string) string {
			return n.ModelName(&config.ModelConfig{ID: id})
		}); m != nil || miss != nil {
			return m, matched, miss
		}
		return nil, name, nil
	}
	if m := cfg.FindModel(name); m != nil {
		return m, name, nil
	}
	return nil, name, nil
}

// publicModels reports whether a models route answers whatever authErr
// says: the driver's listing is public (Conventions.ModelsPublic), its listing answers
// a request without a key (Conventions.ModelsWithoutKey) or its
// single-model route ignores the key (Conventions.ModelIgnoresKey).
// A request without a key to a single-model path the driver answers as an
// unknown route (keylessModelMiss) gets that answer too.
func publicModels(drv driver.Driver, kind driver.Kind, cred driver.Credential, param string) bool {
	conv := driver.ConventionsOf(drv)
	if kind == driver.KindModel {
		if km, ok := driver.As[keylessModelMiss](drv); ok && cred.Key == "" && km.KeylessModelMiss(param) {
			return true
		}
		return conv.ModelIgnoresKey
	}
	return conv.ModelsPublic || conv.ModelsWithoutKey && cred.Key == ""
}

// keylessModelMiss is implemented by drivers whose GET /models/{param}
// answers a request without a key with its unknown-route 404 before the key
// check (Anthropic's /v1/models/{id}/..., task 316).
type keylessModelMiss interface {
	KeylessModelMiss(param string) bool
}

// slugRouter is implemented by drivers with a single-model route whose
// parameter must be the provider's full model name (OpenRouter
// GET /api/v1/model/{author}/{slug}, ADR 0040).
type slugRouter interface {
	ModelPathBySlug(path string) bool
}

// findListed is GET /models/{id}: the listed model whose id is id. On a
// driver with a slug route (slugRouter) only that route serves a model, by
// its full slug in any case (OpenRouter GET /api/v1/model/{author}/{slug},
// checked against the real API in task 163); its other single-model paths
// serve none (ADR 0046).
func findListed(models []driver.ModelInfo, drv driver.Driver, path, id string) *driver.ModelInfo {
	n, named := drv.(driver.ModelNamer)
	if sr, ok := drv.(slugRouter); ok && named {
		if !sr.ModelPathBySlug(path) {
			return nil
		}
		// Variant suffixes as on the model field (task 186): a miss is
		// "Model not found".
		m, _, _ := config.ResolveVariant(id, func(name string) (*config.ModelConfig, string) {
			for i := range models {
				if strings.EqualFold(n.ModelName(models[i].Cfg), name) {
					return models[i].Cfg, models[i].Cfg.ID
				}
			}
			return nil, ""
		})
		for i := range models {
			if m != nil && models[i].Cfg == m {
				return &models[i]
			}
		}
		return nil
	}
	for i := range models {
		if models[i].Cfg.ID == id {
			return &models[i]
		}
	}
	return nil
}

// listedParam is the id GetModel reports for a miss: empty on a slug
// driver's path that serves no model, which then answers as the provider
// answers an unknown route (ADR 0046).
func listedParam(drv driver.Driver, path, id string) string {
	if sr, ok := drv.(slugRouter); ok && !sr.ModelPathBySlug(path) {
		return ""
	}
	return id
}

// request carries everything about one request through the pipeline.
type request struct {
	in Input
	// decl is what the driver declares to the engine (tokenizer, prompt
	// template and cache, content filter; R15).
	decl driver.EngineDecl
	req  *core.Request
	cfg  *config.Config
	key  *config.KeyConfig
	// cred is the credential the driver extracted; anon marks the
	// account-less one (driver.Credential.Anonymous) the engine honoured.
	cred  driver.Credential
	anon  bool
	model *config.ModelConfig
	// billing is the driver's billing of the request (interface (d), ADR
	// 0072) after Extra Usage; nil for a driver without it.
	billing  *driver.Billing
	subjects []limits.Subject
	rl       core.RateLimitState
	resp     *core.Response
	plan     *latency.Plan
	lat      config.LatencyProfile
	gen      config.GenerationProfile
	errs     config.ErrorProfile
	outages  [4][]config.Outage // errors.outages of root, model, key, header profile
	chaos    chaos.Decision
	scn      *scenario.Resolved
	rng      *generate.RNG
	idRNG    *generate.RNG // ids only, so minting never shifts content
	seed     int64
	testID   string
	t0       time.Time
	speed    float64
	prompt   generate.PromptBreakdown
	// tk counts and splits the model's text (models[].tokenizer, ADR
	// 0053); text generates prose with it.
	tk   generate.Tokenizer
	text *generate.Generator
	// sentBreakdown is the x-wwz-input-breakdown value the response carries
	// (nil without the header); it goes to fields.input_breakdown (ADR 0032).
	sentBreakdown *limits.InputBreakdown
	estOut        int
	stall         *scenario.Stall
	cutAfter      int
	finishErrKind core.ErrorKind
	status        int
	overrides     overrides
	// pc is the prompt-cache outcome (nil: no cache for this request).
	pc *pcState
	// rs is the Responses API state (nil on the other endpoints).
	rs *respState
	// delivered is the reply a finish_length cut sent (nil: all of resp).
	delivered *core.Response
}

type overrides struct {
	response      string
	toolCalls     []core.ToolCall
	hasTools      bool
	returnErr     string
	delayMS       int
	toolProb      float64
	hasToolProb   bool
	contentFilter bool
	// forceFilter: x-wwz-return-error: content_filter on a driver that
	// filters with a 200; no drawn error replaces it.
	forceFilter bool
	seed        int64
	chaos       chaosOverrides
}

// chaosOverrides are the x-wwz-chaos-* headers (spec 009 "Camadas", ADR
// 0027 I2). Unset fields are nil; invalid values never get here.
type chaosOverrides struct {
	profile        string
	rate           *float64
	types          []string
	midStreamRate  *float64
	midStreamTypes []string
}

// apply layers the headers over the resolved error profile p (header > step
// > key > model > global). It reports whether anything changed p.
func (c chaosOverrides) apply(cfg *config.Config, p *config.ErrorProfile) bool {
	applied := false
	if c.profile != "" {
		if np, ok := cfg.Profiles.Errors[c.profile]; ok {
			*p = np
			applied = true
		}
	}
	if c.rate != nil {
		p.Rate, applied = *c.rate, true
	}
	if c.types != nil {
		p.Types, applied = c.types, true
	}
	if c.midStreamRate != nil {
		p.MidStreamRate, applied = *c.midStreamRate, true
	}
	if c.midStreamTypes != nil {
		p.MidStreamTypes, applied = c.midStreamTypes, true
	}
	return applied
}

// parseChaosRate accepts a percentage in [0, 100].
func parseChaosRate(v string) *float64 {
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || !(f >= 0 && f <= 100) {
		return nil
	}
	return &f
}

// parseChaosTypes splits a comma-separated list keeping the names valid
// tells apart; nil when nothing valid is left.
func parseChaosTypes(v string, valid func(string) bool) []string {
	var out []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" && valid(t) {
			out = append(out, t)
		}
	}
	return out
}

func validMidType(t string) bool {
	return chaos.ValidateProfile(&config.ErrorProfile{MidStreamTypes: []string{t}}) == nil
}

func parseChaosOverrides(h driver.MapHeaders) chaosOverrides {
	return chaosOverrides{
		profile:        strings.TrimSpace(h.Get("x-wwz-chaos-profile")),
		rate:           parseChaosRate(h.Get("x-wwz-chaos-rate")),
		types:          parseChaosTypes(h.Get("x-wwz-chaos-types"), chaos.ProfileType), // what errors.types accepts
		midStreamRate:  parseChaosRate(h.Get("x-wwz-chaos-mid-stream-rate")),
		midStreamTypes: parseChaosTypes(h.Get("x-wwz-chaos-mid-stream-types"), validMidType),
	}
}

// Handle serves one request. w must implement Streamer for streaming
// responses; MemWriter does.
func (e *Engine) Handle(ctx context.Context, in Input, w core.ResponseWriter) {
	cfg := e.Config()
	drv := in.Driver
	e.M.InFlight.Inc(drv.Name())
	defer e.M.InFlight.Dec(drv.Name())
	t0 := time.Now()
	// The strict client verification comes before everything (spec 020,
	// ADR 0074, item 7): a forged client is refused before its credential,
	// body and limits are looked at.
	if v, ok := driver.ClientVerifierOf(drv, in.Kind); ok {
		head, _ := driver.RawHeadOf(in.Headers)
		if er := v.VerifyClient(in.Kind, head, in.Body); er != nil {
			e.refuseForged(ctx, in, w, er, t0)
			return
		}
	}
	at := e.now() // the start of the request prices it (ADR 0072, item 15)

	// Credentials first: every route needs them when auth is on.
	cred := resolveToken(cfg, drv, drv.Authenticate(in.Headers), at)
	key, authErr := e.authenticate(cfg, drv, cred)
	if in.Headers == nil {
		in.Headers = driver.MapHeaders{}
	}
	authErr = e.checkToken(cfg, in, cred, authErr) // expired, beta, scope (ADR 0074)
	driver.SetAccount(in.Headers, accountOf(cred, key))
	// The catalog of the route (driver.CatalogRouter, ADR 0072): the
	// listing, the model accepted and its price; the driver reads it back.
	cat := catalogOf(in)
	driver.SetCatalog(in.Headers, cat)
	// bare is what errors raised before the body is parsed see: the headers
	// (presented key and account), no body fields.
	// The endpoint picks the envelope (OpenRouter answers the 401 of
	// /api/v1/messages in Anthropic's, auth-order/badkey_messages_*).
	bare := &core.Request{Headers: in.Headers, ClientIP: in.ClientIP}
	if in.Kind == driver.KindMessages {
		bare.Endpoint = core.EndpointMessages
	}

	switch in.Kind {
	case driver.KindRouteMiss:
		// Unknown route or wrong method of a driver that answers them
		// itself; it decides where the key comes in (task 235).
		status := 404
		if len(in.Miss.Allow) > 0 {
			status = 405
		}
		if rm, ok := driver.As[driver.RouteMisser](drv); ok {
			rm.WriteRouteMiss(w, bare, in.Miss, authErr)
		} else {
			drv.WriteError(w, bare, &core.Error{Kind: core.ErrNotFound, Status: status, Message: "Not Found"}, nil)
		}
		e.count(drv, "", "route_miss", status)
		return
	case driver.KindModels, driver.KindModel:
		if publicModels(drv, in.Kind, cred, in.PathParam) {
			authErr = nil
		}
		if authErr != nil {
			e.count(drv, "", string(in.Kind), e.writeError(w, drv, bare, authErr, nil))
			return
		}
		models := e.modelsIn(drv.Protocol(), cat)
		if in.Kind == driver.KindModels {
			drv.ListModels(w, bare, models)
			e.count(drv, "", "models", 200)
			return
		}
		found := findListed(models, drv, in.Path, in.PathParam)
		drv.GetModel(w, bare, found, listedParam(drv, in.Path, in.PathParam))
		if found == nil {
			e.count(drv, "", "model", 404) // never a label from client text (ADR 0040)
		} else {
			e.count(drv, found.Cfg.ID, "model", 200)
		}
		return
	case driver.KindKeyInfo, driver.KindUsage, driver.KindEmbeddings, driver.KindResource:
		// Several auxiliary routes share a Kind (OpenRouter /key, /credits,
		// /keys): the driver tells them apart by the route.
		driver.SetRoute(in.Headers, in.Method, in.Path)
		if driver.IsPublicRoute(drv, driver.Route{Method: in.Method, Path: in.Path, Kind: in.Kind}) {
			authErr = nil // answered without a credential (OpenCode's device flow)
		}
		if authErr != nil {
			drv.WriteError(w, bare, authErr, nil)
			return
		}
		xc := driver.ExtraContext{Key: key, Limits: cfg.LimitsFor(key, e.accountTier(key)).Account.Limits, Models: e.modelsIn(drv.Protocol(), cat), Now: at, Keys: keyLookup(cfg)}
		if in.Kind == driver.KindKeyInfo || in.Kind == driver.KindUsage {
			// The key routes report spend and credits; OpenCode's usage
			// route the account's quota windows (task 254).
			xc.KeyBalance, xc.AccountBalance = e.balances(ctx, cfg, key)
		}
		// The quota owner, model quotas and budgets of a driver that
		// decides the billing (ADR 0072): usage and Console.
		e.billingView(ctx, cfg, in, cat, bare, cred, key, &xc)
		drv.Extra(in.Kind, w, in.Headers, in.Body, xc)
		e.count(drv, "", string(in.Kind), 200)
		return
	}

	if cfg.Server.MaxRequestBodyMB > 0 && len(in.Body) > cfg.Server.MaxRequestBodyMB*1024*1024 {
		er := &core.Error{Kind: core.ErrRequestTooLarge, Message: "Request entity too large"}
		drv.WriteError(w, bare, er, nil)
		e.count(drv, "", string(in.Kind), 413)
		return
	}

	// Check order (spec 006 16.7): providers with AuthFirst refuse a bad key
	// before they look at the body; the others (OpenCode Go) check the
	// required headers, the body and the model, and the key last.
	conv := driver.ConventionsOf(drv)
	req, perr := drv.ParseRequest(in.Kind, in.Body, in.Headers)
	if perr != nil {
		switch {
		case conv.AuthFirst && authErr != nil && !driver.BeforeAuth(perr):
			perr = authErr
		case !conv.AuthFirst && len(cred.Missing) > 0:
			perr = missingError(cred)
		default:
			// Checks the provider makes after it resolved the model: an
			// unknown model is answered first (Gemini, task 235).
			if name, ok := driver.AfterModel(perr); ok {
				// Only drivers without ModelNamer mark ExtraAfterModel, so
				// resolveModel never fails here.
				if m, _, _ := resolveModel(cfg, drv, name); !e.servable(m, drv, in.Kind, cat) {
					perr = e.modelNotFound(m)
				}
			}
		}
		e.count(drv, "", string(in.Kind), e.writeError(w, drv, bare, perr, nil))
		return
	}
	req.Headers = in.Headers
	req.ClientIP = in.ClientIP
	req.RequestID = e.newID()
	if m, ok := drv.(driver.IDMinter); ok {
		req.RequestID = m.RequestID(in.Kind, req.RequestID)
	}
	req.Headers["x-wwz-path"] = in.Path

	r := &request{in: in, decl: driver.EngineDeclOf(drv), req: req, cfg: cfg, key: key, cred: cred, anon: anonymous(drv, cred), t0: t0, speed: 1, tk: simulated, text: simulatedText}
	r.testID = in.Headers.Get(strings.ToLower(cfg.Chaos.ScopeHeader))
	if conv.AuthFirst && authErr != nil {
		e.fail(r, w, authErr)
		return
	}
	if len(cred.Missing) > 0 {
		e.fail(r, w, missingError(cred))
		return
	}
	if authErr != nil {
		// The provider checks the model before the key: an unknown model
		// is refused even with an invalid key.
		if m, name, verr := resolveModel(cfg, drv, req.Model); !e.servable(m, drv, in.Kind, cat) {
			req.Model = name
			if verr == nil {
				verr = &core.Error{Kind: core.ErrModelNotFound}
			}
			e.fail(r, w, verr)
			return
		}
		e.fail(r, w, authErr)
		return
	}
	// Per-request overrides; without request headers they keep their
	// "absent" values (delayMS -1), so the profile's latency stays.
	r.overrides = overrides{delayMS: -1}
	if cfg.Server.RequestHeaders == nil || *cfg.Server.RequestHeaders {
		r.overrides = parseOverrides(in.Headers)
	}
	if r.overrides.returnErr == string(core.ErrContentFilter) && r.decl.ContentFilterAsFinish {
		// x-wwz-return-error follows the type table (ADR 0070): on these
		// drivers content_filter is the same 200, ended filtered, as
		// errors.types: [content_filter] and x-wwz-content-filter.
		r.overrides.contentFilter, r.overrides.forceFilter = true, true
	} else if r.overrides.returnErr != "" {
		er := chaos.BuildError(r.overrides.returnErr, nil)
		er.Message = "requested via x-wwz-return-error: " + er.Message
		e.fail(r, w, er)
		return
	}

	// Model.
	model, name, verr := resolveModel(cfg, drv, req.Model)
	req.Model = name // scenarios see the name asked for (no slug prefix); labels use r.model.ID (ADR 0040)
	if verr != nil {
		e.fail(r, w, verr)
		return
	}
	if !e.servable(model, drv, in.Kind, cat) {
		e.fail(r, w, e.modelNotFound(model))
		return
	}
	// The model effective in the route's catalog (ADR 0072, item 16):
	// context window, output cap, capabilities and price of that catalog.
	model = model.ForCatalog(cat)
	r.model = model
	r.text = generatorOf(model, r.decl) // task 265: Gemini calibration
	r.tk = r.text.Tokenizer
	if key != nil && len(key.Models) > 0 && !contains(key.Models, model.ID) {
		e.fail(r, w, &core.Error{Kind: core.ErrModelNotFound, Message: "This key does not have access to model " + model.ID})
		return
	}
	var err error
	if r.lat, err = cfg.LatencyFor(model); err != nil {
		e.fail(r, w, &core.Error{Kind: core.ErrServerError, Message: err.Error()})
		return
	}
	if r.gen, err = cfg.GenerationFor(model); err != nil {
		e.fail(r, w, &core.Error{Kind: core.ErrServerError, Message: err.Error()})
		return
	}
	if r.errs, err = cfg.ErrorsFor(model); err != nil {
		e.fail(r, w, &core.Error{Kind: core.ErrServerError, Message: err.Error()})
		return
	}
	// errors.outages windows add up across levels (spec 009): root, model,
	// key and the profile named by x-wwz-chaos-profile.
	r.outages[0], r.outages[1] = cfg.Errors.Outages, r.errs.Outages
	// The key's profiles beat the model's (ADR 0026: header > step > key >
	// model > global); the whole profile is replaced, never merged.
	if err = keyProfiles(cfg, key, &r.lat, &r.errs); err != nil {
		e.fail(r, w, &core.Error{Kind: core.ErrServerError, Message: err.Error()})
		return
	}
	r.outages[2] = r.errs.Outages
	// x-wwz-chaos-* headers beat key, model and global (ADR 0027, I2).
	chaosHeaders := r.overrides.chaos.apply(cfg, &r.errs)
	r.outages[3] = r.errs.Outages
	// Seed and RNG.
	r.seed = cfg.Server.Seed
	if r.overrides.seed != 0 {
		r.seed = r.overrides.seed
	}
	convID := scenario.ConversationID(req, "x-wwz-conversation-id")
	r.rng = generate.NewRNG(r.seed, "req", req.RequestID, convID)
	if r.seed != 0 {
		// Deterministic per conversation turn rather than per request id.
		r.rng = generate.NewRNG(r.seed, "conv", convID, strconv.Itoa(len(req.Messages)))
	}
	r.idRNG = generate.NewRNG(r.seed, "ids", req.RequestID, convID)
	if r.seed != 0 {
		r.idRNG = generate.NewRNG(r.seed, "ids", convID, strconv.Itoa(len(req.Messages)))
	}

	// Prompt accounting.
	r.prompt = countPrompt(r.tk, req, r.decl.Template)
	if er := e.responsesPrevious(ctx, r); er != nil {
		e.fail(r, w, er)
		return
	}
	if r.prompt.Total > model.ContextWindow {
		e.fail(r, w, &core.Error{Kind: core.ErrContextLength, Message: fmt.Sprintf("This model's maximum context length is %d tokens. However, your messages resulted in %d tokens. Please reduce the length of the messages.", model.ContextWindow, r.prompt.Total)})
		return
	}
	// The effective price (ADR 0072, item 15): the route's catalog, the
	// overrides and multipliers at the start of the request and its input
	// tokens; every cost of the request reads r.model.Pricing.
	model = pricedModel(model, "", at, r.prompt.Total) // already effective in cat
	r.model = model
	// Capability checks (as the provider would).
	if len(req.Tools) > 0 && model.Capabilities.Tools != nil && !*model.Capabilities.Tools {
		e.fail(r, w, &core.Error{Kind: core.ErrInvalidRequest, Message: "This model does not support tools.", Param: "tools"})
		return
	}
	// The billing origin (interface (d), ADR 0072): the driver may refuse
	// here, before Check, and then nothing is counted in the store.
	if bd, ok := billingDecider(drv); ok {
		b, berr := bd.Billing(e.billingQuery(cfg, in, cat, req, cred, key, model))
		if berr != nil {
			e.fail(r, w, berr)
			return
		}
		r.billing = &b
		driver.SetBilling(in.Headers, b)
	}

	// Scenario.
	if sel, serr := e.Scenarios.Select(req, cfg.Scenarios.Select, model.Scenarios); serr != nil {
		e.fail(r, w, &core.Error{Kind: core.ErrInvalidRequest, Message: serr.Error(), Param: "scenario"})
		return
	} else if sel.Scenario != nil {
		rv := &scenario.Resolver{Tokenizer: r.tk, ArgOptions: argOptions(r.gen), GlobalSeed: r.seed, Attempts: func(c, s string) int {
			n, _ := e.Store.Attempts(ctx, c, s, time.Hour)
			return n
		}}
		res, rerr := rv.Resolve(sel, req, convID)
		if rerr != nil {
			if ce, ok := rerr.(*core.Error); ok {
				e.fail(r, w, ce)
			} else {
				e.fail(r, w, &core.Error{Kind: core.ErrServerError, Message: rerr.Error()})
			}
			return
		}
		if !res.Fallback {
			r.scn = res
			if res.Speed > 0 {
				r.speed = res.Speed
			}
			if sel.Speed > 0 {
				r.speed = sel.Speed
			}
		}
	}

	// Limits.
	r.subjects = e.billedSubjects(cfg, drv, key, req, model, cat, r.billing)
	if r.billing == nil {
		if fs, ok := e.freeModelsSubject(ctx, cfg, drv, key, model); ok {
			r.subjects = append(r.subjects, fs)
		}
	}
	est := r.estimateOutput()
	r.estOut = est
	reserve := est
	if drv.LimitPolicy().TPMReserve == "none" {
		reserve = 0
	}
	// The prompt cache is read before Check: the cached tokens enter the
	// itpm reservation (cache/valkey/README.md 2.4).
	e.promptCacheLookup(ctx, r)
	cached := 0
	if r.pc != nil {
		cached = r.pc.read
	}
	e.subscriptionView(ctx, r) // the quota windows of a subscription's token (ADR 0074)
	dec, lerr := e.Store.Check(ctx, limits.CheckRequest{Subjects: r.subjects, EstPromptTokens: r.prompt.Total, EstOutputTokens: reserve, CachedTokens: cached, Now: time.Now(), TestID: r.testID, RequestID: req.RequestID})
	if lerr == nil && extraUsage(r.billing, dec) {
		// Extra Usage (ADR 0072, item 12): the subscription quota ran out,
		// so the request goes on billed to the balance. The denial consumed
		// nothing; check again with the balance subjects.
		b := *r.billing
		b.Source = driver.BillingBalance
		r.billing = &b
		driver.SetBilling(in.Headers, b)
		r.subjects = e.billedSubjects(cfg, drv, key, req, model, cat, r.billing)
		dec, lerr = e.Store.Check(ctx, limits.CheckRequest{Subjects: r.subjects, EstPromptTokens: r.prompt.Total, EstOutputTokens: reserve, CachedTokens: cached, Now: time.Now(), TestID: r.testID, RequestID: req.RequestID})
	}
	if lerr != nil {
		e.Log.Warn("limits store error", "err", lerr)
		if cfg.Store.OnDown == "deny" {
			e.fail(r, w, &core.Error{Kind: core.ErrOverloaded, Message: "control store unavailable"})
			return
		}
		dec = limits.Decision{Allowed: true}
	}
	r.rl = dec.State
	if !dec.Allowed && dec.Queue {
		// Queue mode: wait and retry once.
		done := e.M.waiting(drv.Name(), model.ID)
		ok := sleepCtx(ctx, dec.RetryAfter/2)
		done()
		if !ok {
			return
		}
		dec, _ = e.Store.Check(ctx, limits.CheckRequest{Subjects: r.subjects, EstPromptTokens: r.prompt.Total, EstOutputTokens: reserve, CachedTokens: cached, Now: time.Now(), TestID: r.testID, RequestID: req.RequestID})
		r.rl = dec.State
	}
	if !dec.Allowed {
		er := denialError(dec, r.prompt.Total, reserve)
		if dec.Kind == core.ErrAccountBanned || dec.Kind == core.ErrAccountSuspended {
			e.M.Blocked.Inc(drv.Name(), string(dec.Kind))
		} else {
			e.M.RateLimited.Inc(drv.Name(), dec.LimitName)
		}
		e.recordStrike(ctx, r, dec)
		r.finishErrKind = dec.Kind
		e.failNoFinish(r, w, er)
		e.finishDenied(ctx, r, dec)
		return
	}
	// From here on Finish must run.
	// Chaos.
	r.chaos = e.Injector.DecideQuery(chaos.Query{Profile: &r.errs, Model: model.ID, Driver: drv.Name(), TestID: r.testID, Outages: r.outages[:]}, r.rng, e.now())
	if chaosHeaders && r.chaos.Source == "profile" && !r.chaos.Outage {
		r.chaos.Source = "header"
	}
	// Precedence (spec 017 I2, ADR 0027): an admin decision (one-shot,
	// test-scoped event, SetScoped profile or global event) or an error drawn
	// from the x-wwz-chaos-* headers wins over the scenario step; the step
	// wins over the key's, model's and global error profile.
	if r.scn != nil && r.scn.Error != nil && (r.chaos.Pre == nil || (r.chaos.Source != "admin" && r.chaos.Source != "header")) {
		r.chaos.Pre = r.scn.Error
		r.chaos.Source = "step"
	}
	if r.overrides.forceFilter {
		// The header forces the filtered reply, without a draw (spec 009).
		r.chaos.Pre = nil
	}
	if r.chaos.Pre != nil && r.chaos.Pre.Kind == core.ErrContentFilter && r.decl.ContentFilterAsFinish {
		// The driver filters content with a 200 that ends in finish_reason
		// content_filter / stop_reason refusal (specs 006, 009); the other
		// drivers write their own reply for the error.
		e.M.ErrorsInjected.Inc(drv.Name(), string(core.ErrContentFilter), "pre")
		r.chaos.Pre, r.chaos.Moderation = nil, &contentFilterModeration
	}
	if r.chaos.Pre != nil {
		e.M.ErrorsInjected.Inc(drv.Name(), string(r.chaos.Pre.Kind), "pre")
		r.finishErrKind = r.chaos.Pre.Kind
		e.failAndFinish(ctx, r, w, r.chaos.Pre)
		return
	}
	// A step timeline (I14) defines the reply; fold it into the step so the
	// plan, usage and the non-streaming body match what the stream replays.
	e.foldTimeline(r)
	// Generate the response plan.
	e.plan(r)
	r.responsesUsage()
	r.applyPromptCache()
	if n, ok := drv.(driver.ModelNamer); ok {
		r.resp.Model = n.ModelName(model) // ADR 0038: the provider's name for the model
	}
	// Latency plan.
	tr := e.tracker(model.ID)
	active := tr.Enter()
	if r.lat.Concurrency > 0 && r.lat.MaxQueue > 0 && active > int64(r.lat.Concurrency+r.lat.MaxQueue) {
		tr.Leave()
		er := &core.Error{Kind: core.ErrOverloaded, Message: "Model queue is full", RetryAfter: time.Second}
		r.finishErrKind = er.Kind
		e.failAndFinish(ctx, r, w, er)
		return
	}
	leave := e.M.occupy(tr, drv.Name(), model.ID, r.lat.Concurrency > 0 && active > int64(r.lat.Concurrency))
	latProf := r.lat
	if r.scn != nil && r.scn.Latency != nil {
		latProf = mergeLatency(r.lat, *r.scn.Latency)
	}
	if r.overrides.delayMS >= 0 && r.overrides.hasDelay() {
		d := time.Duration(r.overrides.delayMS) * time.Millisecond
		latProf = config.LatencyProfile{ITL: &config.Dist{Kind: "fixed", Value: d}, TTFT: &config.Dist{Kind: "fixed", Value: d}}
	}
	// The tokens read from the prompt cache shorten the prefill (spec 008).
	r.plan = latency.BuildCached(&latProf, r.rng, r.prompt.Total, r.resp.Usage.CachedTokens, active-1, r.speed)
	r.setInputBreakdown() // I16
	e.M.TTFT.Observe((r.plan.Queue + r.plan.TTFT).Seconds(), drv.Name(), model.ID)
	if e.M.VLLMTTFT != nil {
		e.M.VLLMTTFT.Observe((r.plan.Queue + r.plan.TTFT).Seconds(), model.ID)
	}
	e.M.ITL.Observe(r.plan.ITL.Seconds(), drv.Name(), model.ID)
	e.M.Overhead.Observe(time.Since(t0).Seconds(), drv.Name())

	if req.Stream {
		st, ok := w.(Streamer)
		if !ok {
			leave()
			e.failAndFinish(ctx, r, w, &core.Error{Kind: core.ErrServerError, Message: "transport cannot stream"})
			return
		}
		e.M.Streams.Inc(drv.Name())
		st.Stream(func(sw core.ResponseWriter) {
			defer leave()
			defer e.M.Streams.Dec(drv.Name())
			e.stream(ctx, r, sw)
			e.finish(ctx, r)
		})
		return
	}
	defer leave()
	e.nonStream(ctx, r, w)
	e.finish(ctx, r)
}

// contentFilterModeration is the moderation a content_filter error type
// becomes on the drivers that declare ContentFilterAsFinish (a successful
// reply, finish_reason content_filter or stop_reason refusal, rather than
// the HTTP error the others write): the reply is cut and ends filtered.
var contentFilterModeration = config.Moderation{Probability: 100, Mode: "finish_reason"}

// keyProfiles replaces lat and errs with the profiles named by the key, if
// any (keys[].latency and keys[].errors name entries of profiles.*).
func keyProfiles(cfg *config.Config, key *config.KeyConfig, lat *config.LatencyProfile, errs *config.ErrorProfile) error {
	if key == nil {
		return nil
	}
	if key.Latency != "" {
		p, ok := cfg.Profiles.Latency[key.Latency]
		if !ok {
			return fmt.Errorf("key %q: unknown latency profile %q", key.Label, key.Latency)
		}
		*lat = p
	}
	if key.Errors != "" {
		p, ok := cfg.Profiles.Errors[key.Errors]
		if !ok {
			return fmt.Errorf("key %q: unknown errors profile %q", key.Label, key.Errors)
		}
		*errs = p
	}
	return nil
}

func (o overrides) hasDelay() bool { return o.delayMS >= 0 }

func parseOverrides(h driver.MapHeaders) overrides {
	o := overrides{delayMS: -1}
	if v := h.Get("x-wwz-response-base64"); v != "" {
		if b, err := decodeB64(v); err == nil {
			o.response = string(b)
		}
	} else if v := h.Get("x-wwz-response"); v != "" {
		o.response = v
	}
	if v := h.Get("x-wwz-tool-calls"); v != "" {
		var tcs []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Function  *struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal([]byte(v), &tcs) == nil {
			o.hasTools = true
			for i, tc := range tcs {
				name, args := tc.Name, tc.Arguments
				if tc.Function != nil {
					name, args = tc.Function.Name, tc.Function.Arguments
				}
				var s string
				argStr := string(args)
				if json.Unmarshal(args, &s) == nil {
					argStr = s
				}
				if argStr == "" {
					argStr = "{}"
				}
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("call_hdr_%d", i)
				}
				o.toolCalls = append(o.toolCalls, core.ToolCall{ID: id, Name: name, Arguments: argStr})
			}
		}
	}
	o.returnErr = h.Get("x-wwz-return-error")
	if v := h.Get("x-wwz-delay-ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			o.delayMS = n
		}
	}
	if v := h.Get("x-wwz-tool-prob"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if f <= 1 {
				f *= 100
			}
			o.toolProb, o.hasToolProb = f, true
		}
	}
	if v := h.Get("x-wwz-content-filter"); v == "1" || v == "true" {
		o.contentFilter = true
	}
	if v := h.Get("x-wwz-seed"); v != "" {
		o.seed, _ = strconv.ParseInt(v, 10, 64)
	}
	o.chaos = parseChaosOverrides(h)
	return o
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func argOptions(g config.GenerationProfile) toolargs.Options {
	o := toolargs.DefaultOptions()
	if tc := g.ToolCalls; tc != nil {
		o.OptionalProbability = tc.OptionalParamProbability
		o.MinInt, o.MaxInt = tc.MinInt, tc.MaxInt
		o.MinNumber, o.MaxNumber = tc.MinNumber, tc.MaxNumber
		o.MinArrayLen, o.MaxArrayLen = tc.MinArrayLen, tc.MaxArrayLen
	}
	return o
}

func mergeLatency(base, over config.LatencyProfile) config.LatencyProfile {
	if over.Queue != nil {
		base.Queue = over.Queue
	}
	if over.TTFT != nil {
		base.TTFT = over.TTFT
	}
	if over.ITL != nil {
		base.ITL = over.ITL
	}
	if over.ReasoningITL != nil {
		base.ReasoningITL = over.ReasoningITL
	}
	if over.ToolCallDelay != nil {
		base.ToolCallDelay = over.ToolCallDelay
	}
	if over.ToolArgsITL != nil {
		base.ToolArgsITL = over.ToolArgsITL
	}
	if over.FinishDelay != nil {
		base.FinishDelay = over.FinishDelay
	}
	if over.LoadFactor != 0 {
		base.LoadFactor = over.LoadFactor
	}
	if over.ChunkTokens != 0 {
		base.ChunkTokens = over.ChunkTokens
	}
	if over.Pacing != nil {
		base.Pacing = over.Pacing
	}
	if over.Clock != "" {
		base.Clock = over.Clock
	}
	return base
}

func (e *Engine) authenticate(cfg *config.Config, drv driver.Driver, cred driver.Credential) (*config.KeyConfig, *core.Error) {
	if anonymous(drv, cred) {
		// No account (OpenCode public, ADR 0072): never a keys[] entry,
		// whatever server.auth says; the driver's billing decides.
		return nil, nil
	}
	if cfg.Server.Auth == "none" {
		if cred.Key != "" {
			if k := credKey(cfg, cred); k != nil {
				return k, nil
			}
		}
		return nil, nil
	}
	if cred.Key == "" {
		return nil, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."}
	}
	k := credKey(cfg, cred)
	if k == nil {
		return nil, &core.Error{Kind: core.ErrInvalidAPIKey}
	}
	switch k.Status {
	case "banned", "revoked", "deactivated":
		return nil, &core.Error{Kind: core.ErrAccountBanned}
	case "suspended":
		return nil, &core.Error{Kind: core.ErrAccountSuspended}
	case "expired":
		return nil, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "This API key has expired."}
	}
	if k.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, k.ExpiresAt); err == nil && time.Now().After(t) {
			return nil, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "This API key has expired."}
		}
	}
	if k.Driver != "" && k.Driver != drv.Name() {
		return nil, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "This API key is not valid for this endpoint."}
	}
	return k, nil
}

// credKey is the keys[] entry of cred: the one its KeyLabel names (a
// token a KeyResolver resolved), else the one whose key it presents.
func credKey(cfg *config.Config, cred driver.Credential) *config.KeyConfig {
	if cred.KeyLabel != "" {
		return keyLookup(cfg)(cred.KeyLabel)
	}
	return cfg.FindKey(cred.Key)
}

// keyLookup finds the keys[] entries of cfg by label.
func keyLookup(cfg *config.Config) driver.KeyLookup {
	return func(label string) *config.KeyConfig {
		for i := range cfg.Keys {
			if cfg.Keys[i].Label == label {
				return &cfg.Keys[i]
			}
		}
		return nil
	}
}

// resolveToken asks a driver.KeyResolver whether a presented key that
// matches no keys[] entry is a token standing for one (OpenCode's Console
// access token, ADR 0072): if so, cred carries the label of that entry.
func resolveToken(cfg *config.Config, drv driver.Driver, cred driver.Credential, now time.Time) driver.Credential {
	if cred.Key == "" || cred.Anonymous || cfg.FindKey(cred.Key) != nil {
		return cred
	}
	if r, ok := driver.As[driver.KeyResolver](drv); ok {
		if label, ok := r.ResolveKey(cred.Key, keyLookup(cfg), now); ok {
			cred.KeyLabel = label
		}
	}
	return cred
}

// accountOf is the Account the driver sees: the key as presented and the
// account and project of its keys[] entry (ADR 0049). It is the one place
// that decides where account and project come from.
func accountOf(cred driver.Credential, key *config.KeyConfig) driver.Account {
	a := driver.Account{Key: cred.Key}
	if key != nil {
		a.Account, a.Project = key.Account, key.Project
	}
	return a
}

// subjects are the entities a request is counted on (ADR 0051): every limit
// of the tier counts on the account; the project and the key count only the
// ceilings declared for them (projects[].limits, keys[].limits) and
// otherwise carry only block and ban, like ip and user. A driver without the
// account level (none today) counts the tier on the key, under the key's
// own ceiling.
func (e *Engine) subjects(cfg *config.Config, drv driver.Driver, key *config.KeyConfig, req *core.Request, model *config.ModelConfig) []limits.Subject {
	pol := drv.LimitPolicy()
	subs := make([]limits.Subject, 0, len(pol.Entities))
	eff := cfg.LimitsFor(key, e.accountTier(key))
	if !contains(pol.Entities, "account") && eff.Account.ID != "" {
		eff.Key.Limits = underCeiling(eff.Account.Limits, eff.Key.Limits)
		eff.Account = config.LevelLimits{}
	}
	var dep config.Limits
	if contains(pol.Entities, "deployment") {
		eff.Account.Limits, dep = splitDeployment(eff.Account.Limits)
	}
	for _, ent := range pol.Entities {
		var id string
		var l config.Limits
		var window time.Duration
		switch ent {
		case "deployment":
			id, l, window = deploymentSubject(drv, eff, model, dep)
		case "ip":
			id = req.ClientIP
		case "key":
			id, l = eff.Key.ID, eff.Key.Limits
		case "account":
			id, l = eff.Account.ID, eff.Account.Limits
		case "project":
			id, l = eff.Project.ID, eff.Project.Limits
		case "user":
			if req.Metadata != nil {
				id = req.Metadata["user"]
				if id == "" {
					id = req.Metadata["user_id"]
				}
			}
			if id == "" {
				id = req.Header("x-opencode-session")
			}
		}
		if id == "" {
			continue
		}
		// The driver's TPMReserve and ExcludeCacheRead hold on every level
		// with a token limit.
		if pol.TPMReserve == "none" && l.TPMReserve == "" {
			l.TPMReserve = "none"
		}
		if pol.ExcludeCacheRead {
			l.ExcludeCacheRead = true
		}
		subs = append(subs, limits.Subject{Entity: limits.Entity{Kind: limits.EntityKind(ent), ID: id}, Limits: l, Pricing: model.Pricing, Window: window})
	}
	// Model-level concurrency comes from the latency profile.
	return subs
}

// deploymentLimiter is a driver whose provider limits each deployment of a
// model (Azure OpenAI, spec 006 16.6): given the model and the tier's
// count limits, it returns the deployment's limits (RPM derived from TPM)
// and the short window the requests are evaluated over.
type deploymentLimiter interface {
	DeploymentLimits(model *config.ModelConfig, l config.Limits) (config.Limits, time.Duration)
}

// splitDeployment splits the tier limits of an account on a driver that
// limits per deployment: the count limits (requests, tokens, concurrency
// and how they are counted) go to each deployment, the dollar limits
// (spend, credits, quota) and the mode stay on the account. Every
// deployment of the account gets the tier's count limits, as the quota
// assigned to a deployment becomes its limit.
func splitDeployment(l config.Limits) (account, dep config.Limits) {
	account = config.Limits{
		SpendMonthlyUSD: l.SpendMonthlyUSD, SpendSoftUSD: l.SpendSoftUSD, CreditsUSD: l.CreditsUSD, QuotaUSD: l.QuotaUSD,
		Mode: l.Mode, AfterN: l.AfterN, Window: l.Window,
	}
	dep = l
	dep.SpendMonthlyUSD, dep.SpendSoftUSD, dep.CreditsUSD, dep.QuotaUSD = 0, 0, nil, nil
	return account, dep
}

// deploymentSubject is the deployment entity of a request: id
// "<account>/<model id>" (a key without keys[].account has its implicit
// account), the tier's count limits shaped by the driver, and the
// evaluation window. id is "" without an account (no key).
func deploymentSubject(drv driver.Driver, eff config.EffectiveLimits, model *config.ModelConfig, l config.Limits) (string, config.Limits, time.Duration) {
	owner := eff.Account.ID
	if owner == "" {
		return "", config.Limits{}, 0
	}
	var window time.Duration
	if dl, ok := driver.As[deploymentLimiter](drv); ok {
		l, window = dl.DeploymentLimits(model, l)
	}
	return owner + "/" + model.ID, l, window
}

// freeModelLimiter is a driver whose provider caps the requests to its free
// model variants per account, apart from the account's own limits
// (OpenRouter's ":free"; spec 006 16.4, ADR 0060). creditsUSD is what the
// account got: the configured credits_usd plus its purchases.
type freeModelLimiter interface {
	FreeModelLimits(creditsUSD float64) (rpm, rpd int)
}

// freeModelsSubject is the account's free-models unit for a request to a
// free variant (models[].id ending in ":free") on a driver with such caps:
// its own entity (limits.KindFreeModels, id of the account), so the caps
// count only those requests and the account's tier limits still apply.
// The daily cap reads the account's purchases from the store unless the
// configured credits already reach the highest cap.
func (e *Engine) freeModelsSubject(ctx context.Context, cfg *config.Config, drv driver.Driver, key *config.KeyConfig, model *config.ModelConfig) (limits.Subject, bool) {
	fl, ok := driver.As[freeModelLimiter](drv)
	if !ok || key == nil || model == nil || !limits.FreeModel(model.ID) {
		return limits.Subject{}, false
	}
	acct := cfg.LimitsFor(key, e.accountTier(key)).Account
	if acct.ID == "" {
		return limits.Subject{}, false
	}
	credits := 0.0
	if acct.Limits.CreditsUSD != nil {
		credits = *acct.Limits.CreditsUSD
	}
	rpm, rpd := fl.FreeModelLimits(credits)
	if _, top := fl.FreeModelLimits(math.MaxFloat64); rpd < top {
		st, err := e.Store.Get(ctx, limits.Entity{Kind: limits.KindAccount, ID: acct.ID})
		if err != nil {
			e.Log.Warn("limits store error", "err", err)
		} else {
			rpm, rpd = fl.FreeModelLimits(credits + st.PurchasedUSD)
		}
	}
	return limits.Subject{
		Entity:  limits.Entity{Kind: limits.KindFreeModels, ID: acct.ID},
		Limits:  config.Limits{RPM: rpm, RPD: rpd},
		Pricing: model.Pricing,
	}, true
}

// underCeiling is the tier limits l of an account with the key's ceiling c
// on the same entity: each count limit is the tighter of the two declared
// values. Dollar limits and modes stay those of the account.
func underCeiling(l, c config.Limits) config.Limits {
	for _, p := range [...]struct{ dst, v *int }{
		{&l.RPS, &c.RPS},
		{&l.Burst, &c.Burst},
		{&l.RPM, &c.RPM},
		{&l.RPD, &c.RPD},
		{&l.TPM, &c.TPM},
		{&l.ITPM, &c.ITPM},
		{&l.OTPM, &c.OTPM},
		{&l.TPD, &c.TPD},
		{&l.Concurrency, &c.Concurrency},
	} {
		if *p.v > 0 && (*p.dst == 0 || *p.v < *p.dst) {
			*p.dst = *p.v
		}
	}
	return l
}

func (r *request) estimateOutput() int {
	if r.req.MaxTokens > 0 {
		return r.req.MaxTokens
	}
	return 1024
}

// requestIDHeader is the x-request-id value: a driver-minted id ("req_…")
// as is, or "req_" + the engine's opaque id.
func requestIDHeader(id string) string {
	if strings.HasPrefix(id, "req_") {
		return id
	}
	return "req_" + id
}

// responseID is the response object id in the driver's format, or the
// request id for drivers that do not mint ids.
func (r *request) responseID(now time.Time) string {
	if m, ok := r.in.Driver.(driver.IDMinter); ok && r.idRNG != nil {
		return m.ResponseID(r.in.Kind, r.idRNG, now)
	}
	return r.req.RequestID
}

// toolCallID is a tool call id in the driver's format (call_…, toolu_01…).
func (r *request) toolCallID(i int) string {
	if m, ok := r.in.Driver.(driver.IDMinter); ok && r.idRNG != nil {
		return m.ToolCallID(r.in.Kind, r.idRNG)
	}
	return fmt.Sprintf("call_%s_%d", shortID(r.rng), i)
}

// endpoint is the endpoint label of metrics, events, logs and records: the
// route the request came through. The gateways (openrouter, opencode)
// translate a Responses request into a Chat one for the engine (task 240),
// so a Responses route is named by its kind, not by req.Endpoint (task 267).
func (r *request) endpoint() string {
	if r.in.Kind == driver.KindResponses {
		return string(core.EndpointResponses)
	}
	return string(r.req.Endpoint)
}

// modelLabel is the model label of metrics: models[].id of the resolved
// model, or "" when it did not resolve, so client text never makes a series
// (ADR 0040).
func (r *request) modelLabel() string {
	if r.model != nil {
		return r.model.ID
	}
	return ""
}

// eventModel is the model of events, logs and records: models[].id of the
// resolved model, or the name asked for when it did not resolve (ADR 0040).
func (r *request) eventModel() string {
	if r.model != nil {
		return r.model.ID
	}
	return r.req.Model
}

// newID returns a short unique id.
func (e *Engine) newID() string {
	n := e.counter.Add(1)
	return strconv.FormatInt(time.Now().UnixNano()/1000, 36) + strconv.FormatUint(n, 36)
}

// statusOf is the status of er: its own when set (the driver writes that
// one), else the kind's.
func (e *Engine) statusOf(er *core.Error) int {
	if er.Status > 0 {
		return er.Status
	}
	switch er.Kind {
	case core.ErrInvalidRequest, core.ErrContextLength, core.ErrContentFilter, core.ErrMissingSession:
		return 400
	case core.ErrInvalidAPIKey:
		return 401
	case core.ErrPermission, core.ErrGeoBlocked, core.ErrAccountSuspended, core.ErrAccountBanned, core.ErrForgedRequest:
		return 403
	case core.ErrModelNotFound, core.ErrNotFound:
		return 404
	case core.ErrRequestTooLarge:
		return 413
	case core.ErrRateLimit, core.ErrQuota, core.ErrSlowDown, core.ErrSpendLimit, core.ErrBudget:
		return 429
	case core.ErrBadGateway:
		return 502
	case core.ErrOverloaded:
		return 503
	case core.ErrGatewayTimeout:
		return 504
	}
	return 500
}

// statusWriter remembers the status the driver wrote.
type statusWriter struct {
	core.ResponseWriter
	status int
}

func (w *statusWriter) Status(code int) {
	w.status = code
	w.ResponseWriter.Status(code)
}

// writeError has drv write er and returns the status it sent, which the
// metrics and the events count: a driver may answer a kind with another
// status than statusOf's (OpenCode's unknown model is a 400, spec 006
// 16.3). A driver that sets none sent statusOf's.
func (e *Engine) writeError(w core.ResponseWriter, drv driver.Driver, req *core.Request, er *core.Error, rl *core.RateLimitState) int {
	sw := &statusWriter{ResponseWriter: w}
	drv.WriteError(sw, req, er, rl)
	if sw.status > 0 {
		return sw.status
	}
	return e.statusOf(er)
}

func (e *Engine) count(drv driver.Driver, model, endpoint string, status int) {
	e.M.Requests.Inc(drv.Name(), model, endpoint, strconv.Itoa(status))
}

// fail writes an error before limits were charged.
func (e *Engine) fail(r *request, w core.ResponseWriter, er *core.Error) {
	r.finishErrKind = er.Kind
	e.failNoFinish(r, w, er)
}

func (e *Engine) failNoFinish(r *request, w core.ResponseWriter, er *core.Error) {
	r.status = e.statusOf(er)
	if er.Transport != core.TransportNone && w.Fault(er.Transport) {
		e.record(r)
		return
	}
	e.addDiagHeaders(r, er)
	status := e.writeError(w, r.in.Driver, r.req, er, &r.rl)
	r.status = status
	e.count(r.in.Driver, r.modelLabel(), r.endpoint(), status)
	e.M.E2E.Observe(time.Since(r.t0).Seconds(), r.in.Driver.Name(), r.modelLabel())
	e.record(r)
	e.Resilience.Observe(r.testID, r.req, status, er)
	e.logReq(r, status, er)
}

func (e *Engine) addDiagHeaders(r *request, er *core.Error) {
	if er.Extra == nil {
		er.Extra = map[string]any{}
	}
	if h := driver.ConventionsOf(r.in.Driver).RequestIDHeader; h != "" {
		er.Extra["header:"+h] = requestIDHeader(r.req.RequestID)
	}
	if e.podName != "" {
		er.Extra["header:x-wwz-pod"] = e.podName
	}
	if r.scn != nil {
		er.Extra["header:x-wwz-scenario"] = r.scn.Scenario
		er.Extra["header:x-wwz-step"] = r.scn.Step
		er.Extra["header:x-wwz-attempt"] = strconv.Itoa(r.scn.Attempt)
	}
	if v, ok := r.inputBreakdown(); ok {
		er.Extra["header:"+inputBreakdownHeader] = v
	} else {
		r.sentBreakdown = nil
		delete(er.Extra, "header:"+inputBreakdownHeader) // er may be a shared step error
	}
}

func (e *Engine) failAndFinish(ctx context.Context, r *request, w core.ResponseWriter, er *core.Error) {
	if er.Kind == core.ErrGatewayTimeout && r.errs.TimeoutDelay > 0 {
		sleepCtx(ctx, r.errs.TimeoutDelay)
	}
	if er.Transport == core.TransportSlowHeaders {
		d := er.TransportDelay
		if d <= 0 {
			d = chaos.DefaultSlowHeadersDelay
		}
		sleepCtx(ctx, d)
	}
	e.failNoFinish(r, w, er)
	e.finish(ctx, r)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// finishDenied publishes the error event of a request refused by Check
// (spec 012): Finish with Denial set writes only the event, no counter moves.
func (e *Engine) finishDenied(ctx context.Context, r *request, dec limits.Decision) {
	fr := limits.FinishRequest{Subjects: r.subjects, RequestID: r.req.RequestID, Now: time.Now(), Status: r.status, ErrorKind: dec.Kind, DurationMS: time.Since(r.t0).Milliseconds(), Model: r.eventModel(), Driver: r.in.Driver.Name(), Endpoint: r.endpoint(), TestID: r.testID, Account: r.account(), Key: r.eventKey(), Denial: &limits.Denial{Entity: dec.Entity, Limit: dec.LimitName}}
	if r.billing != nil {
		fr.BillingSource = r.billing.Source
	}
	_ = e.Store.Finish(ctx, fr)
}

// denialError turns a limits denial into the error the driver renders.
// The store's own message ("rate limit of 1 requests per minute exceeded")
// is not the provider's text, so it stays out of Message (it still reaches
// the event stream through finishDenied) and the driver writes its dialect's
// default (spec 006, ADR 0014). Ban and suspension reasons are operator text
// and pass through. The numbers of the denial travel in Extra so the driver
// can quote them: limit_value (the configured limit), limit_used (consumed
// in the window) and limit_requested (what this request asked for).
func denialError(dec limits.Decision, promptTokens, reserve int) *core.Error {
	er := &core.Error{Kind: dec.Kind, RetryAfter: dec.RetryAfter, Extra: map[string]any{"limit": dec.LimitName}}
	if dec.Entity.Kind != "" {
		// The kind of the entity that denied (ADR 0072): the driver picks
		// the budget's coverage and the free-usage error by it.
		er.Extra["entity_kind"] = string(dec.Entity.Kind)
	}
	if dec.Entity.Kind == limits.KindFreeModels {
		// The provider's free-models caps have a form of their own
		// (OpenRouter's limit_source openrouter_free_tier_per_minute).
		er.Extra["free_models"] = true
	}
	if dec.Kind == core.ErrAccountBanned || dec.Kind == core.ErrAccountSuspended {
		// The abuse ladder's reason is internal (task 250): the driver
		// writes its provider's text.
		if !strings.HasPrefix(dec.Message, abuseReason) {
			er.Message = dec.Message
		}
		return er
	}
	st := dec.State
	if !st.Present {
		return er
	}
	var lim, rem, req int
	switch dec.LimitName {
	case "rpm":
		lim, rem, req = st.RequestsLimit, st.RequestsRemaining, 1
	case "tpm":
		lim, rem, req = st.TokensLimit, st.TokensRemaining, promptTokens+reserve
	case "itpm":
		lim, rem, req = st.InputTokensLimit, st.InputTokensRemain, promptTokens
	case "otpm":
		lim, rem, req = st.OutputTokensLimit, st.OutputTokensRem, reserve
	}
	if lim <= 0 {
		return er
	}
	er.Extra["limit_value"] = lim
	er.Extra["limit_used"] = min(lim, max(0, lim-rem))
	er.Extra["limit_requested"] = req
	return er
}

func (e *Engine) recordStrike(ctx context.Context, r *request, dec limits.Decision) {
	ab := r.cfg.Abuse
	if !ab.Enabled || ab.Strike429 == nil || dec.Kind != core.ErrRateLimit {
		return
	}
	for _, s := range r.subjects {
		if s.Entity.Kind != limits.KindKey {
			continue
		}
		n, _ := e.Store.Strike(ctx, s.Entity, "rate_limit", ab.Strike429.Window)
		e.onStrike(ctx, ab, s.Entity, n)
	}
}

// balances reads the store's view of key and of its account for the
// key-info routes; nil for what the store cannot tell (no key, store
// error).
func (e *Engine) balances(ctx context.Context, cfg *config.Config, key *config.KeyConfig) (k, a *driver.Balance) {
	if key == nil {
		return nil, nil
	}
	read := func(ent limits.Entity) *driver.Balance {
		st, err := e.Store.Get(ctx, ent)
		if err != nil {
			e.Log.Warn("limits store error", "err", err)
			return nil
		}
		b := balanceOf(st)
		b.Quota5hUSD, b.Quota5hSince = st.Quota5hUSD, st.Quota5hSince // OpenCode's rolling window (task 296)
		return b
	}
	eff := cfg.LimitsFor(key, e.accountTier(key))
	if eff.Key.ID != "" {
		k = read(limits.Entity{Kind: limits.KindKey, ID: eff.Key.ID})
	}
	if eff.Account.ID != "" {
		a = read(limits.Entity{Kind: limits.KindAccount, ID: eff.Account.ID})
	}
	return k, a
}

func (e *Engine) finish(ctx context.Context, r *request) {
	u := core.Usage{}
	if r.resp != nil {
		u = r.resp.Usage
	}
	cost := int64(0)
	if r.model != nil {
		cost = costMicroUSD(r.model.Pricing, u)
	}
	var tools []string
	if r.resp != nil {
		for _, tc := range r.resp.ToolCalls {
			tools = append(tools, tc.Name)
		}
	}
	fr := limits.FinishRequest{Subjects: r.subjects, RequestID: r.req.RequestID, Now: time.Now(), EstOutputTokens: r.estOut, ActualOutput: u.CompletionTokens, ActualPrompt: u.PromptTokens, CachedTokens: u.CachedTokens, ReasoningTokens: u.ReasoningTokens, CostMicroUSD: cost, Status: r.status, ErrorKind: r.finishErrKind, DurationMS: time.Since(r.t0).Milliseconds(), Model: r.eventModel(), Driver: r.in.Driver.Name(), Endpoint: r.endpoint(), TestID: r.testID, Account: r.account(), Key: r.eventKey(), ToolCalls: tools, InputBreakdown: r.sentBreakdown, OnUsage: e.onUsage()}
	// The debit follows the driver's billing (ADR 0072): free nothing,
	// go/go-plus the quota by the allowance, balance the cost.
	billedFinish(&fr, r.billing)
	_ = e.Store.Finish(ctx, fr)
	if r.resp != nil {
		d := r.in.Driver.Name()
		e.M.Tokens.Add(int64(u.PromptTokens), d, r.modelLabel(), "prompt")
		e.M.Tokens.Add(int64(u.CompletionTokens-u.ReasoningTokens), d, r.modelLabel(), "completion")
		e.M.Tokens.Add(int64(u.ReasoningTokens), d, r.modelLabel(), "reasoning")
		e.M.Tokens.Add(int64(u.CachedTokens), d, r.modelLabel(), "cached")
		for _, name := range tools {
			e.M.ToolCalls.Inc(d, name)
		}
	}
	if r.scn != nil {
		e.M.ScenarioSteps.Inc(r.scn.Scenario, r.scn.Step)
	}
}

// account is the raw account id of the request's key (event field account,
// task 075), "" without a key or account.
func (r *request) account() string {
	if r.key == nil {
		return ""
	}
	return r.key.Account
}

// eventKey is the key of the event when no subject is a key: the
// account-less credential as presented ("public", ADR 0072).
func (r *request) eventKey() string {
	if r.anon {
		return r.cred.Key
	}
	return ""
}

// costMicroUSD prices usage (spec 007): fresh input at input_per_m, cache
// reads at cache_read_per_m, 5 min writes at cache_write_per_m (input_per_m
// when unset) and 1 h writes at 2 × input_per_m, as Anthropic bills them.
func costMicroUSD(p config.Pricing, u core.Usage) int64 {
	prompt, completion := costPicoUSD(p, u)
	return (prompt + completion) / 1e6
}

// costPicoUSD is the cost of u in pico-dollars, split in prompt (fresh
// input, cache reads and writes) and completion (output with reasoning),
// as OpenRouter's cost_details reports it (task 246).
func costPicoUSD(p config.Pricing, u core.Usage) (prompt, completion int64) {
	w1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	w5m := u.CacheWriteTokens - w1h
	writePrice := p.CacheWritePerM
	if writePrice == 0 {
		writePrice = p.InputPerM
	}
	in := float64(u.PromptTokens-u.CachedTokens-u.CacheWriteTokens) * p.InputPerM
	cached := float64(u.CachedTokens) * p.CacheReadPerM
	written := float64(w5m)*writePrice + float64(w1h)*2*p.InputPerM
	out := float64(u.CompletionTokens) * p.OutputPerM
	// per-million pricing × tokens = micro-dollars; × 1e6 = pico-dollars,
	// rounded so 13 × 0.1 is 1.3 µUSD and not 1.3000000000000003.
	return int64(math.Round((in + cached + written) * 1e6)), int64(math.Round(out * 1e6))
}

// setCost prices u with p: the whole micro-dollars and the pico-dollar
// split.
func setCost(p config.Pricing, u *core.Usage) {
	u.PromptCostPicoUSD, u.CompletionCostPicoUSD = costPicoUSD(p, *u)
	u.CostMicroUSD = (u.PromptCostPicoUSD + u.CompletionCostPicoUSD) / 1e6
}

func (e *Engine) logReq(r *request, status int, er *core.Error) {
	// Success logs at debug and errors at info: skip building the attributes
	// when the level is off (task 268).
	if e.Log == nil || !e.Log.Enabled(context.Background(), logLevel(er)) {
		return
	}
	attrs := []any{"request_id", r.req.RequestID, "driver", r.in.Driver.Name(), "model", r.eventModel(), "endpoint", r.endpoint(), "status", status, "ms", time.Since(r.t0).Milliseconds(), "ip", r.req.ClientIP}
	if r.key != nil {
		attrs = append(attrs, "key", r.key.Label)
	}
	if r.scn != nil {
		attrs = append(attrs, "scenario", r.scn.Scenario, "step", r.scn.Step)
	}
	if er != nil {
		attrs = append(attrs, "error", string(er.Kind), "chaos", r.chaos.Source)
		e.Log.Info("request", attrs...)
		return
	}
	if r.resp != nil {
		attrs = append(attrs, "prompt_tokens", r.resp.Usage.PromptTokens, "completion_tokens", r.resp.Usage.CompletionTokens, "tool_calls", len(r.resp.ToolCalls))
	}
	e.Log.Debug("request", attrs...)
}

func logLevel(er *core.Error) slog.Level {
	if er != nil {
		return slog.LevelInfo
	}
	return slog.LevelDebug
}

func (e *Engine) record(r *request) {
	if e.Recorder == nil {
		return
	}
	rec := Record{Time: r.t0, RequestID: r.req.RequestID, Driver: r.in.Driver.Name(), Model: r.eventModel(), Endpoint: r.endpoint(), Status: r.status, DurationMS: time.Since(r.t0).Milliseconds(), IP: r.req.ClientIP, Stream: r.req.Stream, TestID: r.testID}
	if r.key != nil {
		rec.Key = r.key.Label
	}
	if r.scn != nil {
		rec.Scenario, rec.Step, rec.Attempt = r.scn.Scenario, r.scn.Step, r.scn.Attempt
	}
	if r.plan != nil {
		rec.TTFTMS = (r.plan.Queue + r.plan.TTFT).Milliseconds()
		rec.ITLMS = float64(r.plan.ITL.Microseconds()) / 1000
	}
	if r.chaos.Pre != nil {
		rec.Error = string(r.chaos.Pre.Kind)
	} else if r.finishErrKind != "" {
		rec.Error = string(r.finishErrKind)
	}
	if r.chaos.Mid != nil {
		rec.MidStream = string(r.chaos.Mid.Kind)
	}
	rec.ToolBreak = r.chaos.ToolBreak
	if r.resp != nil {
		rec.PromptTokens, rec.CompletionTokens, rec.ReasoningTokens = r.resp.Usage.PromptTokens, r.resp.Usage.CompletionTokens, r.resp.Usage.ReasoningTokens
		for _, tc := range r.resp.ToolCalls {
			rec.ToolCalls = append(rec.ToolCalls, tc.Name)
		}
		rec.FinishReason = string(r.resp.FinishReason)
	}
	rec.Seed = r.seed
	e.Recorder.Add(rec)
}

// maxVerificationDetail caps client_verification_detail in the log (spec
// 013).
const maxVerificationDetail = 200

// refuseForged answers a request the strict client verification refused
// (spec 020, ADR 0074): the driver's 403 at once (latency zero), before
// the credential, the body and the limits. Nothing goes to the store but
// the error event, which has no subject: no debit, no count, no
// concurrency slot, no abuse strike. It counts in wwz_requests_total and
// wwz_client_verification_failures_total and goes to the log and
// /admin/requests (spec 013).
func (e *Engine) refuseForged(ctx context.Context, in Input, w core.ResponseWriter, er *core.Error, t0 time.Time) {
	drv := in.Driver
	reason, detail, _ := driver.ForgedReason(er)
	req := &core.Request{Headers: in.Headers, ClientIP: in.ClientIP, RequestID: e.newID(), Endpoint: core.EndpointMessages}
	if in.Kind == driver.KindCountTokens {
		req.Endpoint = core.EndpointCountTokens
	}
	if m, ok := drv.(driver.IDMinter); ok {
		req.RequestID = m.RequestID(in.Kind, req.RequestID)
	}
	if e.podName != "" {
		if er.Extra == nil {
			er.Extra = map[string]any{}
		}
		er.Extra["header:x-wwz-pod"] = e.podName
	}
	status := e.writeError(w, drv, req, er, nil)
	endpoint := string(req.Endpoint)
	e.count(drv, "", endpoint, status)
	e.M.ClientVerificationFailures.Inc(drv.Name(), reason)
	ms := time.Since(t0).Milliseconds()
	testID := in.Headers.Get(strings.ToLower(e.Config().Chaos.ScopeHeader))
	if e.Recorder != nil {
		e.Recorder.Add(Record{Time: t0, RequestID: req.RequestID, Driver: drv.Name(), Endpoint: endpoint, Status: status, DurationMS: ms, IP: in.ClientIP, TestID: testID, Error: string(er.Kind)})
	}
	e.Resilience.Observe(testID, req, status, er)
	if e.Log != nil && e.Log.Enabled(ctx, slog.LevelInfo) {
		if len(detail) > maxVerificationDetail {
			detail = strings.ToValidUTF8(detail[:maxVerificationDetail], "")
		}
		e.Log.Info("request", "request_id", req.RequestID, "driver", drv.Name(), "model", "", "endpoint", endpoint, "status", status, "ms", ms, "ip", in.ClientIP,
			"error", string(er.Kind), "client_verification_reason", reason, "client_verification_detail", detail)
	}
	_ = e.Store.Finish(ctx, limits.FinishRequest{RequestID: req.RequestID, Now: time.Now(), Status: status, ErrorKind: er.Kind, DurationMS: ms, Driver: drv.Name(), Endpoint: endpoint, TestID: testID, Reason: reason})
}
