package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync/atomic"
)

// profileTypeChecks validate the names in errors.types and
// errors.mid_stream_types; internal/chaos registers them so config does not
// import the list of injectable types.
type profileTypeChecks struct{ pre, mid func(string) bool }

var profileTypes atomic.Pointer[profileTypeChecks]

// RegisterProfileTypes sets the functions Validate uses to accept the names
// in an error profile's types (pre) and mid_stream_types (mid). Nil
// functions accept any name.
func RegisterProfileTypes(pre, mid func(string) bool) {
	profileTypes.Store(&profileTypeChecks{pre: pre, mid: mid})
}

// DefaultErrorTypes is the list errors.types falls back to when empty
// (spec 009); DefaultMidStreamTypes is the one for mid_stream_types.
var (
	DefaultErrorTypes     = []string{"rate_limit", "server_error", "overloaded", "bad_gateway", "gateway_timeout", "invalid_request", "model_not_found", "context_length"}
	DefaultMidStreamTypes = []string{"stream_error_event", "stream_truncated", "stream_hang"}
)

// validateWeights rejects a weights key that is in neither the profile's
// types nor its mid-stream types (or their defaults) and a weight that is
// not finite and positive (ADR 0059).
func validateWeights(path string, p ErrorProfile) error {
	types, mid := p.Types, p.MidStreamTypes
	if len(types) == 0 {
		types = DefaultErrorTypes
	}
	if len(mid) == 0 {
		mid = DefaultMidStreamTypes
	}
	for _, k := range slices.Sorted(maps.Keys(p.Weights)) {
		if !slices.Contains(types, k) && !slices.Contains(mid, k) {
			return fmt.Errorf("config: %s.weights[%s]: type is not in types (allowed: %s)", path, k, strings.Join(slices.Concat(types, mid), ", "))
		}
		if w := p.Weights[k]; w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			return fmt.Errorf("config: %s.weights[%s]=%v: weight must be finite and > 0", path, k, w)
		}
	}
	return nil
}

// validateErrorTypes rejects an unknown name in p.Types or p.MidStreamTypes
// and invalid weights; path names the profile in the error.
func validateErrorTypes(path string, p ErrorProfile) error {
	if err := validateWeights(path, p); err != nil {
		return err
	}
	if p.SlowHeadersDelay < 0 {
		return fmt.Errorf("config: %s.slow_headers_delay must not be negative", path)
	}
	chk := profileTypes.Load()
	if chk == nil {
		return nil
	}
	if chk.pre != nil {
		for i, t := range p.Types {
			if !chk.pre(t) {
				return fmt.Errorf("config: %s.types[%d]: unknown error type %q", path, i, t)
			}
		}
	}
	if chk.mid != nil {
		for i, t := range p.MidStreamTypes {
			if !chk.mid(t) {
				return fmt.Errorf("config: %s.mid_stream_types[%d]: unknown mid-stream type %q", path, i, t)
			}
		}
	}
	return nil
}

// validateReasoningField applies to a model's generation profile the rule
// drivers[].options uses (ADR 0046, 0057): the value must be a known one,
// and each mounted openai or azure driver that serves the model accepts
// only hidden. An empty value is the default and always valid.
func (c *Config) validateReasoningField(path, v string, m *ModelConfig) error {
	if v == "" {
		return nil
	}
	if !slices.Contains(reasoningFields, v) {
		return fmt.Errorf("config: %s.reasoning_field=%q is invalid (allowed: %v)", path, v, reasoningFields)
	}
	for _, d := range c.Drivers {
		opts, _ := driverOptionValues(d.Name)
		allowed, ok := opts["reasoning_field"]
		if ok && m.ModelSupports(d.Name) && !slices.Contains(allowed, v) {
			return fmt.Errorf("config: %s.reasoning_field=%q is invalid for driver %s that serves the model (allowed: %v)", path, v, d.Name, allowed)
		}
	}
	return nil
}
