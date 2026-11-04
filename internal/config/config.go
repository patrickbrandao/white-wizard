// Package config defines the YAML configuration of the White Wizard server
// (wwz) and loads it with the precedence flag > env > YAML > default.
package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Config is the root of wwz.yaml.
type Config struct {
	Server     ServerConfig      `yaml:"server"`
	Admin      AdminConfig       `yaml:"admin"`
	Store      StoreConfig       `yaml:"store"`
	Drivers    []DriverMount     `yaml:"drivers"`
	Keys       []KeyConfig       `yaml:"keys"`
	Accounts   []AccountConfig   `yaml:"accounts"`
	Projects   []ProjectConfig   `yaml:"projects"`
	Models     []ModelConfig     `yaml:"models"`
	Profiles   Profiles          `yaml:"profiles"`
	Tiers      map[string]Tier   `yaml:"tiers"`
	Abuse      AbuseConfig       `yaml:"abuse"`
	Scenarios  ScenariosConfig   `yaml:"scenarios"`
	Metrics    MetricsConfig     `yaml:"metrics"`
	Logging    LoggingConfig     `yaml:"logging"`
	Chaos      ChaosConfig       `yaml:"chaos"`
	Geo        GeoConfig         `yaml:"geo"`
	Vars       map[string]string `yaml:"vars"` // free-form, exposed to templates
	Resilience ResilienceConfig  `yaml:"resilience"`
	Usage      UsageConfig       `yaml:"usage"`
	// Errors holds the global errors settings (errors.outages, spec 009).
	Errors GlobalErrors   `yaml:"errors"`
	raw    map[string]any // original document, for /admin/config
	// warnings are the legacy forms Validate translated (ADR 0048).
	warnings []string
	// inline caches the inline model profiles (LatencyFor, ErrorsFor,
	// GenerationFor) once converted; nil (a Config not built by Parse)
	// converts them on every call.
	inline *inlineProfiles
}

// UsageConfig controls extra usage reporting (I16, ADR 0027); reloadable.
type UsageConfig struct {
	// Extended adds the x-wwz-input-breakdown header (input tokens by
	// origin) to every inference response. The body never changes.
	Extended bool `yaml:"extended"`
}

// ResilienceConfig controls the client resilience report (I15,
// GET /admin/resilience).
type ResilienceConfig struct {
	// Track exposes per-X-Test-Id retry reports; nil means true.
	Track *bool `yaml:"track"`
	// MaxReports caps how many reports GET /admin/resilience lists by default
	// (0 = 100; at most 10000).
	MaxReports int `yaml:"max_reports"`
}

// Tracking reports whether resilience reports are exposed.
func (r ResilienceConfig) Tracking() bool { return r.Track == nil || *r.Track }

// ServerConfig is the data plane.
type ServerConfig struct {
	Listen             string        `yaml:"listen"`      // ":8000"
	Reuseport          bool          `yaml:"reuseport"`   // SO_REUSEPORT listeners, one per CPU
	Concurrency        int           `yaml:"concurrency"` // fasthttp max concurrent connections
	MaxRequestBodyMB   int           `yaml:"max_request_body_mb"`
	ReadTimeout        time.Duration `yaml:"read_timeout"`
	WriteTimeout       time.Duration `yaml:"write_timeout"`
	IdleTimeout        time.Duration `yaml:"idle_timeout"`
	Auth               string        `yaml:"auth"` // keys | none
	StartupDuration    time.Duration `yaml:"startup_duration"`
	Seed               int64         `yaml:"seed"`
	RequestHeaders     *bool         `yaml:"request_headers"` // honour x-wwz-* per-request overrides
	MaxChunksPerStream int           `yaml:"max_chunks_per_stream"`
	PingInterval       time.Duration `yaml:"ping_interval"`
	TrustedProxies     []string      `yaml:"trusted_proxies"`
	TLS                TLSConfig     `yaml:"tls"`
	MaxRPS             int           `yaml:"max_rps"` // global protection, 0 = unlimited
}

// TLSConfig enables HTTPS on the data plane.
type TLSConfig struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	SelfSigned bool   `yaml:"self_signed"`
}

// AdminConfig is the control plane (net/http).
type AdminConfig struct {
	Listen string `yaml:"listen"` // ":9000"
	Token  string `yaml:"token"`  // optional bearer token for /admin/*
	Debug  bool   `yaml:"debug"`  // keep last requests for /admin/requests
}

// StoreConfig selects the control state backend.
type StoreConfig struct {
	Kind        string        `yaml:"kind"` // memory | valkey
	Addr        []string      `yaml:"addr"`
	Password    string        `yaml:"password"`
	Cluster     bool          `yaml:"cluster"`
	ReadTimeout time.Duration `yaml:"read_timeout"`
	OnDown      string        `yaml:"on_down"` // allow | deny | local
	Prefix      string        `yaml:"prefix"`
	LocalFirst  bool          `yaml:"local_first"` // per-instance token buckets synced to Valkey
	EventStream bool          `yaml:"event_stream"`
	EventMaxLen int64         `yaml:"event_max_len"`
	EventSample float64       `yaml:"event_sample"`
}

// DriverMount enables a driver at a path prefix.
type DriverMount struct {
	Name    string            `yaml:"name"`
	Prefix  string            `yaml:"prefix"`
	Options map[string]string `yaml:"options"`
}

