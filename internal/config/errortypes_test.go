package config

import (
	"strings"
	"testing"
)

func registerFakeTypes(t *testing.T, pre, mid func(string) bool) {
	t.Helper()
	RegisterProfileTypes(pre, mid)
	t.Cleanup(func() { profileTypes.Store(nil) })
}

func TestErrorTypesValidated(t *testing.T) {
	registerFakeTypes(t,
		func(s string) bool { return s != "nope" },
		func(s string) bool { return s != "nope" })
	cases := []struct{ name, yaml, wantErr string }{
		{"ok", "profiles: {errors: {p: {types: [rate_limit], mid_stream_types: [stream_truncated]}}}", ""},
		{"profile type", "profiles: {errors: {p: {types: [rate_limit, nope]}}}", `profiles.errors.p.types[1]: unknown error type "nope"`},
		{"profile mid", "profiles: {errors: {p: {mid_stream_types: [nope]}}}", `profiles.errors.p.mid_stream_types[0]: unknown mid-stream type "nope"`},
		{"inline model", "models: [{id: m, errors: {types: [nope]}}]", `models[m].errors.types[0]: unknown error type "nope"`},
		{"key refs bad profile", "profiles: {errors: {p: {types: [nope]}}}\nkeys: [{key: k, errors: p}]", "profiles.errors.p.types[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { expectParse(t, tc.yaml, tc.wantErr) })
	}
}

func TestErrorTypesNilChecks(t *testing.T) {
	registerFakeTypes(t, nil, nil)
	expectParse(t, "profiles: {errors: {p: {types: [x], mid_stream_types: [y]}}}", "")
}

func TestErrorTypesUnregistered(t *testing.T) {
	expectParse(t, "profiles: {errors: {p: {types: [x]}}}", "")
}

func TestModelReasoningField(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"inline without field", "models: [{id: m, protocols: [openai], generation: {mode: echo}}]", ""},
		{"default untouched", "models: [{id: m, protocols: [openai]}]", ""},
		{"inline hidden on openai", "models: [{id: m, protocols: [openai], generation: {reasoning_field: hidden}}]", ""},
		{
			"inline reasoning_content on openai", "models: [{id: m, protocols: [openai], generation: {reasoning_field: reasoning_content}}]",
			`models[m].generation.reasoning_field="reasoning_content" is invalid for driver openai`,
		},
		{
			"named explicit on openai", "profiles: {generation: {g: {reasoning_field: reasoning}}}\nmodels: [{id: m, protocols: [openai], generation: g}]",
			`models[m].generation (profile "g").reasoning_field="reasoning" is invalid for driver openai`,
		},
		{"named explicit on opencode only", "profiles: {generation: {g: {reasoning_field: reasoning_content}}}\nmodels: [{id: m, drivers: [opencode], generation: g}]", ""},
		{"azure", "drivers: [{name: azure}]\nmodels: [{id: m, drivers: [azure], generation: {reasoning_field: inline_tag}}]", "invalid for driver azure"},
		{"anthropic ignores", "models: [{id: m, protocols: [anthropic], generation: {reasoning_field: reasoning}}]", ""},
		{"bogus value", "models: [{id: m, drivers: [opencode], generation: {reasoning_field: thoughts}}]", `reasoning_field="thoughts" is invalid (allowed`},
		{"named unused profile", "profiles: {generation: {g: {reasoning_field: reasoning}}}\nmodels: [{id: m, protocols: [openai]}]", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { expectParse(t, tc.yaml, tc.wantErr) })
	}
}

// ApplyDefaults runs again on a parsed Config (serve, reload): the filled
// default must not be taken for a value the operator wrote.
func TestModelReasoningFieldSurvivesSecondDefaults(t *testing.T) {
	c, err := Parse([]byte("models: [{id: m, protocols: [openai]}]"))
	if err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(c)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestErrorWeightsValidated(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"ok listed", "profiles: {errors: {p: {types: [rate_limit, overloaded], weights: {rate_limit: 5}}}}", ""},
		{"ok default list", "profiles: {errors: {p: {weights: {bad_gateway: 2, context_length: 0.5}}}}", ""},
		{"ok mid", "profiles: {errors: {p: {mid_stream_types: [stream_hang], weights: {stream_hang: 3}}}}", ""},
		{"slow ok", "profiles: {errors: {p: {slow_headers_delay: 2s}}}", ""},
		{"slow negative", "profiles: {errors: {p: {slow_headers_delay: -1s}}}", "slow_headers_delay"},
		{"typo", "profiles: {errors: {p: {weights: {rate_limt: 5}}}}", "profiles.errors.p.weights[rate_limt]"},
		{"outside types", "profiles: {errors: {p: {types: [overloaded], weights: {rate_limit: 5}}}}", "profiles.errors.p.weights[rate_limit]"},
		{"zero", "profiles: {errors: {p: {weights: {rate_limit: 0}}}}", "profiles.errors.p.weights[rate_limit]=0: weight must be finite and > 0"},
		{"negative", "profiles: {errors: {p: {weights: {rate_limit: -1}}}}", "weight must be finite and > 0"},
		{"inf", "profiles: {errors: {p: {weights: {rate_limit: .inf}}}}", "weight must be finite and > 0"},
		{"nan", "profiles: {errors: {p: {weights: {rate_limit: .nan}}}}", "weight must be finite and > 0"},
		{"inline model", "models: [{id: m, errors: {weights: {nope: 1}}}]", "models[m].errors.weights[nope]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { expectParse(t, tc.yaml, tc.wantErr) })
	}
}

func expectParse(t *testing.T, y, wantErr string) {
	t.Helper()
	_, err := Parse([]byte(y))
	if wantErr == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want %q", err, wantErr)
	}
}
