package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/goccy/go-yaml"
)

// UnmarshalYAML lets a Dist be written either as a bare duration ("300ms",
// which means fixed) or as a mapping with a dist kind.
func (d *Dist) UnmarshalYAML(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	if !strings.Contains(s, ":") || (strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'")) {
		dur, err := time.ParseDuration(strings.Trim(s, "\"'"))
		if err != nil {
			// Allow bare integers as milliseconds.
			if n, err2 := strconv.Atoi(strings.Trim(s, "\"'")); err2 == nil {
				dur = time.Duration(n) * time.Millisecond
			} else {
				return fmt.Errorf("latency: %q is not a duration or a distribution: %w", s, err)
			}
		}
		*d = Dist{Kind: "fixed", Value: dur}
		return nil
	}
	type plain Dist
	var p plain
	if err := yaml.Unmarshal(b, &p); err != nil {
		return err
	}
	*d = Dist(p)
	if d.Kind == "" {
		switch {
		case d.Mean != 0 && d.StdDev != 0:
			d.Kind = "normal"
		case d.Median != 0:
			d.Kind = "lognormal"
		case d.Min != 0 || d.Max != 0:
			d.Kind = "uniform"
		case d.Mean != 0:
			d.Kind = "exponential"
		default:
			d.Kind = "fixed"
		}
	}
	return nil
}

// Load reads a YAML file, applies environment and defaults, and validates.
func Load(path string) (*Config, error) {
	var data []byte
	var err error
	if path != "" {
		data, err = os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}
	return Parse(data)
}