// KeyConfig is an API key the simulator accepts.
type KeyConfig struct {
	Key       string   `yaml:"key"`
	Label     string   `yaml:"label"`
	Account   string   `yaml:"account"`
	Project   string   `yaml:"project"`
	Tier      string   `yaml:"tier"`
	Driver    string   `yaml:"driver"` // driver hint for root collisions
	Limits    *Limits  `yaml:"limits"`
	Latency   string   `yaml:"latency"` // profile override
	Errors    string   `yaml:"errors"`
	Status    string   `yaml:"status"` // active | suspended | banned | expired
	ExpiresAt string   `yaml:"expires_at"`
	Models    []string `yaml:"models"` // allowlist; empty = all
	// ServiceAccount: the key belongs to a service account, not to a
	// member (OpenCode: outside the Go quota and the Console budgets; ADR
	// 0072). Drivers without the notion ignore it.
	ServiceAccount bool `yaml:"service_account,omitempty"`
}

// Limits are rate and spend limits of an entity.
type Limits struct {
	RPS          int           `yaml:"rps"`
	Burst        int           `yaml:"burst"`
	RPM          int           `yaml:"rpm"`
	RPD          int           `yaml:"rpd"`
	TPM          int           `yaml:"tpm"`
	ITPM         int           `yaml:"itpm"`
	OTPM         int           `yaml:"otpm"`
	TPD          int           `yaml:"tpd"`
	Concurrency  int           `yaml:"concurrency"`
	MaxQueue     int           `yaml:"max_queue"`
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	// Spend in USD per month; 0 = unlimited.
	SpendMonthlyUSD float64 `yaml:"spend_monthly_usd"`
	SpendSoftUSD    float64 `yaml:"spend_soft_usd"`
	// SpendMonthlyModelsUSD is the monthly budget of each model, keyed by
	// models[].id or <catalog>/<id> (ADR 0072); a map with entries at an
	// upper level replaces the lower one. An empty map declares nothing:
	// the YAML round trip of POST /admin/config drops it.
	SpendMonthlyModelsUSD map[string]float64 `yaml:"spend_monthly_models_usd,omitempty"`
	CreditsUSD            *float64           `yaml:"credits_usd"` // OpenRouter-style balance
	// Windows in dollars (OpenCode Go style): rolling 5h, weekly, monthly.
	QuotaUSD         *QuotaUSD `yaml:"quota_usd"`
	Algorithm        string    `yaml:"algorithm"`   // token_bucket | fixed_window
	TPMReserve       string    `yaml:"tpm_reserve"` // max_tokens | none
	ExcludeCacheRead bool      `yaml:"exclude_cache_read"`
	OnLimit          string    `yaml:"on_limit"` // reject | queue
	Mode             string    `yaml:"mode"`     // always | after_n | window | off
	AfterN           int       `yaml:"after_n"`
	Window           *Window   `yaml:"window"` // mode: window
	Ramp             *Ramp     `yaml:"ramp"`
}

// Window is the periodic window of `mode: window` (spec 010): the limits
// apply during the first Duration of every Every, counted from the Unix
// epoch so every instance and store agree on the phase.
type Window struct {
	Every    time.Duration `yaml:"every"`
	Duration time.Duration `yaml:"duration"`
}

// Active reports whether now falls inside the enforced part of the window.
func (w Window) Active(now time.Time) bool {
	if w.Every <= 0 || w.Duration >= w.Every {
		return true
	}
	if w.Duration <= 0 {
		return false
	}
	phase := time.Duration(now.UnixNano() % int64(w.Every))
	if phase < 0 {
		phase += w.Every
	}
	return phase < w.Duration
}

// QuotaUSD is a dollar quota split across windows (percent of monthly).
type QuotaUSD struct {
	MonthlyUSD   float64 `yaml:"monthly_usd"`
	Rolling5hPct float64 `yaml:"rolling_5h_pct"`
	WeeklyPct    float64 `yaml:"weekly_pct"`
	// Models is the monthly quota of each model, keyed by models[].id or
	// <catalog>/<id>, with the same window percentages (ADR 0072).
	Models map[string]float64 `yaml:"models,omitempty"`
}

// Ramp is the acceleration limit (OpenAI slow_down / Anthropic acceleration).
type Ramp struct {
	ThresholdTPM   int           `yaml:"threshold_tpm"`
	MaxIncreasePct float64       `yaml:"max_increase_pct"`
	Window         time.Duration `yaml:"window"`
}

// Tier groups limits and promotion rules.
type Tier struct {
	Limits      Limits   `yaml:"limits"`
	PromoteWhen *Promote `yaml:"promote_when"`
	Next        string   `yaml:"next"`
	// Plan is the subscription plan the tier stands for, one of the
	// DriverDecl.Plans of a registered driver; empty = the driver's
	// default. It is not part of cfg:policies (ADR 0072).
	Plan string `yaml:"plan,omitempty"`
}

// Promote describes automatic tier upgrade (ADR 0051): every non-zero
// condition must hold (AND) for the account to move to Tier.Next.
type Promote struct {
	PurchasedUSD float64 `yaml:"purchased_usd"` // credits bought, accumulated
	SpendUSD     float64 `yaml:"spend_usd"`     // spend, accumulated (usage history)
	AgeDays      int     `yaml:"age_days"`      // days since the account was first seen
	Requests     int64   `yaml:"requests"`      // requests served, accumulated
}

