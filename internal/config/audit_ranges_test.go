package config

import (
	"strings"
	"testing"
)

// rangeCase is one row of the range and enum tables of spec 003: every value
// in ok passes, every value in bad fails citing the field.
type rangeCase struct {
	field   string // YAML path below the section ("rpm", "quota_usd.weekly_pct")
	yaml    func(v string) string
	ok, bad []string
}

// limitsAt wraps a limits object into every place spec 003 accepts one:
// tiers.<nome>.limits, accounts[].limits, projects[].limits and keys[].limits.
var limitsAt = map[string]func(limits string) string{
	"tiers":    func(l string) string { return "tiers:\n  t: {limits: " + l + "}\n" },
	"accounts": func(l string) string { return "accounts:\n  - {id: acct, limits: " + l + "}\n" },
	"projects": func(l string) string { return "projects:\n  - {id: proj, limits: " + l + "}\n" },
	"keys":     func(l string) string { return "keys:\n  - {key: k, label: k, limits: " + l + "}\n" },
}

// TestSpec003LimitsRangesAndEnums walks the "Campos de limits" table of spec
// 003 in all four places: a value on the edge passes, a value outside fails
// with the field named (task 385).
func TestSpec003LimitsRangesAndEnums(t *testing.T) {
	obj := func(field string) func(string) string {
		return func(v string) string { return "{" + field + ": " + v + "}" }
	}
	nested := func(outer, inner string) func(string) string {
		return func(v string) string { return "{" + outer + ": {" + inner + ": " + v + "}}" }
	}
	nonNegInt := []string{"rps", "burst", "rpm", "rpd", "tpm", "itpm", "otpm", "tpd", "concurrency", "max_queue", "after_n"}
	var cases []rangeCase
	for _, f := range nonNegInt {
		cases = append(cases, rangeCase{f, obj(f), []string{"0", "1", "1000000"}, []string{"-1"}})
	}
	cases = append(cases,
		rangeCase{"queue_timeout", obj("queue_timeout"), []string{"0s", "1ms", "2m"}, []string{"-1s"}},
		rangeCase{"spend_monthly_usd", obj("spend_monthly_usd"), []string{"0", "0.5", "500"}, []string{"-0.01"}},
		rangeCase{"spend_soft_usd", obj("spend_soft_usd"), []string{"0", "10"}, []string{"-1"}},
		rangeCase{"credits_usd", obj("credits_usd"), []string{"0", "5.5"}, []string{"-0.5"}},
		rangeCase{"monthly_usd", nested("quota_usd", "monthly_usd"), []string{"0", "10"}, []string{"-1"}},
		rangeCase{"rolling_5h_pct", nested("quota_usd", "rolling_5h_pct"), []string{"0", "50", "100"}, []string{"-0.1", "100.1"}},
		rangeCase{"weekly_pct", nested("quota_usd", "weekly_pct"), []string{"0", "50", "100"}, []string{"-0.1", "100.1"}},
		rangeCase{"threshold_tpm", nested("ramp", "threshold_tpm"), []string{"0", "1000"}, []string{"-1"}},
		rangeCase{"max_increase_pct", nested("ramp", "max_increase_pct"), []string{"0", "50"}, []string{"-1"}},
		rangeCase{"window", nested("ramp", "window"), []string{"0s", "1m"}, []string{"-1m"}},
		rangeCase{"algorithm", obj("algorithm"), []string{"token_bucket", "fixed_window"}, []string{"sliding", "Token_Bucket"}},
		rangeCase{"tpm_reserve", obj("tpm_reserve"), []string{"max_tokens", "none"}, []string{"all", "0"}},
		rangeCase{"on_limit", obj("on_limit"), []string{"reject", "queue"}, []string{"drop"}},
		rangeCase{"mode", obj("mode"), []string{"always", "after_n", "off"}, []string{"sometimes", "Always"}},
	)
	for place, wrap := range limitsAt {
		for _, c := range cases {
			for _, v := range c.ok {
				if _, err := Parse([]byte(wrap(c.yaml(v)))); err != nil {
					t.Errorf("%s %s=%s: edge value refused: %v", place, c.field, v, err)
				}
			}
			for _, v := range c.bad {
				_, err := Parse([]byte(wrap(c.yaml(v))))
				if err == nil {
					t.Errorf("%s %s=%s: accepted", place, c.field, v)
					continue
				}
				leaf := c.field[strings.LastIndex(c.field, ".")+1:]
				if !strings.Contains(err.Error(), leaf) {
					t.Errorf("%s %s=%s: error does not cite the field: %v", place, c.field, v, err)
				}
			}
		}
	}
}

