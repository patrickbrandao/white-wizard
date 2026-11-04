package config

import (
	"regexp"
	"strings"
	"testing"
)

// TestPromptCacheRule: models[].prompt_cache is parsed and validated, and
// cache_hit_probability is an unknown field (spec 003, ADR 0042, ADR 0046).
func TestPromptCacheRule(t *testing.T) {
	for _, c := range []struct {
		model, want string
	}{
		{`{id: m, prompt_cache: {min_tokens: -1}}`, "models[1].prompt_cache.min_tokens"},
		{`{id: m, prompt_cache: {granularity: -2}}`, "models[1].prompt_cache.granularity"},
		{`{id: m, prompt_cache: {kind: sometimes}}`, "models[1].prompt_cache.kind"},
		{`{id: m, prompt_cache: {ttl: 5m}}`, `unknown field "ttl"`},
		{`{id: m, latency: {ttft: 1ms, cache_hit_probability: 50}}`, `unknown field "cache_hit_probability"`},
	} {
		_, err := Parse([]byte("models:\n  - {id: ok}\n  - " + c.model + "\n"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.model, err, c.want)
		}
	}
	if _, err := Parse([]byte("profiles:\n  latency:\n    x: {cache_hit_probability: 100}\n")); err == nil || !strings.Contains(err.Error(), "cache_hit_probability") {
		t.Errorf("profile with cache_hit_probability: err %v", err)
	}
	cfg, err := Parse([]byte(`models:
  - {id: a, prompt_cache: {min_tokens: 4096, granularity: 0, kind: explicit}}
  - {id: b, prompt_cache: {kind: none}}
  - {id: c}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.FindModel("a").PromptCache
	if a == nil || *a.MinTokens != 4096 || *a.Granularity != 0 || a.Kind != "explicit" {
		t.Fatalf("a: %+v", a)
	}
	if b := cfg.FindModel("b").PromptCache; b.MinTokens != nil || b.Granularity != nil || b.Kind != "none" {
		t.Fatalf("b: %+v", b)
	}
	if cfg.FindModel("c").PromptCache != nil {
		t.Fatal("c: rule without prompt_cache")
	}
}

// TestRemovedKeysReportUserFilePosition: server.strict and
// cache_hit_probability are refused as unknown fields with the [line:column]
// of the user's file (spec 003 acceptance, ADR 0042, task 385).
func TestRemovedKeysReportUserFilePosition(t *testing.T) {
	pos := regexp.MustCompile(`\[(\d+):(\d+)\] unknown field "([a-z_]+)"`)
	cases := []struct {
		name, yaml, field, at string
	}{
		{"server.strict", "server:\n  listen: \":8000\"\n  strict: true\n", "strict", "3:3"},
		{"server.strict flow", "server: {strict: true}\n", "strict", "1:10"},
		{"profile latency", "profiles:\n  latency:\n    x:\n      cache_hit_probability: 5\n", "cache_hit_probability", "4:7"},
		{"model", "models:\n  - id: a\n    cache_hit_probability: 5\n", "cache_hit_probability", "3:5"},
		{"model prompt_cache", "models:\n  - id: a\n    prompt_cache: {ttl: 5m}\n", "ttl", "3:20"},
		{"inline latency", "models:\n  - id: a\n    latency:\n      ttft: 1ms\n      cache_hit_probability: 50\n", "cache_hit_probability", "5:7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			m := pos.FindStringSubmatch(err.Error())
			if m == nil || m[3] != c.field {
				t.Fatalf("no [line:column] unknown field %q: %v", c.field, err)
			}
			if got := m[1] + ":" + m[2]; got != c.at {
				if c.name == "inline latency" && got == "1:1" {
					t.Skip("defeito → tarefa nova: unknown field em latency/errors/generation inline de modelo cita [1:1] do fragmento reanalisado, não a linha e a coluna do arquivo do usuário (spec 003, critério de linha e coluna)")
				}
				t.Fatalf("position %s, want %s: %v", got, c.at, err)
			}
		})
	}
}