// ModelConfig declares a served model.
type ModelConfig struct {
	ID        string   `yaml:"id"`
	Aliases   []string `yaml:"aliases"`
	Protocols []string `yaml:"protocols"` // wire dialects (ADR 0048)
	// Drivers is the catalog: drivers that list and accept the model;
	// empty = every driver speaking one of Protocols (ADR 0048).
	Drivers       []string     `yaml:"drivers"`
	DisplayName   string       `yaml:"display_name"`
	OwnedBy       string       `yaml:"owned_by"`
	Created       int64        `yaml:"created"`
	ContextWindow int          `yaml:"context_window"`
	MaxOutput     int          `yaml:"max_output"`
	Capabilities  Capabilities `yaml:"capabilities"`
	Latency       any          `yaml:"latency"`    // profile name or inline LatencyProfile
	Errors        any          `yaml:"errors"`     // profile name or inline ErrorProfile
	Generation    any          `yaml:"generation"` // profile name or inline GenerationProfile
	Scenarios     []string     `yaml:"scenarios"`
	Tokenizer     string       `yaml:"tokenizer"`
	Pricing       Pricing      `yaml:"pricing,omitempty"` // omitted when not declared (pricing.go)
	// Catalogs restricts the model to the catalogs a driver declares and
	// prices it per catalog (ADR 0072); empty = every catalog, with Pricing.
	Catalogs map[string]CatalogEntry `yaml:"catalogs,omitempty"`
	Extra    map[string]any          `yaml:"extra"` // dialect-specific /models fields
	// Version is the snapshot name the real provider answers in model
	// (gpt-4o-mini → gpt-4o-mini-2024-07-18; spec 003, ADR 0049).
	Version string `yaml:"version"`
	// DeprecatedAt and RetiresAt are lifecycle dates (YYYY-MM-DD). From
	// RetiresAt on, the model answers as nonexistent.
	DeprecatedAt string `yaml:"deprecated_at"`
	RetiresAt    string `yaml:"retires_at"`
	// PromptCache swaps the driver's prompt-cache rule for the one of
	// another real model (spec 003/007, ADR 0046); nil = driver default.
	PromptCache *PromptCacheRule `yaml:"prompt_cache"`

	retires time.Time // RetiresAt parsed by Validate
}

// PromptCacheRule is models[].prompt_cache. Unset fields keep the driver's
// default for the current generation of real models.
type PromptCacheRule struct {
	// MinTokens is the shortest prefix the provider caches.
	MinTokens *int `yaml:"min_tokens"`
	// Granularity is the step of cached lengths above MinTokens; 0 means
	// the exact boundary.
	Granularity *int `yaml:"granularity"`
	// Kind is automatic (every prompt writes its prefixes), explicit
	// (cache_control breakpoints) or none.
	Kind string `yaml:"kind"`
}

// validate checks the rule of models[i].
func (p *PromptCacheRule) validate(i int) error {
	if p == nil {
		return nil
	}
	if p.MinTokens != nil && *p.MinTokens < 0 {
		return fmt.Errorf("config: models[%d].prompt_cache.min_tokens must be >= 0, got %d", i, *p.MinTokens)
	}
	if p.Granularity != nil && *p.Granularity < 0 {
		return fmt.Errorf("config: models[%d].prompt_cache.granularity must be >= 0, got %d", i, *p.Granularity)
	}
	switch p.Kind {
	case "", "automatic", "explicit", "none":
	default:
		return fmt.Errorf("config: models[%d].prompt_cache.kind must be automatic, explicit or none, got %q", i, p.Kind)
	}
	return nil
}

// Capabilities declare what a model supports.
type Capabilities struct {
	Tools      *bool `yaml:"tools"`
	Reasoning  *bool `yaml:"reasoning"`
	Vision     *bool `yaml:"vision"`
	JSONSchema *bool `yaml:"json_schema"`
}

// Profiles are named, reusable behaviours.
type Profiles struct {
	Latency    map[string]LatencyProfile    `yaml:"latency"`
	Errors     map[string]ErrorProfile      `yaml:"errors"`
	Generation map[string]GenerationProfile `yaml:"generation"`
}

// Dist is a latency distribution. A bare duration in YAML parses as fixed.
type Dist struct {
	Kind     string        `yaml:"dist"` // fixed | uniform | normal | lognormal | exponential | pareto
	Value    time.Duration `yaml:"value"`
	Min      time.Duration `yaml:"min"`
	Max      time.Duration `yaml:"max"`
	Mean     time.Duration `yaml:"mean"`
	StdDev   time.Duration `yaml:"stddev"`
	Median   time.Duration `yaml:"median"`
	P99      time.Duration `yaml:"p99"`
	Scale    time.Duration `yaml:"scale"`
	Shape    float64       `yaml:"shape"`
	MaxValue time.Duration `yaml:"max_value"`
}

// LatencyProfile models each stage.
type LatencyProfile struct {
	Model              string        `yaml:"model"` // constant | per_token | polynomial
	Queue              *Dist         `yaml:"queue"`
	TTFT               *Dist         `yaml:"ttft"`
	PrefillOverhead    *Dist         `yaml:"prefill_overhead"`
	PrefillPerToken    *Dist         `yaml:"prefill_per_token"`
	ITL                *Dist         `yaml:"itl"`
	ReasoningITL       *Dist         `yaml:"reasoning_itl"`
	ToolCallDelay      *Dist         `yaml:"tool_call_delay"`
	ToolArgsITL        *Dist         `yaml:"tool_args_itl"`
	FinishDelay        *Dist         `yaml:"finish_delay"`
	LoadFactor         float64       `yaml:"load_factor"`
	Concurrency        int           `yaml:"concurrency"`
	MaxQueue           int           `yaml:"max_queue"`
	QueueTimeout       time.Duration `yaml:"queue_timeout"`
	ChunkTokens        int           `yaml:"chunk_tokens"`
	ChunkBytes         int           `yaml:"chunk_bytes"`
	SpeedupRatio       float64       `yaml:"speedup_ratio"`
	Clock              string        `yaml:"clock"` // real | logical
	Pacing             *Pacing       `yaml:"pacing"`
	CacheTTFTReduction float64       `yaml:"cache_ttft_reduction"`
}

// Pacing is explicit pacing (DotMock/MockServer style).
type Pacing struct {
	TokensPerSecond  float64         `yaml:"tokens_per_second"`
	Jitter           float64         `yaml:"jitter"` // 0..1
	InterChunkDelays []time.Duration `yaml:"inter_chunk_delays"`
	TotalDuration    time.Duration   `yaml:"total_duration"`
	ChunkRunes       int             `yaml:"chunk_runes"`
	SubwordStreaming bool            `yaml:"subword_streaming"`
	DribbleChunks    int             `yaml:"dribble_chunks"`
	DribbleTotal     time.Duration   `yaml:"dribble_total"`
}