// TestSpec003LimitsWindow: `window` has durations >= 0 with duration <= every,
// and `mode: window` requires both > 0.
func TestSpec003LimitsWindow(t *testing.T) {
	for place, wrap := range limitsAt {
		for _, ok := range []string{
			"{window: {every: 0s, duration: 0s}}",
			"{window: {every: 1m, duration: 1m}}",
			"{window: {every: 1m, duration: 10s}}",
			"{mode: window, window: {every: 1m, duration: 1ns}}",
			"{mode: window, window: {every: 1m, duration: 1m}}",
		} {
			if _, err := Parse([]byte(wrap(ok))); err != nil {
				t.Errorf("%s %s: refused: %v", place, ok, err)
			}
		}
		for _, bad := range []struct{ limits, want string }{
			{"{window: {every: -1s, duration: 0s}}", "window"},
			{"{window: {every: 1m, duration: -1s}}", "window"},
			{"{window: {every: 1m, duration: 61s}}", "duration"},
			{"{mode: window}", "window"},
			{"{mode: window, window: {every: 1m}}", "window"},
			{"{mode: window, window: {duration: 1m}}", "window"},
			{"{mode: window, window: {every: 0s, duration: 0s}}", "window"},
		} {
			if _, err := Parse([]byte(wrap(bad.limits))); err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Errorf("%s %s: err %v", place, bad.limits, err)
			}
		}
	}
}

// TestSpec003StoreAndServerTables covers the server and store tables.
func TestSpec003StoreAndServerTables(t *testing.T) {
	ok := []string{
		"server: {auth: keys}\n", "server: {auth: none}\n", "server: {auth: ''}\n",
		"server: {trusted_proxies: ['10.0.0.0/8', '::1/128']}\n",
		"store: {kind: memory}\n",
		"store: {kind: valkey, addr: ['v:6379']}\n",
		"store: {on_down: allow}\n", "store: {on_down: deny}\n", "store: {on_down: local}\n",
		"store: {read_timeout: 0s}\n", "store: {read_timeout: 5ms}\n",
		"store: {event_max_len: 0}\n", "store: {event_max_len: 1000}\n",
		"store: {event_sample: 0}\n", "store: {event_sample: 0.5}\n", "store: {event_sample: 1}\n",
		"resilience: {max_reports: 0}\n", "resilience: {max_reports: 10000}\n",
		"drivers: [{name: openai, prefix: /openai}]\n", "drivers: [{name: openai}]\n",
		"keys: [{key: k, status: active}]\n", "keys: [{key: k, status: suspended}]\n",
		"keys: [{key: k, status: banned}]\n", "keys: [{key: k, status: expired}]\n",
		"tiers: {A.b_c-1: {}}\n", "tiers: {" + strings.Repeat("a", 64) + ": {}}\n",
	}
	for _, y := range ok {
		if _, err := Parse([]byte(y)); err != nil {
			t.Errorf("%q: edge value refused: %v", y, err)
		}
	}
	bad := []struct{ yaml, want string }{
		{"server: {auth: maybe}\n", "server.auth"},
		{"store: {kind: redis}\n", "store.kind"},
		{"store: {kind: valkey}\n", "store.addr"},
		{"store: {on_down: panic}\n", "on_down"},
		{"store: {read_timeout: -1ms}\n", "read_timeout"},
		{"store: {event_max_len: -1}\n", "event_max_len"},
		{"store: {event_sample: -0.1}\n", "event_sample"},
		{"store: {event_sample: 1.1}\n", "event_sample"},
		{"resilience: {max_reports: -1}\n", "max_reports"},
		{"resilience: {max_reports: 10001}\n", "max_reports"},
		{"drivers: [{prefix: /x}]\n", "name"},
		{"drivers: [{name: openai, prefix: openai}]\n", "prefix"},
		{"tiers: {'a b': {}}\n", "tier name"},
		{"tiers: {" + strings.Repeat("a", 65) + ": {}}\n", "tier name"},
		{"keys: [{key: k, tier: 'a/b'}]\n", "tier"},
	}
	for _, c := range bad {
		if _, err := Parse([]byte(c.yaml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err %v, want %q", c.yaml, err, c.want)
		}
	}
}
