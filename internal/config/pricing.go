package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Pricing per million tokens, USD. Multipliers and Overrides change it by
// time of day, date or context size (spec 003, ADR 0072).
//
// A pricing block is declared when the YAML has one (even pricing: {}) or
// when any field is set; an undeclared Pricing is omitted when the
// configuration is serialized, so GET and POST /admin/config never turn a
// model without pricing into a free one.
type Pricing struct {
	InputPerM      float64           `yaml:"input_per_m"`
	OutputPerM     float64           `yaml:"output_per_m"`
	CacheReadPerM  float64           `yaml:"cache_read_per_m"`
	CacheWritePerM float64           `yaml:"cache_write_per_m"`
	Multipliers    []PriceMultiplier `yaml:"multipliers,omitempty"`
	Overrides      []PriceOverride   `yaml:"overrides,omitempty"`

	declared bool // the YAML has the block
}

// PriceMultiplier is one pricing.multipliers entry: every factor whose When
// matches at the start of the request multiplies the price.
type PriceMultiplier struct {
	When   When    `yaml:"when"`
	Factor float64 `yaml:"factor"`
}

// PriceOverride is one pricing.overrides entry: the first one whose When
// matches replaces the prices it declares.
type PriceOverride struct {
	When           When     `yaml:"when"`
	InputPerM      *float64 `yaml:"input_per_m,omitempty"`
	OutputPerM     *float64 `yaml:"output_per_m,omitempty"`
	CacheReadPerM  *float64 `yaml:"cache_read_per_m,omitempty"`
	CacheWritePerM *float64 `yaml:"cache_write_per_m,omitempty"`
}

// UnmarshalYAML marks the block as declared; unknown keys stay an error.
func (p *Pricing) UnmarshalYAML(b []byte) error {
	switch strings.TrimSpace(string(b)) {
	case "", "null", "~":
		*p = Pricing{}
		return nil
	}
	type plain Pricing
	var q plain
	if err := yaml.UnmarshalWithOptions(b, &q, yaml.Strict()); err != nil {
		return err
	}
	*p = Pricing(q)
	p.declared = true
	return nil
}

// Declared reports whether the model declares a pricing block: the YAML has
// one or a field is set.
func (p Pricing) Declared() bool {
	return p.declared || p.InputPerM != 0 || p.OutputPerM != 0 || p.CacheReadPerM != 0 ||
		p.CacheWritePerM != 0 || len(p.Multipliers) > 0 || len(p.Overrides) > 0
}

// IsZero omits an undeclared pricing when the configuration is serialized.
func (p Pricing) IsZero() bool { return !p.Declared() }

// Declare returns p marked as declared, as if the YAML had the block.
func (p Pricing) Declare() Pricing {
	p.declared = true
	return p
}

// Free reports whether the pricing makes the model free (ADR 0072, item 3):
// declared and without an input price. An absent pricing is paid at US$ 0.
func (p Pricing) Free() bool { return p.Declared() && p.InputPerM == 0 }

// Effective is the price of a request that starts at at with contextTokens
// input tokens (cache included): the first override that matches replaces
// the prices it declares, then the product of the matching multipliers
// scales every price. The result has no multipliers nor overrides.
func (p Pricing) Effective(at time.Time, contextTokens int) Pricing {
	out := Pricing{
		InputPerM: p.InputPerM, OutputPerM: p.OutputPerM,
		CacheReadPerM: p.CacheReadPerM, CacheWritePerM: p.CacheWritePerM,
		declared: p.Declared(),
	}
	at = at.UTC()
	for _, o := range p.Overrides {
		if !o.When.Matches(at, contextTokens) {
			continue
		}
		for _, f := range []struct {
			dst *float64
			src *float64
		}{{&out.InputPerM, o.InputPerM}, {&out.OutputPerM, o.OutputPerM}, {&out.CacheReadPerM, o.CacheReadPerM}, {&out.CacheWritePerM, o.CacheWritePerM}} {
			if f.src != nil {
				*f.dst = *f.src
			}
		}
		break
	}
	factor := 1.0
	for _, m := range p.Multipliers {
		if m.When.Matches(at, contextTokens) {
			factor *= m.Factor
		}
	}
	if factor != 1 {
		out.InputPerM *= factor
		out.OutputPerM *= factor
		out.CacheReadPerM *= factor
		out.CacheWritePerM *= factor
	}
	return out
}