// ErrorProfile controls injected failures.
type ErrorProfile struct {
	Rate           float64            `yaml:"rate"` // 0..100
	Types          []string           `yaml:"types"`
	Weights        map[string]float64 `yaml:"weights"`
	MidStreamRate  float64            `yaml:"mid_stream_rate"`
	MidStreamTypes []string           `yaml:"mid_stream_types"`
	AfterChunks    int                `yaml:"after_chunks"`
	AfterRatio     float64            `yaml:"after_ratio"`
	HangDuration   time.Duration      `yaml:"hang_duration"`
	TimeoutDelay   time.Duration      `yaml:"timeout_delay"`
	// SlowHeadersDelay delays the slow_headers fault; 0 = 15 s (ADR 0058).
	SlowHeadersDelay time.Duration    `yaml:"slow_headers_delay"`
	RetryAfter       time.Duration    `yaml:"retry_after"`
	AfterN           int64            `yaml:"after_n"`
	Burst            *Burst           `yaml:"burst"`
	ToolMisbehavior  *ToolMisbehavior `yaml:"tool_misbehavior"`
	Moderation       *Moderation      `yaml:"moderation"`
	// Outages are scheduled unavailability windows (spec 009); they add to
	// the windows of the other levels (global, model, key).
	Outages []Outage `yaml:"outages"`
}

// Burst is a periodic failure window.
type Burst struct {
	Every    time.Duration `yaml:"every"`
	Duration time.Duration `yaml:"duration"`
	Rate     float64       `yaml:"rate"`
}

// ToolMisbehavior is the AIMock-style catalogue of broken tool calls.
// Tool limits the breakage to calls of that tool and Drivers to requests
// served by the listed drivers (spec 017 I1); empty means no filter.
type ToolMisbehavior struct {
	Rate    float64  `yaml:"rate"`
	Kinds   []string `yaml:"kinds"`
	Tool    string   `yaml:"tool"`
	Drivers []string `yaml:"drivers"`
}

// toolMisbehaviorKinds maps every accepted tool-misbehaviour kind to its
// canonical name. The spec 017 names truncated, trailing_comma and
// single_quotes are aliases of the invalid_json_* kinds.
var toolMisbehaviorKinds = map[string]string{
	"invalid_json_truncated":      "invalid_json_truncated",
	"invalid_json_trailing_comma": "invalid_json_trailing_comma",
	"invalid_json_single_quotes":  "invalid_json_single_quotes",
	"truncated":                   "invalid_json_truncated",
	"trailing_comma":              "invalid_json_trailing_comma",
	"single_quotes":               "invalid_json_single_quotes",
	"missing_required":            "missing_required",
	"wrong_type":                  "wrong_type",
	"extra_property":              "extra_property",
	"enum_mismatch":               "enum_mismatch",
	"not_object":                  "not_object",
	"unknown_tool":                "unknown_tool",
	"duplicate_id":                "duplicate_id",
	"length_mid_tool":             "length_mid_tool",
	"empty_response":              "empty_response",
	"reasoning_only":              "reasoning_only",
}

// CanonicalToolMisbehavior resolves a kind (or its spec alias) to the
// canonical name; ok is false for an unknown kind.
func CanonicalToolMisbehavior(kind string) (string, bool) {
	c, ok := toolMisbehaviorKinds[kind]
	return c, ok
}

// validate rejects out-of-range rates, unknown kinds and unknown drivers.
func (t *ToolMisbehavior) validate(where string) error {
	if t == nil {
		return nil
	}
	if t.Rate < 0 || t.Rate > 100 || math.IsNaN(t.Rate) {
		return fmt.Errorf("config: %s.tool_misbehavior.rate must be between 0 and 100, got %v", where, t.Rate)
	}
	for _, k := range t.Kinds {
		if _, ok := toolMisbehaviorKinds[k]; !ok {
			return fmt.Errorf("config: %s.tool_misbehavior: unknown kind %q", where, k)
		}
	}
	for _, d := range t.Drivers {
		if ok := toolMisbehaviorDrivers(); !slices.Contains(ok, d) {
			return fmt.Errorf("config: %s.tool_misbehavior: unknown driver %q (want one of %s)", where, d, strings.Join(ok, ", "))
		}
	}
	return nil
}

// Moderation simulates content filtering.
type Moderation struct {
	Probability float64 `yaml:"probability"`
	Mode        string  `yaml:"mode"` // finish_reason | http_403 | refusal
	Category    string  `yaml:"category"`
}

// GenerationProfile controls content.
type GenerationProfile struct {
	Mode                      string           `yaml:"mode"` // random | echo | lorem | dataset | template | json
	Template                  string           `yaml:"template"`
	OutputTokens              *TokenDist       `yaml:"output_tokens"`
	ReasoningTokens           *ReasoningTokens `yaml:"reasoning_tokens"`
	ToolCalls                 *ToolCallsGen    `yaml:"tool_calls"`
	TextBeforeToolProbability float64          `yaml:"text_before_tool_probability"`
	ReasoningField            string           `yaml:"reasoning_field"` // reasoning_content | reasoning | inline_tag
	ToolArgsChunkBytes        int              `yaml:"tool_args_chunk_bytes"`
	DatasetPath               string           `yaml:"dataset_path"`
	Sentences                 []string         `yaml:"sentences"`
	RedactedProbability       float64          `yaml:"redacted_probability"`
	InterleavedThinking       bool             `yaml:"interleaved_thinking"`
}

