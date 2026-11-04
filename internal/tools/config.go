package tools

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/goccy/go-yaml"
)

// Defaults of tools.yaml (spec 017).
const (
	DefaultListen      = ":9200"
	DefaultTimeout     = 30 * time.Second
	DefaultMaxSessions = 100000
	DefaultSessionTTL  = 30 * time.Minute
)

// Range is a uniform latency interval [Min, Max].
type Range struct {
	Min, Max time.Duration
}

// Rates are injected outcome rates in percent (0–100).
type Rates struct {
	Error, Timeout, InvalidPayload float64
}

// RateOverrides replace the global Rates field by field (nil = inherit).
type RateOverrides struct {
	Error, Timeout, InvalidPayload *float64
}

// ToolConfig is the per-tool section tools.<name>.
type ToolConfig struct {
	Latency  *Range        // nil = global latency
	Outcomes RateOverrides // replaces the global rates field by field
	FailRate *float64      // run_command only: % of failing test runs
}

// Config is the wwz-tools configuration (tools.yaml + env + flags).
type Config struct {
	Listen      string
	Seed        int64 // 0 = random per session
	Token       string
	Timeout     time.Duration
	MaxSessions int
	SessionTTL  time.Duration
	Latency     Range
	Outcomes    Rates
	Tools       map[string]ToolConfig
}

// DefaultConfig returns the defaults: the per-tool latencies and the
// run_command fail_rate of the spec 017 example are the defaults.
func DefaultConfig() Config {
	fail := 10.0
	return Config{
		Listen:      DefaultListen,
		Timeout:     DefaultTimeout,
		MaxSessions: DefaultMaxSessions,
		SessionTTL:  DefaultSessionTTL,
		Latency:     Range{5 * time.Millisecond, 50 * time.Millisecond},
		Tools: map[string]ToolConfig{
			ToolRunCommand: {Latency: &Range{200 * time.Millisecond, 2 * time.Second}, FailRate: &fail},
			ToolWebSearch:  {Latency: &Range{100 * time.Millisecond, 800 * time.Millisecond}},
			ToolFetchURL:   {Latency: &Range{50 * time.Millisecond, 500 * time.Millisecond}},
		},
	}
}

// FieldError is one configuration failure, naming the field path
// (tools.fetch_url.outcomes.error) or the variable (WWZ_TOOLS_SEED).
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Flags are the command-line overrides. "" / nil = not given.
type Flags struct {
	Config string
	Listen string
	Seed   *int64
}

// Load builds the configuration with precedence flag > env > file > default.
// getenv nil = os.Getenv. Failures are joined *FieldError values (or a read
// error for the file).
func Load(f Flags, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := DefaultConfig()
	if f.Config != "" {
		data, err := os.ReadFile(filepath.Clean(f.Config))
		if err != nil {
			return Config{}, fmt.Errorf("tools config: %w", err)
		}
		if cfg, err = parse(data); err != nil {
			return Config{}, err
		}
	}
	if v := getenv("WWZ_TOOLS_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := getenv("WWZ_TOOLS_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, &FieldError{"WWZ_TOOLS_SEED", fmt.Sprintf("inteiro inválido %q", v)}
		}
		cfg.Seed = n
	}
	cfg.Token = getenv("WWZ_TOOLS_TOKEN")
	if f.Listen != "" {
		cfg.Listen = f.Listen
	}
	if f.Seed != nil {
		cfg.Seed = *f.Seed
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks ranges; failures are joined *FieldError values.
func (c *Config) Validate() error {
	var errs []error
	fail := func(field, format string, a ...any) {
		errs = append(errs, &FieldError{field, fmt.Sprintf(format, a...)})
	}
	if c.Listen == "" {
		fail("listen", "obrigatório")
	}
	if c.Timeout <= 0 {
		fail("timeout", "deve ser > 0")
	}
	if c.MaxSessions < 1 {
		fail("max_sessions", "deve ser >= 1")
	}
	if c.SessionTTL <= 0 {
		fail("session_ttl", "deve ser > 0")
	}
	checkRange := func(field string, r Range) {
		if r.Min < 0 || r.Max < r.Min {
			fail(field, "exige 0 <= min <= max (min %s, max %s)", r.Min, r.Max)
		}
	}
	checkRates := func(field string, r Rates) {
		for _, x := range []struct {
			name string
			v    float64
		}{{"error", r.Error}, {"timeout", r.Timeout}, {"invalid_payload", r.InvalidPayload}} {
			if x.v < 0 || x.v > 100 || math.IsNaN(x.v) {
				fail(field+"."+x.name, "fora de 0–100: %v", x.v)
			}
		}
		if r.Error+r.Timeout+r.InvalidPayload > 100 {
			fail(field, "soma das taxas passa de 100")
		}
	}
	checkRange("latency", c.Latency)
	checkRates("outcomes", c.Outcomes)
	names := make([]string, 0, len(c.Tools))
	for name := range c.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tc := c.Tools[name]
		p := "tools." + name
		if toolIndex(name) < 0 {
			fail(p, "ferramenta desconhecida")
			continue
		}
		if tc.Latency != nil {
			checkRange(p+".latency", *tc.Latency)
		}
		checkRates(p+".outcomes", c.Outcomes.merge(tc.Outcomes))
		if tc.FailRate != nil {
			if name != ToolRunCommand {
				fail(p+".fail_rate", "só vale para run_command")
			} else if *tc.FailRate < 0 || *tc.FailRate > 100 || math.IsNaN(*tc.FailRate) {
				fail(p+".fail_rate", "fora de 0–100: %v", *tc.FailRate)
			}
		}
	}
	return errors.Join(errs...)
}