// Parse decodes YAML bytes (empty = all defaults) and applies env + defaults.
func Parse(data []byte) (*Config, error) {
	c := &Config{}
	if len(data) > 0 {
		var raw map[string]any
		_ = yaml.Unmarshal(data, &raw)
		if err := rejectRenamedDriverFields(raw); err != nil {
			return nil, err
		}
		if err := yaml.UnmarshalWithOptions(data, c, yaml.Strict()); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		c.raw = raw
	}
	ApplyEnv(c)
	ApplyDefaults(c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.inline = &inlineProfiles{}
	return c, nil
}

// ApplyEnv overrides selected fields from WWZ_* variables.
func ApplyEnv(c *Config) {
	if v := os.Getenv("WWZ_LISTEN"); v != "" {
		c.Server.Listen = v
	}
	if v := os.Getenv("WWZ_PORT"); v != "" {
		c.Server.Listen = ":" + strings.TrimPrefix(v, ":")
	}
	if v := os.Getenv("WWZ_ADMIN_LISTEN"); v != "" {
		c.Admin.Listen = v
	}
	if v := os.Getenv("WWZ_AUTH"); v != "" {
		c.Server.Auth = v
	}
	if v := os.Getenv("WWZ_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Server.Seed = n
		}
	}
	// WWZ_VALKEY_ADDR implies store.kind=valkey only when WWZ_STORE is not
	// set; an explicit WWZ_STORE always wins.
	if v := os.Getenv("WWZ_VALKEY_ADDR"); v != "" {
		c.Store.Kind = "valkey"
		c.Store.Addr = strings.Split(v, ",")
	}
	if v := os.Getenv("WWZ_STORE"); v != "" {
		c.Store.Kind = v
	}
	if v := os.Getenv("WWZ_VALKEY_PASSWORD"); v != "" {
		c.Store.Password = v
	}
	if v := os.Getenv("WWZ_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("WWZ_REUSEPORT"); v != "" {
		c.Server.Reuseport = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("WWZ_SCENARIOS_DIR"); v != "" {
		c.Scenarios.Dir = v
	}
}

// ApplyDefaults fills zero values with sane defaults and a built-in model set
// when none is configured.
func ApplyDefaults(c *Config) {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8000"
	}
	if c.Admin.Listen == "" {
		c.Admin.Listen = ":9000"
	}
	if c.Server.Concurrency == 0 {
		c.Server.Concurrency = 256 * 1024
	}
	if c.Server.MaxRequestBodyMB == 0 {
		c.Server.MaxRequestBodyMB = 4
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 60 * time.Second
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = 120 * time.Second
	}
	if c.Server.Auth == "" {
		c.Server.Auth = "keys"
	}
	if c.Server.RequestHeaders == nil {
		t := true
		c.Server.RequestHeaders = &t
	}
	if c.Server.MaxChunksPerStream == 0 {
		c.Server.MaxChunksPerStream = 100000
	}
	if c.Server.PingInterval == 0 {
		c.Server.PingInterval = 15 * time.Second
	}
	if c.Store.Kind == "" {
		c.Store.Kind = "memory"
	}
	if c.Store.OnDown == "" {
		c.Store.OnDown = "local"
	}
	if c.Store.ReadTimeout == 0 {
		c.Store.ReadTimeout = 5 * time.Millisecond
	}
	if c.Store.Prefix == "" {
		c.Store.Prefix = "wwz"
	}
	if c.Store.EventMaxLen == 0 {
		c.Store.EventMaxLen = 1_000_000
	}
	if c.Store.EventSample == 0 {
		c.Store.EventSample = 1
	}
	if c.Chaos.ScopeHeader == "" {
		c.Chaos.ScopeHeader = "x-test-id"
	}
	if c.Scenarios.Builtin == nil {
		t := true
		c.Scenarios.Builtin = &t
	}
	if c.Scenarios.OnNoMatch == "" {
		c.Scenarios.OnNoMatch = "fallback"
	}
	if c.Scenarios.ConversationID == "" {
		c.Scenarios.ConversationID = "header"
	}
	if len(c.Scenarios.Select) == 0 {
		c.Scenarios.Select = []string{"header", "metadata", "marker", "model_suffix", "match"}
	}
	if c.Metrics.Enabled == nil {
		t := true
		c.Metrics.Enabled = &t
	}
	if c.Metrics.MaxSeries == 0 {
		c.Metrics.MaxSeries = 5000
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Logging.Sample == 0 {
		c.Logging.Sample = 1
	}
	if c.Resilience.Track == nil {
		t := true
		c.Resilience.Track = &t
	}
	if c.Resilience.MaxReports == 0 {
		c.Resilience.MaxReports = 100
	}
	if c.Abuse.StrikeTTL == 0 {
		c.Abuse.StrikeTTL = 30 * 24 * time.Hour
	}
	if c.Profiles.Latency == nil {
		c.Profiles.Latency = map[string]LatencyProfile{}
	}
	if c.Profiles.Errors == nil {
		c.Profiles.Errors = map[string]ErrorProfile{}
	}
	if c.Profiles.Generation == nil {
		c.Profiles.Generation = map[string]GenerationProfile{}
	}
	if _, ok := c.Profiles.Latency["default"]; !ok {
		c.Profiles.Latency["default"] = LatencyProfile{}
	}
	if _, ok := c.Profiles.Latency["fast"]; !ok {
		c.Profiles.Latency["fast"] = LatencyProfile{}
	}
	if _, ok := c.Profiles.Latency["gpt-4o-like"]; !ok {
		c.Profiles.Latency["gpt-4o-like"] = LatencyProfile{
			TTFT: &Dist{Kind: "normal", Mean: 350 * time.Millisecond, StdDev: 80 * time.Millisecond},
			ITL:  &Dist{Kind: "normal", Mean: 12 * time.Millisecond, StdDev: 3 * time.Millisecond},
		}
	}
	if _, ok := c.Profiles.Latency["llama-70b-like"]; !ok {
		c.Profiles.Latency["llama-70b-like"] = LatencyProfile{
			TTFT:       &Dist{Kind: "normal", Mean: 600 * time.Millisecond, StdDev: 150 * time.Millisecond},
			ITL:        &Dist{Kind: "normal", Mean: 25 * time.Millisecond, StdDev: 6 * time.Millisecond},
			LoadFactor: 2,
		}
	}
	if _, ok := c.Profiles.Latency["deepseek-r1-like"]; !ok {
		c.Profiles.Latency["deepseek-r1-like"] = LatencyProfile{
			TTFT:         &Dist{Kind: "normal", Mean: 900 * time.Millisecond, StdDev: 200 * time.Millisecond},
			ReasoningITL: &Dist{Kind: "normal", Mean: 15 * time.Millisecond, StdDev: 4 * time.Millisecond},
			ITL:          &Dist{Kind: "normal", Mean: 20 * time.Millisecond, StdDev: 5 * time.Millisecond},
		}
	}
	if _, ok := c.Profiles.Latency["slow-flaky"]; !ok {
		c.Profiles.Latency["slow-flaky"] = LatencyProfile{
			TTFT: &Dist{Kind: "lognormal", Median: 800 * time.Millisecond, P99: 8 * time.Second},
			ITL:  &Dist{Kind: "lognormal", Median: 30 * time.Millisecond, P99: 400 * time.Millisecond},
		}
	}
	if _, ok := c.Profiles.Errors["default"]; !ok {
		c.Profiles.Errors["default"] = ErrorProfile{}
	}
	if _, ok := c.Profiles.Errors["none"]; !ok {
		c.Profiles.Errors["none"] = ErrorProfile{}
	}
	if _, ok := c.Profiles.Errors["flaky"]; !ok {
		c.Profiles.Errors["flaky"] = ErrorProfile{Rate: 5, Types: []string{"rate_limit", "server_error", "overloaded"}, MidStreamRate: 2, MidStreamTypes: []string{"stream_truncated", "stream_error_event"}}
	}
	if _, ok := c.Profiles.Generation["default"]; !ok {
		c.Profiles.Generation["default"] = GenerationProfile{}
	}
	for name, g := range c.Profiles.Generation {
		c.Profiles.Generation[name] = generationDefaults(g)
	}
	if len(c.Drivers) == 0 {
		c.Drivers = defaultMounts()
	}
	if len(c.Models) == 0 {
		t := true
		c.Models = []ModelConfig{
			{ID: "wwz-fast", OwnedBy: "white-wizard", Protocols: []string{DialectOpenAI, DialectAnthropic}, ContextWindow: 128000, Capabilities: Capabilities{Tools: &t, Reasoning: &t, JSONSchema: &t}, Latency: "fast", Errors: "none", Generation: "default", Pricing: Pricing{InputPerM: 0.5, OutputPerM: 1.5}},
			{ID: "wwz-realistic", OwnedBy: "white-wizard", Protocols: []string{DialectOpenAI, DialectAnthropic}, ContextWindow: 128000, Capabilities: Capabilities{Tools: &t, Reasoning: &t}, Latency: "gpt-4o-like", Errors: "none", Generation: "default", Pricing: Pricing{InputPerM: 2.5, OutputPerM: 10}},
			{ID: "wwz-flaky", OwnedBy: "white-wizard", Protocols: []string{DialectOpenAI, DialectAnthropic}, ContextWindow: 32000, Capabilities: Capabilities{Tools: &t, Reasoning: &t}, Latency: "slow-flaky", Errors: "flaky", Generation: "default", Pricing: Pricing{InputPerM: 1, OutputPerM: 3}},
		}
	}
	for i := range c.Models {
		m := &c.Models[i]
		if m.ContextWindow == 0 {
			m.ContextWindow = 128000
		}
		if m.MaxOutput == 0 {
			m.MaxOutput = 16384
		}
		if m.OwnedBy == "" {
			m.OwnedBy = "white-wizard"
		}
		if m.Created == 0 {
			m.Created = 1735689600 // 2025-01-01
		}
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		if m.Tokenizer == "" {
			m.Tokenizer = TokenizerAuto
		}
		if m.Latency == nil {
			m.Latency = "default"
		}
		if m.Errors == nil {
			m.Errors = "default"
		}
		if m.Generation == nil {
			m.Generation = "default"
		}
	}
	if c.Server.Auth == "keys" && len(c.Keys) == 0 {
		// Keys are required in "keys" mode; ship test keys so the server is
		// usable out of the box (like Axium's sk-mock-key-01..10).
		for i := 1; i <= 10; i++ {
			c.Keys = append(c.Keys, KeyConfig{Key: fmt.Sprintf("wwz-key-%02d", i), Label: fmt.Sprintf("test-%02d", i), Account: "acct-test", Project: "proj-test", Tier: "default"})
		}
	}
	for i := range c.Keys {
		k := &c.Keys[i]
		if k.Label == "" {
			k.Label = "key-" + strconv.Itoa(i+1)
		}
		if k.Status == "" {
			k.Status = "active"
		}
		if k.Account == "" {
			k.Account = "acct-" + k.Label
		}
	}
	// A key without tier takes the tier of its account (ADR 0051), so the
	// keys of one account never disagree because of a default.
	for i := range c.Keys {
		if k := &c.Keys[i]; k.Tier == "" {
			k.Tier = c.AccountTier(k.Account)
		}
	}
	if c.Tiers == nil {
		c.Tiers = map[string]Tier{}
	}
	if _, ok := c.Tiers["default"]; !ok {
		c.Tiers["default"] = Tier{}
	}
}

// Resolved profile lookups.

// LatencyFor returns the resolved latency profile of a model (inline or named).
func (c *Config) LatencyFor(m *ModelConfig) (LatencyProfile, error) {
	return resolveCached(c.inline, inlineLatency, m.Latency, c.Profiles.Latency, nil)
}

// ErrorsFor returns the resolved error profile of a model.
func (c *Config) ErrorsFor(m *ModelConfig) (ErrorProfile, error) {
	return resolveCached(c.inline, inlineErrors, m.Errors, c.Profiles.Errors, nil)
}

// GenerationFor returns the resolved generation profile of a model.
func (c *Config) GenerationFor(m *ModelConfig) (GenerationProfile, error) {
	// Inline profiles need defaults too (cached with them); a named one
	// already has them and passes through without allocating (task 268).
	return resolveCached(c.inline, inlineGeneration, m.Generation, c.Profiles.Generation, generationDefaults)
}

// toolCallsProbabilityDefaults holds the three tool-call probabilities of a
// profile without them (spec 003). They are filled per field when the YAML
// is decoded, so an explicit 0 stays 0 (task 463): `tool_calls:
// {probability: 0}` never calls a tool.
var toolCallsProbabilityDefaults = ToolCallsGen{Probability: 35, ExtraCallProbability: 45, OptionalParamProbability: 50}

// UnmarshalYAML decodes a tool_calls block with the same strict decoder and
// gives the probabilities it leaves out their defaults.
func (t *ToolCallsGen) UnmarshalYAML(decode func(any) error) error {
	type plain ToolCallsGen
	var p plain
	if err := decode(&p); err != nil {
		return err
	}
	var keys map[string]any
	_ = decode(&keys) // the block decoded as a struct, so it is a map
	d := toolCallsProbabilityDefaults
	if _, ok := keys["probability"]; !ok {
		p.Probability = d.Probability
	}
	if _, ok := keys["extra_call_probability"]; !ok {
		p.ExtraCallProbability = d.ExtraCallProbability
	}
	if _, ok := keys["optional_param_probability"]; !ok {
		p.OptionalParamProbability = d.OptionalParamProbability
	}
	*t = ToolCallsGen(p)
	return nil
}

// generationDefaults fills the unset fields of a generation profile. The
// tool-call settings are copied before a change, so a profile shared by
// concurrent requests is never written.
func generationDefaults(g GenerationProfile) GenerationProfile {
	if g.Mode == "" {
		g.Mode = "random"
	}
	if g.OutputTokens == nil {
		g.OutputTokens = &TokenDist{Dist: "histogram", Buckets: [][]float64{{50, 0.4}, {200, 0.4}, {800, 0.2}}}
	}
	if g.ReasoningTokens == nil {
		g.ReasoningTokens = &ReasoningTokens{Min: 30, Max: 200, Probability: 100}
	}
	if g.ToolCalls == nil {
		tc := toolCallsProbabilityDefaults
		g.ToolCalls = &tc
	}
	if tc := g.ToolCalls; tc.MaxInt == 0 || tc.MaxNumber == 0 || tc.MinArrayLen == 0 || tc.MaxArrayLen == 0 {
		cp := *tc
		if cp.MaxInt == 0 {
			cp.MaxInt = 100
		}
		if cp.MaxNumber == 0 {
			cp.MaxNumber = 100
		}
		if cp.MinArrayLen == 0 {
			cp.MinArrayLen = 1
		}
		if cp.MaxArrayLen == 0 {
			cp.MaxArrayLen = 5
		}
		g.ToolCalls = &cp
	}
	// reasoning_field stays empty when unset: the driver's own default
	// applies, and a filled default could not be told from a value the
	// operator wrote (the config is cloned through YAML).
	if g.ToolArgsChunkBytes == 0 {
		g.ToolArgsChunkBytes = 24
	}
	return g
}

// inlineProfiles holds the inline profiles already converted, keyed by the
// kind of profile and the identity of the YAML map they came from. An
// inline profile used to go through a YAML marshal and unmarshal on every
// request (task 270); the maps of a parsed Config are not written, and the
// key keeps its map alive, so an address is never reused for another map.
type inlineProfiles struct{ m sync.Map }

type inlineKind uint8

const (
	inlineLatency inlineKind = iota
	inlineErrors
	inlineGeneration
)

type inlineKey struct {
	kind inlineKind
	m    unsafe.Pointer
}

// resolveCached is resolveProfile followed by fix (when not nil), with the
// result for an inline map cached in cache.
func resolveCached[T any](cache *inlineProfiles, kind inlineKind, v any, named map[string]T, fix func(T) T) (T, error) {
	resolve := func() (T, error) {
		p, err := resolveProfile(v, named)
		if err != nil || fix == nil {
			return p, err
		}
		return fix(p), nil
	}
	rv := reflect.ValueOf(v)
	if cache == nil || rv.Kind() != reflect.Map || rv.IsNil() {
		return resolve()
	}
	key := inlineKey{kind: kind, m: rv.UnsafePointer()}
	if p, ok := cache.m.Load(key); ok {
		return p.(T), nil
	}
	p, err := resolve()
	if err == nil {
		cache.m.Store(key, p)
	}
	return p, err
}

func resolveProfile[T any](v any, named map[string]T) (T, error) {
	var zero T
	switch x := v.(type) {
	case nil:
		p, ok := named["default"]
		if !ok {
			return zero, errors.New("no default profile")
		}
		return p, nil
	case string:
		p, ok := named[x]
		if !ok {
			return zero, fmt.Errorf("unknown profile %q", x)
		}
		return p, nil
	default:
		b, err := yaml.Marshal(x)
		if err != nil {
			return zero, err
		}
		var p T
		if err := yaml.UnmarshalWithOptions(b, &p, yaml.Strict()); err != nil {
			return zero, err
		}
		return p, nil
	}
}

func mergeLimits(base, over Limits) Limits {
	if over.RPS != 0 {
		base.RPS = over.RPS
	}
	if over.Burst != 0 {
		base.Burst = over.Burst
	}
	if over.RPM != 0 {
		base.RPM = over.RPM
	}
	if over.RPD != 0 {
		base.RPD = over.RPD
	}
	if over.TPM != 0 {
		base.TPM = over.TPM
	}
	if over.ITPM != 0 {
		base.ITPM = over.ITPM
	}
	if over.OTPM != 0 {
		base.OTPM = over.OTPM
	}
	if over.TPD != 0 {
		base.TPD = over.TPD
	}
	if over.Concurrency != 0 {
		base.Concurrency = over.Concurrency
	}
	if over.MaxQueue != 0 {
		base.MaxQueue = over.MaxQueue
	}
	if over.QueueTimeout != 0 {
		base.QueueTimeout = over.QueueTimeout
	}
	if over.SpendMonthlyUSD != 0 {
		base.SpendMonthlyUSD = over.SpendMonthlyUSD
	}
	if over.SpendSoftUSD != 0 {
		base.SpendSoftUSD = over.SpendSoftUSD
	}
	if len(over.SpendMonthlyModelsUSD) > 0 {
		base.SpendMonthlyModelsUSD = over.SpendMonthlyModelsUSD
	}
	if over.CreditsUSD != nil {
		base.CreditsUSD = over.CreditsUSD
	}
	if over.QuotaUSD != nil {
		base.QuotaUSD = over.QuotaUSD
	}
	if over.Algorithm != "" {
		base.Algorithm = over.Algorithm
	}
	if over.TPMReserve != "" {
		base.TPMReserve = over.TPMReserve
	}
	if over.ExcludeCacheRead {
		base.ExcludeCacheRead = true
	}
	if over.OnLimit != "" {
		base.OnLimit = over.OnLimit
	}
	if over.Mode != "" {
		base.Mode = over.Mode
	}
	if over.AfterN != 0 {
		base.AfterN = over.AfterN
	}
	if over.Window != nil {
		base.Window = over.Window
	}
	if over.Ramp != nil {
		base.Ramp = over.Ramp
	}
	return base
}