// TokenDist is an output length distribution.
type TokenDist struct {
	Dist    string      `yaml:"dist"` // histogram | uniform | normal | fixed
	Buckets [][]float64 `yaml:"buckets"`
	Min     int         `yaml:"min"`
	Max     int         `yaml:"max"`
	Mean    float64     `yaml:"mean"`
	StdDev  float64     `yaml:"stddev"`
	Value   int         `yaml:"value"`
}

// ReasoningTokens controls thinking length.
type ReasoningTokens struct {
	Min         int     `yaml:"min"`
	Max         int     `yaml:"max"`
	Probability float64 `yaml:"probability"` // 0..100
}

// ToolCallsGen controls random tool calling.
type ToolCallsGen struct {
	Probability              float64 `yaml:"probability"`
	ExtraCallProbability     float64 `yaml:"extra_call_probability"`
	OptionalParamProbability float64 `yaml:"optional_param_probability"`
	MinInt                   int64   `yaml:"min_int"`
	MaxInt                   int64   `yaml:"max_int"`
	MinNumber                float64 `yaml:"min_number"`
	MaxNumber                float64 `yaml:"max_number"`
	MinArrayLen              int     `yaml:"min_array_len"`
	MaxArrayLen              int     `yaml:"max_array_len"`
	SkipValidation           bool    `yaml:"skip_validation"`
}

// AbuseConfig is the escalation ladder.
type AbuseConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Strike429        *StrikeRule   `yaml:"strike_429"`
	StrikeInvalidKey *StrikeRule   `yaml:"strike_invalid_key"`
	SharedKeyIPs     int           `yaml:"shared_key_ips"`
	Ladder           []LadderStep  `yaml:"ladder"`
	BanAfterStrikes  int           `yaml:"ban_after_strikes"`
	StrikeTTL        time.Duration `yaml:"strike_ttl"`
}

// StrikeRule is "N events in M minutes".
type StrikeRule struct {
	Count  int           `yaml:"count"`
	Window time.Duration `yaml:"window"`
}

// LadderStep is one rung of the escalation.
type LadderStep struct {
	Action   string        `yaml:"action"` // warn | throttle | suspend | ban
	Duration time.Duration `yaml:"duration"`
	Pct      float64       `yaml:"pct"`
}

// ScenariosConfig points at scenario files.
type ScenariosConfig struct {
	// Builtin loads the scenarios embedded in the binary (default true;
	// spec 011, ADR 0048).
	Builtin        *bool    `yaml:"builtin"`
	Dir            string   `yaml:"dir"`
	Files          []string `yaml:"files"`
	Select         []string `yaml:"select"`          // header, metadata, marker, model_suffix, match
	ConversationID string   `yaml:"conversation_id"` // header | hash
	OnNoMatch      string   `yaml:"on_no_match"`
}

// UseBuiltin reports whether the embedded scenarios are loaded.
func (s ScenariosConfig) UseBuiltin() bool { return s.Builtin == nil || *s.Builtin }

// MetricsConfig tunes Prometheus exposition.
type MetricsConfig struct {
	Enabled   *bool     `yaml:"enabled"`
	MaxSeries int       `yaml:"max_series"`
	VLLM      bool      `yaml:"vllm_mirror"`
	Buckets   []float64 `yaml:"buckets"`
}

// LoggingConfig controls structured logs.
type LoggingConfig struct {
	Level  string  `yaml:"level"`
	Format string  `yaml:"format"` // json | text
	Sample float64 `yaml:"sample"`
	HTTP   bool    `yaml:"http"`
}

// ChaosConfig is global chaos scoping.
type ChaosConfig struct {
	ScopeHeader string `yaml:"scope_header"` // X-Test-Id
}

// GeoConfig blocks countries (requires client IP → country mapping; CIDR map in MVP).
type GeoConfig struct {
	BlockedCIDRs []string `yaml:"blocked_cidrs"`
}

