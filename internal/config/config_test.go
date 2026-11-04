package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8000" || c.Store.Kind != "memory" || len(c.Models) != 3 || len(c.Keys) != 10 {
		t.Fatalf("unexpected defaults: %+v", c.Server)
	}
	if len(c.Drivers) != 4 {
		t.Fatalf("expected 4 default drivers, got %d", len(c.Drivers))
	}
}

// TestUsageExtended: usage.extended (I16) defaults to false, parses true and
// the section is strict YAML.
func TestUsageExtended(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Usage.Extended {
		t.Fatal("usage.extended must default to false")
	}
	c, err = Parse([]byte("usage:\n  extended: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Usage.Extended {
		t.Fatal("usage.extended: true not parsed")
	}
	if _, err := Parse([]byte("usage:\n  input_breakdown: true\n")); err == nil {
		t.Fatal("expected error for unknown usage key")
	}
}

func TestResilienceDefaultsAndValidation(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Resilience.Tracking() || c.Resilience.MaxReports != 100 {
		t.Fatalf("resilience defaults: %+v", c.Resilience)
	}
	c, err = Parse([]byte("resilience:\n  track: false\n  max_reports: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Resilience.Tracking() || c.Resilience.MaxReports != 5 {
		t.Fatalf("resilience: %+v", c.Resilience)
	}
	if (ResilienceConfig{}).Tracking() != true {
		t.Fatal("nil track must mean true")
	}
	if _, err := Parse([]byte("resilience:\n  max_reports: -1\n")); err == nil {
		t.Fatal("expected error for negative max_reports")
	}
	if _, err := Parse([]byte("resilience:\n  bogus: 1\n")); err == nil {
		t.Fatal("expected error for unknown resilience key")
	}
}

func TestParseDistScalarAndMap(t *testing.T) {
	doc := `
models:
  - id: m1
    latency:
      ttft: 300ms
      itl: { dist: normal, mean: 20ms, stddev: 4ms }
      queue: { dist: lognormal, median: 10ms, p99: 200ms }
    errors: flaky
`
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	lp, err := c.LatencyFor(&c.Models[0])
	if err != nil {
		t.Fatal(err)
	}
	if lp.TTFT == nil || lp.TTFT.Kind != "fixed" || lp.TTFT.Value != 300*time.Millisecond {
		t.Fatalf("ttft: %+v", lp.TTFT)
	}
	if lp.ITL == nil || lp.ITL.Kind != "normal" || lp.ITL.Mean != 20*time.Millisecond {
		t.Fatalf("itl: %+v", lp.ITL)
	}
	if lp.Queue == nil || lp.Queue.Kind != "lognormal" || lp.Queue.P99 != 200*time.Millisecond {
		t.Fatalf("queue: %+v", lp.Queue)
	}
	ep, err := c.ErrorsFor(&c.Models[0])
	if err != nil || ep.Rate != 5 {
		t.Fatalf("errors: %+v %v", ep, err)
	}
}

func TestValidateUnknownProfile(t *testing.T) {
	_, err := Parse([]byte("models:\n  - id: m\n    latency: nope\n"))
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

// TestValidateKeyProfiles: keys[].latency and keys[].errors must name
// existing profiles at load time (else every request with the key is a 500).
func TestValidateKeyProfiles(t *testing.T) {
	base := "models:\n  - id: m\nprofiles:\n  latency:\n    fast: {}\n  errors:\n    flaky: {rate: 1}\nkeys:\n  - key: k1\n"
	for _, ok := range []string{"", "    latency: fast\n", "    errors: flaky\n", "    latency: fast\n    errors: flaky\n"} {
		if _, err := Parse([]byte(base + ok)); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	for _, bad := range []struct{ yaml, want string }{
		{"    latency: nope\n", `keys[0] references unknown latency profile "nope"`},
		{"    errors: nope\n", `keys[0] references unknown errors profile "nope"`},
	} {
		_, err := Parse([]byte(base + bad.yaml))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("%q: err %v, want %q", bad.yaml, err, bad.want)
		}
	}
}

// TestToolMisbehaviorValidation: kinds (with spec aliases), drivers and
// rate are checked at load; inline profiles are decoded strictly.
func TestToolMisbehaviorValidation(t *testing.T) {
	model := func(errs string) []byte {
		return []byte("models:\n  - id: m\n    errors: " + errs + "\n")
	}
	full := "{tool_misbehavior: {rate: 50, kinds: [truncated, trailing_comma, single_quotes, unknown_tool, length_mid_tool], tool: read_file, drivers: [openai, anthropic, opencode, openrouter]}}"
	c, err := Parse(model(full))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.ErrorsFor(&c.Models[0])
	if p.ToolMisbehavior.Tool != "read_file" || len(p.ToolMisbehavior.Drivers) != 4 {
		t.Fatalf("inline tool_misbehavior lost fields: %+v", p.ToolMisbehavior)
	}
	for _, e := range []string{"{rate: 1, tool_misbehavior: {rate: 0}}", "flaky"} {
		if _, err := Parse(model(e)); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	for _, e := range []string{
		"{tool_misbehavior: {rate: 100, kinds: [bogus]}}",
		"{tool_misbehavior: {rate: 100, drivers: [gemini]}}",
		"{tool_misbehavior: {rate: 101}}",
		"{tool_misbehavior: {rate: 100, driver: [openai]}}",
		"{rate: 1, typo_field: 2}",
	} {
		if _, err := Parse(model(e)); err == nil {
			t.Errorf("%s: expected error", e)
		}
	}
	for _, y := range []string{
		"models:\n  - id: m\n    latency: {ttft: 1ms, bogus: 1}\n",
		"models:\n  - id: m\n    generation: {bogus: 1}\n",
		"profiles:\n  errors:\n    x: {tool_misbehavior: {kinds: [nope]}}\nmodels:\n  - id: m\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%q: expected error", y)
		}
	}
	if k, ok := CanonicalToolMisbehavior("single_quotes"); !ok || k != "invalid_json_single_quotes" {
		t.Fatalf("alias: %q %v", k, ok)
	}
	if _, ok := CanonicalToolMisbehavior("nope"); ok {
		t.Fatal("unknown kind accepted")
	}
}

func TestValidTierName(t *testing.T) {
	for _, n := range []string{"a", "free", "tier-4", "Pro_2.v1", strings.Repeat("z", 64), "0"} {
		if !ValidTierName(n) {
			t.Errorf("%q: want valid", n)
		}
	}
	for _, n := range []string{"", "a b", "a:b", "pró", "a/b", strings.Repeat("z", 65), "a\n"} {
		if ValidTierName(n) {
			t.Errorf("%q: want invalid", n)
		}
	}
}

func TestValidateTierNames(t *testing.T) {
	good := "tiers:\n  tier-4.v_1: { limits: { rpm: 5 } }\nkeys:\n  - key: k\n    tier: tier-4.v_1\n  - key: k2\nmodels:\n  - id: m\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]struct{ doc, want string }{
		"tier space": {"tiers:\n  \"a b\": { limits: { rpm: 5 } }\nmodels:\n  - id: m\n", "config: tiers.a b: invalid tier name"},
		"tier colon": {"tiers:\n  \"a:b\": {}\nmodels:\n  - id: m\n", "config: tiers.a:b: invalid tier name"},
		"tier long":  {"tiers:\n  " + strings.Repeat("x", 65) + ": {}\nmodels:\n  - id: m\n", "invalid tier name"},
		"key tier":   {"keys:\n  - key: k\n    tier: \"a b\"\nmodels:\n  - id: m\n", `config: keys[0].tier: invalid tier name "a b"`},
		"key colon":  {"keys:\n  - key: k\n  - key: k2\n    tier: \"x:y\"\nmodels:\n  - id: m\n", "config: keys[1].tier: invalid tier name"},
	}
	for name, c := range bad {
		_, err := Parse([]byte(c.doc))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// An empty key tier (never defaulted, as in a hand-built Config) is not
	// checked here: the default tier applies.
	c := &Config{Models: []ModelConfig{{ID: "m"}}, Keys: []KeyConfig{{Key: "k"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateStoreEnumsAndRanges(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Store.OnDown != "local" || c.Store.EventSample != 1 {
		t.Fatalf("store defaults: %+v", c.Store)
	}
	for _, ok := range []string{"allow", "deny", "local"} {
		if _, err := Parse([]byte("store:\n  on_down: " + ok + "\n")); err != nil {
			t.Fatalf("on_down %s: %v", ok, err)
		}
	}
	if _, err := Parse([]byte("store:\n  event_sample: 0.25\n")); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"on_down maybe":     "store:\n  on_down: maybe\n",
		"on_down uppercase": "store:\n  on_down: ALLOW\n",
		"event_sample 2":    "store:\n  event_sample: 2\n",
		"event_sample -0.1": "store:\n  event_sample: -0.1\n",
		"event_sample nan":  "store:\n  event_sample: .nan\n",
		"event_max_len -1":  "store:\n  event_max_len: -1\n",
		"read_timeout -1ms": "store:\n  read_timeout: -1ms\n",
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Validate on a struct without defaults accepts the zero values.
	if err := (StoreConfig{}).validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLimits(t *testing.T) {
	good := `
tiers:
  t1:
    limits:
      rps: 10
      burst: 20
      rpm: 600
      tpm: 1000
      concurrency: 4
      max_queue: 8
      queue_timeout: 2s
      spend_monthly_usd: 10
      credits_usd: 0
      quota_usd: { monthly_usd: 60, rolling_5h_pct: 20, weekly_pct: 100 }
      algorithm: fixed_window
      tpm_reserve: none
      on_limit: queue
      mode: after_n
      after_n: 3
      ramp: { threshold_tpm: 100, max_increase_pct: 50, window: 1m }
keys:
  - key: k1
    tier: t1
    limits: { rpm: 5, algorithm: token_bucket, tpm_reserve: max_tokens, on_limit: reject, mode: always }
  - key: k2
    limits: { rpm: 1, mode: window, window: { every: 1m, duration: 10s } }
  - key: k3
    limits: { mode: "off" }
models:
  - id: m
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	fields := []string{
		"rps: -1", "burst: -1", "rpm: -5", "rpd: -1", "tpm: -1", "itpm: -1", "otpm: -1",
		"tpd: -1", "concurrency: -1", "max_queue: -1", "after_n: -1", "queue_timeout: -1s",
		"spend_monthly_usd: -1", "spend_soft_usd: -0.5", "credits_usd: -1",
		"quota_usd: { monthly_usd: -1 }", "quota_usd: { rolling_5h_pct: 101 }",
		"quota_usd: { weekly_pct: -1 }", "ramp: { threshold_tpm: -1 }",
		"ramp: { max_increase_pct: -1 }", "ramp: { window: -1s }",
		"algorithm: leaky", "tpm_reserve: all", "on_limit: drop", "mode: sometimes",
		"mode: window", "mode: window, window: { every: 1m }", "mode: window, window: { duration: 1s }",
		"window: { every: -1s }", "window: { every: 1s, duration: -1s }", "window: { every: 1s, duration: 2s }",
	}
	for _, f := range fields {
		key := "keys:\n  - key: k\n    limits: { " + f + " }\nmodels:\n  - id: m\n"
		if _, err := Parse([]byte(key)); err == nil {
			t.Errorf("keys %s: expected error", f)
		}
		tier := "tiers:\n  t:\n    limits: { " + f + " }\nmodels:\n  - id: m\n"
		if _, err := Parse([]byte(tier)); err == nil {
			t.Errorf("tiers %s: expected error", f)
		}
	}
}

func TestWindowActive(t *testing.T) {
	base := time.Unix(1_700_000_040, 0) // multiple of 60 s
	w := Window{Every: time.Minute, Duration: 10 * time.Second}
	cases := []struct {
		at   time.Duration
		want bool
	}{{0, true}, {9 * time.Second, true}, {10 * time.Second, false}, {59 * time.Second, false}, {time.Minute, true}}
	for _, c := range cases {
		if got := w.Active(base.Add(c.at)); got != c.want {
			t.Errorf("Active(+%s) = %v, want %v", c.at, got, c.want)
		}
	}
	if !w.Active(time.Unix(-55, 0)) || w.Active(time.Unix(-50, 0)) {
		t.Error("phase before the epoch must wrap to [0, every)")
	}
	if (Window{}).Active(base.Add(30*time.Second)) != true {
		t.Error("zero window must always apply")
	}
	if (Window{Every: time.Minute}).Active(base) {
		t.Error("zero duration must never apply")
	}
	if !(Window{Every: time.Minute, Duration: time.Minute}).Active(base.Add(59 * time.Second)) {
		t.Error("duration == every must always apply")
	}
}