// validate checks the multipliers and overrides of the pricing at path
// (models[i].pricing) and compiles their conditions.
func (p *Pricing) validate(path string) error {
	for j := range p.Multipliers {
		m := &p.Multipliers[j]
		at := fmt.Sprintf("%s.multipliers[%d]", path, j)
		if err := m.When.compile(at, false); err != nil {
			return err
		}
		if !(m.Factor > 0) || math.IsInf(m.Factor, 0) {
			return fmt.Errorf("config: %s.factor must be > 0 and finite, got %v", at, m.Factor)
		}
	}
	for j := range p.Overrides {
		o := &p.Overrides[j]
		at := fmt.Sprintf("%s.overrides[%d]", path, j)
		if err := o.When.compile(at, true); err != nil {
			return err
		}
		if o.InputPerM == nil && o.OutputPerM == nil && o.CacheReadPerM == nil && o.CacheWritePerM == nil {
			return fmt.Errorf("config: %s: no price (want input_per_m, output_per_m, cache_read_per_m or cache_write_per_m)", at)
		}
	}
	return nil
}

// When is the condition of a multiplier or override, in the grammar of the
// real OpenCode catalog (spec 003): utc_days {in}, utc_hours and date with
// gte|gt|lte|lt, context (overrides only), any and all; top-level keys
// combine with AND. Raw keeps the declared map, which the opencode listing
// echoes.
type When struct {
	raw   map[string]any
	match whenFunc // compiled by Validate
}

// whenFunc reports whether a condition holds for a request that starts at
// at (UTC) with contextTokens input tokens.
type whenFunc func(at time.Time, contextTokens int) bool

// UnmarshalYAML keeps the declared map; Validate compiles it.
func (w *When) UnmarshalYAML(b []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return err
	}
	*w = When{raw: raw}
	return nil
}

// MarshalYAML writes the declared map back.
func (w When) MarshalYAML() (any, error) { return w.raw, nil }

// Raw is the condition as declared (nil when absent). Callers must not
// change it.
func (w When) Raw() map[string]any { return w.raw }

// Matches reports whether the condition holds for a request that starts at
// at with contextTokens input tokens. A condition that Validate did not
// compile is compiled now; an invalid one never matches.
func (w When) Matches(at time.Time, contextTokens int) bool {
	f := w.match
	if f == nil {
		var err error
		if f, err = compileWhen(w.raw, "when", true); err != nil {
			return false
		}
	}
	return f(at.UTC(), contextTokens)
}

// compile checks the condition of the entry at path and keeps the result.
// A compiled condition is left alone: its map never changes, and a
// revalidation of a configuration in use must not write to it.
func (w *When) compile(path string, allowContext bool) error {
	if w.match != nil {
		return nil
	}
	if len(w.raw) == 0 {
		return fmt.Errorf("config: %s: missing when", path)
	}
	f, err := compileWhen(w.raw, path+".when", allowContext)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	w.match = f
	return nil
}

// utcDays maps the utc_days names to weekdays.
var utcDays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// compileWhen compiles one condition map; its keys combine with AND.
func compileWhen(raw map[string]any, path string, allowContext bool) (whenFunc, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: empty condition", path)
	}
	var parts []whenFunc
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		v := raw[k]
		at := path + "." + k
		var (
			f   whenFunc
			err error
		)
		switch k {
		case "utc_days":
			f, err = compileDays(v, at)
		case "utc_hours":
			f, err = compileCompare(v, at, parseClock, func(t time.Time, _ int) float64 {
				return float64(t.Sub(t.Truncate(24 * time.Hour)))
			})
		case "date":
			f, err = compileCompare(v, at, parseDate, func(t time.Time, _ int) float64 {
				return float64(t.Truncate(24 * time.Hour).Unix())
			})
		case "context":
			if !allowContext {
				return nil, fmt.Errorf("%s: condition \"context\" is only allowed in overrides", path)
			}
			f, err = compileCompare(v, at, parseTokens, func(_ time.Time, n int) float64 { return float64(n) })
		case "any", "all":
			f, err = compileList(k, v, at, allowContext)
		default:
			return nil, fmt.Errorf("%s: unknown condition %q", path, k)
		}
		if err != nil {
			return nil, err
		}
		parts = append(parts, f)
	}
	return func(t time.Time, n int) bool {
		for _, f := range parts {
			if !f(t, n) {
				return false
			}
		}
		return true
	}, nil
}