// Validate checks internal consistency.
func (c *Config) Validate() error {
	if len(c.Models) == 0 {
		return fmt.Errorf("config: at least one model is required")
	}
	seen := map[string]bool{}
	for i, m := range c.Models {
		if m.ID == "" {
			return fmt.Errorf("config: models[%d].id is required", i)
		}
		if seen[m.ID] {
			return fmt.Errorf("config: duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if name, ok := m.Latency.(string); ok && name != "" {
			if _, ok := c.Profiles.Latency[name]; !ok {
				return fmt.Errorf("config: model %q references unknown latency profile %q", m.ID, name)
			}
		}
		if name, ok := m.Errors.(string); ok && name != "" {
			if _, ok := c.Profiles.Errors[name]; !ok {
				return fmt.Errorf("config: model %q references unknown errors profile %q", m.ID, name)
			}
		}
		if name, ok := m.Generation.(string); ok && name != "" {
			if _, ok := c.Profiles.Generation[name]; !ok {
				return fmt.Errorf("config: model %q references unknown generation profile %q", m.ID, name)
			}
		}
		if err := c.validateCatalog(i); err != nil {
			return err
		}
		if err := c.validateInline(&c.Models[i]); err != nil {
			return err
		}
		if err := c.Models[i].validateLifecycle(i); err != nil {
			return err
		}
		if err := c.Models[i].validateExtra(i); err != nil {
			return err
		}
		if err := c.Models[i].validateTokenizer(i); err != nil {
			return err
		}
		if err := c.Models[i].PromptCache.validate(i); err != nil {
			return err
		}
		if err := c.Models[i].Pricing.validate(fmt.Sprintf("models[%d].pricing", i)); err != nil {
			return err
		}
		if err := c.Models[i].validateCatalogs(i); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Profiles.Errors)) {
		if err := validateErrorTypes("profiles.errors."+name, c.Profiles.Errors[name]); err != nil {
			return err
		}
		if err := c.Profiles.Errors[name].ToolMisbehavior.validate("profiles.errors." + name); err != nil {
			return err
		}
		if err := c.validateOutages("profiles.errors."+name+".outages", c.Profiles.Errors[name].Outages); err != nil {
			return err
		}
	}
	if err := c.validateOutages("errors.outages", c.Errors.Outages); err != nil {
		return err
	}
	if err := c.validateGenerations(); err != nil {
		return err
	}
	switch c.Server.Auth {
	case "", "keys", "none":
	default:
		return fmt.Errorf("config: server.auth must be keys or none, got %q", c.Server.Auth)
	}
	switch c.Store.Kind {
	case "", "memory", "valkey":
	default:
		return fmt.Errorf("config: store.kind must be memory or valkey, got %q", c.Store.Kind)
	}
	if c.Store.Kind == "valkey" && len(c.Store.Addr) == 0 {
		return fmt.Errorf("config: store.addr is required when store.kind is valkey")
	}
	if err := c.Store.validate(); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(c.Tiers)) {
		if !ValidTierName(name) {
			return fmt.Errorf("config: tiers.%s: invalid tier name (want [A-Za-z0-9._-]{1,64})", name)
		}
		if err := c.validateLimits(fmt.Sprintf("tiers.%s.limits", name), c.Tiers[name].Limits); err != nil {
			return err
		}
		if err := validatePlan(name, c.Tiers[name].Plan); err != nil {
			return err
		}
	}
	if err := c.validateTierChain(); err != nil {
		return err
	}
	if err := c.validateAccounts(); err != nil {
		return err
	}
	for i, k := range c.Keys {
		if d := c.reservedBy(k.Key); d != "" {
			return fmt.Errorf("config: keys[%d].key %q is reserved by the %s driver", i, k.Key, d)
		}
		if k.Tier != "" && !ValidTierName(k.Tier) {
			return fmt.Errorf("config: keys[%d].tier: invalid tier name %q (want [A-Za-z0-9._-]{1,64})", i, k.Tier)
		}
		// A key's profile overrides must exist at load time; otherwise
		// every request with that key would fail with a 500.
		if k.Latency != "" {
			if _, ok := c.Profiles.Latency[k.Latency]; !ok {
				return fmt.Errorf("config: keys[%d] references unknown latency profile %q", i, k.Latency)
			}
		}
		if k.Errors != "" {
			if _, ok := c.Profiles.Errors[k.Errors]; !ok {
				return fmt.Errorf("config: keys[%d] references unknown errors profile %q", i, k.Errors)
			}
		}
		if k.Limits == nil {
			continue
		}
		if err := c.validateLimits(fmt.Sprintf("keys[%d].limits", i), *k.Limits); err != nil {
			return err
		}
	}
	if c.Resilience.MaxReports < 0 || c.Resilience.MaxReports > 10000 {
		return fmt.Errorf("config: resilience.max_reports must be between 0 and 10000, got %d", c.Resilience.MaxReports)
	}
	for i, d := range c.Drivers {
		if d.Name == "" {
			return fmt.Errorf("config: drivers[].name is required")
		}
		if d.Prefix != "" && !strings.HasPrefix(d.Prefix, "/") {
			return fmt.Errorf("config: driver %q prefix must start with /", d.Name)
		}
		if err := validateDriverOptions(i, d); err != nil {
			return err
		}
		if err := c.checkDriverOptions(i, d); err != nil {
			return err
		}
	}
	return nil
}

// ValidTierName reports whether n is a valid tier name: [A-Za-z0-9._-]{1,64}
// (ADR 0031). The same rule applies to the YAML tiers, keys[].tier and the
// tiers the panel writes to cfg:policies.
func ValidTierName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if c != '_' && c != '-' && c != '.' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// isInline reports whether a model profile field holds an inline profile
// (anything but nil or a profile name).
func isInline(v any) bool {
	switch v.(type) {
	case nil, string:
		return false
	}
	return true
}

