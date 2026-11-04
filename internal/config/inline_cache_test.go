package config

import (
	"reflect"
	"testing"
)

const inlineCfg = `
models:
  - id: inline
    latency: {ttft: 10ms, itl: 2ms}
    errors: {rate: 5}
    generation: {mode: lorem}
  - id: named
`

// TestInlineProfilesCached: an inline profile is converted from YAML once
// per Config and then served from the cache, equal to the conversion
// (task 270).
func TestInlineProfilesCached(t *testing.T) {
	c, err := Parse([]byte(inlineCfg))
	if err != nil {
		t.Fatal(err)
	}
	m := &c.Models[0]
	lat1, err := c.LatencyFor(m)
	if err != nil {
		t.Fatal(err)
	}
	want, err := resolveProfile(m.Latency, c.Profiles.Latency)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lat1, want) {
		t.Fatalf("cached latency %+v, want %+v", lat1, want)
	}
	gen, err := c.GenerationFor(m)
	if err != nil {
		t.Fatal(err)
	}
	if gen.Mode != "lorem" || gen.OutputTokens == nil || gen.ToolCalls == nil {
		t.Fatalf("generation without defaults: %+v", gen)
	}
	if _, err := c.ErrorsFor(m); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, _ = c.LatencyFor(m)
		_, _ = c.ErrorsFor(m)
		_, _ = c.GenerationFor(m)
	}); n != 0 {
		t.Fatalf("cached inline profiles allocate %v times", n)
	}
	gen2, _ := c.GenerationFor(m)
	if gen2.OutputTokens != gen.OutputTokens {
		t.Fatal("the defaults should come from the cache, not be rebuilt")
	}
	// A named profile does not go through the cache.
	if _, err := c.LatencyFor(&c.Models[1]); err != nil {
		t.Fatal(err)
	}
	var n int
	c.inline.m.Range(func(any, any) bool { n++; return true })
	if n != 3 {
		t.Fatalf("cache holds %d profiles, want 3", n)
	}
}

// TestInlineProfilesUncached: a Config not built by Parse has no cache and
// converts every time; errors are not cached.
func TestInlineProfilesUncached(t *testing.T) {
	c := &Config{Profiles: Profiles{Latency: map[string]LatencyProfile{}}}
	m := &ModelConfig{Latency: map[string]any{"ttft": "5ms"}}
	if _, err := c.LatencyFor(m); err != nil {
		t.Fatal(err)
	}
	c.inline = &inlineProfiles{}
	bad := &ModelConfig{Latency: map[string]any{"nope": 1}}
	if _, err := c.LatencyFor(bad); err == nil {
		t.Fatal("unknown field should fail")
	}
	if _, err := c.LatencyFor(bad); err == nil {
		t.Fatal("the error must not be cached as a profile")
	}
	var nilMap map[string]any
	if _, err := c.LatencyFor(&ModelConfig{Latency: nilMap}); err != nil {
		t.Fatal(err)
	}
	c.inline.m.Range(func(k, _ any) bool { t.Fatalf("cached %v", k); return false })
}