// compileDays compiles utc_days: {in: [mon…sun]}.
func compileDays(v any, path string) (whenFunc, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: want {in: [days]}", path)
	}
	for k := range m {
		if k != "in" {
			return nil, fmt.Errorf("%s: unknown operator %q (want in)", path, k)
		}
	}
	list, ok := m["in"].([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s.in: want a non-empty list of days (mon, tue, wed, thu, fri, sat, sun)", path)
	}
	var days [7]bool
	for _, d := range list {
		s, _ := d.(string)
		wd, ok := utcDays[s]
		if !ok {
			return nil, fmt.Errorf("%s.in: unknown day %v (want mon, tue, wed, thu, fri, sat or sun)", path, d)
		}
		days[wd] = true
	}
	return func(t time.Time, _ int) bool { return days[t.Weekday()] }, nil
}

// compareOps are the operators of utc_hours, date and context.
var compareOps = map[string]func(a, b float64) bool{
	"gte": func(a, b float64) bool { return a >= b },
	"gt":  func(a, b float64) bool { return a > b },
	"lte": func(a, b float64) bool { return a <= b },
	"lt":  func(a, b float64) bool { return a < b },
}

// compileCompare compiles {gte|gt|lte|lt: value}: parse turns each value
// into the number measure reads from the request; operators combine with
// AND.
func compileCompare(v any, path string, parse func(any) (float64, error), measure func(time.Time, int) float64) (whenFunc, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("%s: want {gte|gt|lte|lt: value}", path)
	}
	type cmp struct {
		op  func(a, b float64) bool
		val float64
	}
	var cmps []cmp
	for _, k := range slices.Sorted(maps.Keys(m)) {
		op, ok := compareOps[k]
		if !ok {
			return nil, fmt.Errorf("%s: unknown operator %q (want gte, gt, lte or lt)", path, k)
		}
		val, err := parse(m[k])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", path, k, err)
		}
		cmps = append(cmps, cmp{op, val})
	}
	return func(t time.Time, n int) bool {
		x := measure(t, n)
		for _, c := range cmps {
			if !c.op(x, c.val) {
				return false
			}
		}
		return true
	}, nil
}

// compileList compiles any (OR) and all (AND).
func compileList(kind string, v any, path string, allowContext bool) (whenFunc, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s: want a non-empty list of conditions", path)
	}
	fs := make([]whenFunc, len(list))
	for i, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: want a condition", path, i)
		}
		f, err := compileWhen(m, fmt.Sprintf("%s[%d]", path, i), allowContext)
		if err != nil {
			return nil, err
		}
		fs[i] = f
	}
	isAny := kind == "any"
	return func(t time.Time, n int) bool {
		for _, f := range fs {
			if f(t, n) == isAny {
				return isAny
			}
		}
		return !isAny
	}, nil
}

// parseClock reads "HH:MM" as the duration since 00:00 UTC.
func parseClock(v any) (float64, error) {
	s, _ := v.(string)
	if len(s) != 5 || s[2] != ':' || !digits(s[:2]) || !digits(s[3:]) {
		return 0, fmt.Errorf("invalid time %v (want \"HH:MM\")", v)
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("invalid time %q (want \"HH:MM\")", s)
	}
	return float64(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), nil
}

// parseDate reads "YYYY-MM-DD" (UTC) as the Unix time of its 00:00.
func parseDate(v any) (float64, error) {
	s, _ := v.(string) // YAML keeps an unquoted date as text in a map
	d, err := time.Parse(time.DateOnly, s)
	if err != nil || len(s) != len(time.DateOnly) {
		return 0, fmt.Errorf("invalid date %v (want \"YYYY-MM-DD\")", v)
	}
	return float64(d.Unix()), nil
}

// parseTokens reads a token count (a number >= 0).
func parseTokens(v any) (float64, error) {
	var f float64
	switch x := v.(type) {
	case uint64:
		f = float64(x)
	case int64:
		f = float64(x)
	case int:
		f = float64(x)
	case float64:
		f = x
	default:
		return 0, fmt.Errorf("invalid token count %v (want a number >= 0)", v)
	}
	if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid token count %v (want a number >= 0)", v)
	}
	return f, nil
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