// validateInline decodes the model's inline profiles strictly at load time,
// so an unknown field fails here instead of being dropped in silence.
func (c *Config) validateInline(m *ModelConfig) error {
	if isInline(m.Latency) {
		if _, err := c.LatencyFor(m); err != nil {
			return fmt.Errorf("config: model %q inline latency: %w", m.ID, err)
		}
	}
	if isInline(m.Generation) {
		g, err := c.GenerationFor(m)
		if err != nil {
			return fmt.Errorf("config: model %q inline generation: %w", m.ID, err)
		}
		if err := c.validateReasoningField(fmt.Sprintf("models[%s].generation", m.ID), g.ReasoningField, m); err != nil {
			return err
		}
	} else if name, ok := m.Generation.(string); ok {
		if err := c.validateReasoningField(fmt.Sprintf("models[%s].generation (profile %q)", m.ID, name), c.Profiles.Generation[name].ReasoningField, m); err != nil {
			return err
		}
	}
	if isInline(m.Errors) {
		p, err := c.ErrorsFor(m)
		if err != nil {
			return fmt.Errorf("config: model %q inline errors: %w", m.ID, err)
		}
		if err := validateErrorTypes(fmt.Sprintf("models[%s].errors", m.ID), p); err != nil {
			return err
		}
		if err := p.ToolMisbehavior.validate(fmt.Sprintf("model %q errors", m.ID)); err != nil {
			return err
		}
		if err := c.validateOutages(fmt.Sprintf("models[%s].errors.outages", m.ID), p.Outages); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the store enums and ranges (spec 003, 012). Zero values
// mean "use the default" and are accepted.
func (s StoreConfig) validate() error {
	switch s.OnDown {
	case "", "allow", "deny", "local":
	default:
		return fmt.Errorf("config: store.on_down must be allow, deny or local, got %q", s.OnDown)
	}
	if s.EventSample < 0 || s.EventSample > 1 || math.IsNaN(s.EventSample) {
		return fmt.Errorf("config: store.event_sample must be in (0, 1], got %v", s.EventSample)
	}
	if s.EventMaxLen < 0 {
		return fmt.Errorf("config: store.event_max_len must not be negative, got %d", s.EventMaxLen)
	}
	if s.ReadTimeout < 0 {
		return fmt.Errorf("config: store.read_timeout must not be negative, got %s", s.ReadTimeout)
	}
	return nil
}

// validate checks the enums and ranges of a limits block (spec 010, 012).
// Counters, durations and dollar amounts must not be negative (0 = unlimited
// or default); percentages are in [0, 100].
func (l Limits) validate(path string) error {
	ints := []struct {
		name string
		v    int
	}{
		{"rps", l.RPS},
		{"burst", l.Burst},
		{"rpm", l.RPM},
		{"rpd", l.RPD},
		{"tpm", l.TPM},
		{"itpm", l.ITPM},
		{"otpm", l.OTPM},
		{"tpd", l.TPD},
		{"concurrency", l.Concurrency},
		{"max_queue", l.MaxQueue},
		{"after_n", l.AfterN},
	}
	for _, f := range ints {
		if f.v < 0 {
			return fmt.Errorf("config: %s.%s must not be negative, got %d", path, f.name, f.v)
		}
	}
	if l.QueueTimeout < 0 {
		return fmt.Errorf("config: %s.queue_timeout must not be negative, got %s", path, l.QueueTimeout)
	}
	floats := []struct {
		name string
		v    float64
	}{
		{"spend_monthly_usd", l.SpendMonthlyUSD}, {"spend_soft_usd", l.SpendSoftUSD},
	}
	if l.CreditsUSD != nil {
		floats = append(floats, struct {
			name string
			v    float64
		}{"credits_usd", *l.CreditsUSD})
	}
	if q := l.QuotaUSD; q != nil {
		if q.MonthlyUSD < 0 {
			return fmt.Errorf("config: %s.quota_usd.monthly_usd must not be negative, got %v", path, q.MonthlyUSD)
		}
		if err := pct(path+".quota_usd.rolling_5h_pct", q.Rolling5hPct); err != nil {
			return err
		}
		if err := pct(path+".quota_usd.weekly_pct", q.WeeklyPct); err != nil {
			return err
		}
	}
	for _, f := range floats {
		if f.v < 0 || math.IsNaN(f.v) {
			return fmt.Errorf("config: %s.%s must not be negative, got %v", path, f.name, f.v)
		}
	}
	if r := l.Ramp; r != nil {
		if r.ThresholdTPM < 0 {
			return fmt.Errorf("config: %s.ramp.threshold_tpm must not be negative, got %d", path, r.ThresholdTPM)
		}
		if r.MaxIncreasePct < 0 || math.IsNaN(r.MaxIncreasePct) {
			return fmt.Errorf("config: %s.ramp.max_increase_pct must not be negative, got %v", path, r.MaxIncreasePct)
		}
		if r.Window < 0 {
			return fmt.Errorf("config: %s.ramp.window must not be negative, got %s", path, r.Window)
		}
	}
	if w := l.Window; w != nil {
		if w.Every < 0 || w.Duration < 0 {
			return fmt.Errorf("config: %s.window every and duration must not be negative", path)
		}
		if w.Duration > w.Every {
			return fmt.Errorf("config: %s.window.duration (%s) must not exceed every (%s)", path, w.Duration, w.Every)
		}
	}
	if l.Mode == "window" && (l.Window == nil || l.Window.Every <= 0 || l.Window.Duration <= 0) {
		return fmt.Errorf("config: %s.mode window requires window.every and window.duration > 0", path)
	}
	enums := []struct {
		name, v string
		ok      []string
	}{
		{"algorithm", l.Algorithm, []string{"token_bucket", "fixed_window"}},
		{"tpm_reserve", l.TPMReserve, []string{"max_tokens", "none"}},
		{"on_limit", l.OnLimit, []string{"reject", "queue"}},
		{"mode", l.Mode, []string{"always", "after_n", "window", "off"}},
	}
	for _, e := range enums {
		if e.v != "" && !slices.Contains(e.ok, e.v) {
			return fmt.Errorf("config: %s.%s must be one of %s, got %q", path, e.name, strings.Join(e.ok, ", "), e.v)
		}
	}
	return nil
}

func pct(name string, v float64) error {
	if v < 0 || v > 100 || math.IsNaN(v) {
		return fmt.Errorf("config: %s must be between 0 and 100, got %v", name, v)
	}
	return nil
}

// FindModel resolves a model by id or alias.
func (c *Config) FindModel(id string) *ModelConfig {
	for i := range c.Models {
		m := &c.Models[i]
		if m.ID == id {
			return m
		}
		for _, a := range m.Aliases {
			if a == id {
				return m
			}
		}
	}
	return nil
}

// FindModelBySlug resolves a model the way OpenRouter resolves its model
// field (checked against the real API, task 163): slug(name) is the
// provider's name for an id or alias (author/model). The match ignores
// case; a name with the full slug wins over a name without an author, which
// matches the part of the slug after the author. A name with an author that
// is not the slug's (x/model) matches nothing. Only models in the catalog
// of the driver named driverName are considered; the first in config order wins. It returns the
// model and the id or alias that matched, as configured.
func (c *Config) FindModelBySlug(name, driverName string, slug func(string) string) (*ModelConfig, string) {
	short := !strings.Contains(name, "/")
	var byShort *ModelConfig
	shortName := ""
	for i := range c.Models {
		m := &c.Models[i]
		if !m.ModelSupports(driverName) {
			continue
		}
		for _, n := range append([]string{m.ID}, m.Aliases...) {
			s := slug(n)
			if strings.EqualFold(name, s) {
				return m, n
			}
			if _, after, ok := strings.Cut(s, "/"); ok && short && byShort == nil && strings.EqualFold(name, after) {
				byShort, shortName = m, n
			}
		}
	}
	return byShort, shortName
}

// ExtraModelVariant is the core.Error.Extra key of a *VariantMiss: the
// engine's miss on a model variant (OpenRouter's 404s, task 186).
const ExtraModelVariant = "model_variant"

// freeVariant is the suffix of a model's free variant, a model of its own in
// OpenRouter's catalog (models[].id "author/model:free").
const freeVariant = ":free"

// VariantMiss is why a name with variant suffixes resolved to no model,
// though its base names one (checked against the real API, task 186).
type VariantMiss struct {
	// Name is what the real's 404 names: the base before the suffixes
	// when Free, the name asked otherwise.
	Name string
	// Free is set when ":free" (lower case) was asked of a model without a
	// free variant: the real answers 404 "This model is unavailable for
	// free…" with the paid slug. Without it (":FREE" of such a model, or a
	// model served only as a free variant asked without the suffix) the
	// real answers 404 "No endpoints found for <name>.".
	Free bool
}

// FindModelVariant is FindModelBySlug with OpenRouter's variant suffixes
// (ResolveVariant).
func (c *Config) FindModelVariant(name, driverName string, slug func(string) string) (*ModelConfig, string, *VariantMiss) {
	return ResolveVariant(name, func(n string) (*ModelConfig, string) {
		return c.FindModelBySlug(n, driverName, slug)
	})
}

// ResolveVariant resolves name the way OpenRouter resolves a model with
// variant suffixes (":free", ":nitro", ":floor" or any other word after a
// ":" past the author; checked against the real API, task 186), find being
// the lookup of a name without them. The whole name is tried first, so a
// free variant configured as a model of its own ("author/model:free")
// matches in any case. Then the name is cut at each ":" from the last:
//   - a suffix equal to "free" in any case asks for the free variant
//     (base + ":free"); a base without one is a miss;
//   - the other suffixes (":nitro", ":floor", unknown words) answer as the
//     base, which the response names;
//   - a base with no model of its own whose free variant is configured is
//     a miss (the model is served only as free).
//
// A name that names no model at all is (nil, "", nil).
func ResolveVariant(name string, find func(string) (*ModelConfig, string)) (*ModelConfig, string, *VariantMiss) {
	m, matched := find(name)
	if m != nil {
		return m, matched, nil
	}
	cuts := []int{len(name)}
	for i := len(name) - 1; i > strings.LastIndex(name, "/"); i-- {
		if name[i] == ':' {
			cuts = append(cuts, i)
		}
	}
	for _, c := range cuts {
		base := name[:c]
		free, exact := false, false
		if c < len(name) {
			for _, s := range strings.Split(name[c+1:], ":") {
				free = free || strings.EqualFold(s, "free")
				exact = exact || s == "free"
			}
		}
		fm, fmatched := find(base + freeVariant)
		if free && fm != nil {
			return fm, fmatched, nil
		}
		if c < len(name) {
			m, matched = find(base)
		}
		switch {
		case m != nil && free && !strings.HasSuffix(strings.ToLower(matched), freeVariant):
			if exact {
				return nil, "", &VariantMiss{Name: base, Free: true}
			}
			return nil, "", &VariantMiss{Name: name}
		case m != nil:
			return m, matched, nil
		case fm != nil:
			return nil, "", &VariantMiss{Name: name}
		}
	}
	return nil, "", nil
}

// dateLayout is the format of models[].deprecated_at and retires_at.
const dateLayout = "2006-01-02"

// validateLifecycle checks deprecated_at and retires_at (YYYY-MM-DD, retires_at
// not before deprecated_at) and keeps the parsed retirement date.
func (m *ModelConfig) validateLifecycle(i int) error {
	var dep, ret time.Time
	var err error
	if m.DeprecatedAt != "" {
		if dep, err = time.Parse(dateLayout, m.DeprecatedAt); err != nil {
			return fmt.Errorf("config: models[%d].deprecated_at must be YYYY-MM-DD, got %q", i, m.DeprecatedAt)
		}
	}
	if m.RetiresAt != "" {
		if ret, err = time.Parse(dateLayout, m.RetiresAt); err != nil {
			return fmt.Errorf("config: models[%d].retires_at must be YYYY-MM-DD, got %q", i, m.RetiresAt)
		}
	}
	if !dep.IsZero() && !ret.IsZero() && ret.Before(dep) {
		return fmt.Errorf("config: models[%d].retires_at (%s) is before deprecated_at (%s)", i, m.RetiresAt, m.DeprecatedAt)
	}
	m.retires = ret
	return nil
}

// Retired reports whether the model is past retires_at at now (00:00 UTC of
// that day): from then on it answers as nonexistent (ADR 0049).
func (m *ModelConfig) Retired(now time.Time) bool {
	if m.RetiresAt == "" {
		return false
	}
	t := m.retires
	if t.IsZero() {
		var err error
		if t, err = time.Parse(dateLayout, m.RetiresAt); err != nil {
			return false
		}
	}
	return !now.Before(t)
}

// ResponseModel is the model field of a response on a provider that answers
// with the dated snapshot: version when set, else id.
func (m *ModelConfig) ResponseModel() string {
	if m.Version != "" {
		return m.Version
	}
	return m.ID
}

// FindKey returns the key config for a presented key.
func (c *Config) FindKey(key string) *KeyConfig {
	for i := range c.Keys {
		if c.Keys[i].Key == key {
			return &c.Keys[i]
		}
	}
	return nil
}

// Raw returns the original YAML document as a generic map (for /admin/config).
func (c *Config) Raw() map[string]any { return c.raw }