// merge applies per-tool overrides over the global rates.
func (r Rates) merge(o RateOverrides) Rates {
	if o.Error != nil {
		r.Error = *o.Error
	}
	if o.Timeout != nil {
		r.Timeout = *o.Timeout
	}
	if o.InvalidPayload != nil {
		r.InvalidPayload = *o.InvalidPayload
	}
	return r
}

// toolCfg is the effective configuration of one tool.
type toolCfg struct {
	latency  Range
	rates    Rates
	failRate float64
}

func (c *Config) effective(name string) toolCfg {
	tc := c.Tools[name]
	e := toolCfg{latency: c.Latency, rates: c.Outcomes.merge(tc.Outcomes)}
	if tc.Latency != nil {
		e.latency = *tc.Latency
	}
	if tc.FailRate != nil {
		e.failRate = *tc.FailRate
	}
	return e
}

// Parse decodes tools.yaml over DefaultConfig. Unknown fields, wrong types
// and out-of-range values give joined *FieldError values with the field path.
func Parse(data []byte) (Config, error) {
	cfg, err := parse(data)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parse decodes tools.yaml over DefaultConfig without range validation.
func parse(data []byte) (Config, error) {
	cfg := DefaultConfig()
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("tools config: %w", err)
	}
	p := &parser{}
	if raw != nil {
		p.top(raw, &cfg)
	}
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type parser struct {
	errs []error
}

func (p *parser) fail(field, format string, a ...any) {
	p.errs = append(p.errs, &FieldError{field, fmt.Sprintf(format, a...)})
}

// object returns v as a map with sorted keys, or reports a type error.
func (p *parser) object(field string, v any) (map[string]any, []string) {
	m, ok := v.(map[string]any)
	if !ok {
		p.fail(field, "esperado um objeto")
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m, keys
}

func join(prefix, key string) string { return prefix + "." + key }

func (p *parser) top(v any, c *Config) {
	m, keys := p.object("(raiz)", v)
	for _, k := range keys {
		x := m[k]
		switch k {
		case "listen":
			c.Listen = p.str(k, x)
		case "seed":
			c.Seed = p.integer(k, x)
		case "timeout":
			c.Timeout = p.duration(k, x)
		case "max_sessions":
			n := p.integer(k, x)
			if n > math.MaxInt32 {
				p.fail(k, "grande demais: %d", n)
			}
			c.MaxSessions = int(n)
		case "session_ttl":
			c.SessionTTL = p.duration(k, x)
		case "latency":
			c.Latency = p.latency(k, x, c.Latency)
		case "outcomes":
			o := p.rates(k, x)
			c.Outcomes = c.Outcomes.merge(o)
		case "tools":
			p.tools(k, x, c)
		default:
			p.fail(k, "campo desconhecido")
		}
	}
}

func (p *parser) tools(field string, v any, c *Config) {
	m, keys := p.object(field, v)
	for _, name := range keys {
		path := join(field, name)
		if toolIndex(name) < 0 {
			p.fail(path, "ferramenta desconhecida")
			continue
		}
		tc := c.Tools[name]
		tm, tkeys := p.object(path, m[name])
		for _, k := range tkeys {
			fp := join(path, k)
			switch {
			case k == "latency":
				base := c.Latency
				if tc.Latency != nil {
					base = *tc.Latency
				}
				r := p.latency(fp, tm[k], base)
				tc.Latency = &r
			case k == "outcomes":
				o := p.rates(fp, tm[k])
				tc.Outcomes = mergeOverrides(tc.Outcomes, o)
			case k == "fail_rate" && name == ToolRunCommand:
				f := p.number(fp, tm[k])
				tc.FailRate = &f
			default:
				p.fail(fp, "campo desconhecido")
			}
		}
		if c.Tools == nil {
			c.Tools = map[string]ToolConfig{}
		}
		c.Tools[name] = tc
	}
}

func mergeOverrides(a, b RateOverrides) RateOverrides {
	if b.Error != nil {
		a.Error = b.Error
	}
	if b.Timeout != nil {
		a.Timeout = b.Timeout
	}
	if b.InvalidPayload != nil {
		a.InvalidPayload = b.InvalidPayload
	}
	return a
}

func (p *parser) latency(field string, v any, base Range) Range {
	m, keys := p.object(field, v)
	for _, k := range keys {
		switch k {
		case "min":
			base.Min = p.duration(join(field, k), m[k])
		case "max":
			base.Max = p.duration(join(field, k), m[k])
		default:
			p.fail(join(field, k), "campo desconhecido")
		}
	}
	return base
}

func (p *parser) rates(field string, v any) RateOverrides {
	var o RateOverrides
	m, keys := p.object(field, v)
	for _, k := range keys {
		f := p.number(join(field, k), m[k])
		switch k {
		case "error":
			o.Error = &f
		case "timeout":
			o.Timeout = &f
		case "invalid_payload":
			o.InvalidPayload = &f
		default:
			p.fail(join(field, k), "campo desconhecido")
		}
	}
	return o
}

func (p *parser) str(field string, v any) string {
	s, ok := v.(string)
	if !ok {
		p.fail(field, "esperado texto")
	}
	return s
}

func (p *parser) duration(field string, v any) time.Duration {
	s, ok := v.(string)
	if !ok {
		p.fail(field, "esperada uma duração (ex.: 30s)")
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		p.fail(field, "duração inválida %q", s)
	}
	return d
}

func (p *parser) integer(field string, v any) int64 {
	switch n := v.(type) {
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n)
		}
	case int64:
		return n
	}
	p.fail(field, "esperado um inteiro")
	return 0
}

func (p *parser) number(field string, v any) float64 {
	switch n := v.(type) {
	case uint64:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	p.fail(field, "esperado um número")
	return 0
}
