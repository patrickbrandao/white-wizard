package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestGenerationModeValidated: generation.mode outside the list, and mode
// dataset without a readable, well-formed dataset_path, make the
// configuration invalid, citing the field (task 381, ADR 0068).
func TestGenerationModeValidated(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	bad := filepath.Join(dir, "bad.jsonl")
	writeDataset(t, good, `{"prompt":"q","response":"a"}`+"\n")
	writeDataset(t, bad, "{not json}\n")
	cases := []struct{ yaml, want string }{
		{"profiles: {generation: {p: {mode: datset}}}\nmodels: [{id: m, generation: p}]", `config: profiles.generation.p.mode: unknown mode "datset"`},
		{"models: [{id: m, generation: {mode: datset}}]", `config: models[m].generation.mode: unknown mode "datset"`},
		{"models: [{id: m, generation: {mode: dataset}}]", "config: models[m].generation.dataset_path: required with mode dataset"},
		{"profiles: {generation: {p: {mode: dataset}}}\nmodels: [{id: m, generation: p}]", "config: profiles.generation.p.dataset_path: required with mode dataset"},
		{"models: [{id: m, generation: {mode: dataset, dataset_path: " + filepath.Join(dir, "none.jsonl") + "}}]", "config: models[m].generation.dataset_path: open "},
		{"models: [{id: m, generation: {mode: dataset, dataset_path: " + bad + "}}]", "config: models[m].generation.dataset_path: " + bad + ": line 1: "},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: %v (want prefix %q)", tc.yaml, err, tc.want)
		}
	}
	for _, mode := range GenerationModes {
		y := "models: [{id: m, generation: {mode: " + mode + ", dataset_path: " + good + "}}]"
		if _, err := Parse([]byte(y)); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	// dataset_path with another mode is accepted and ignored.
	if _, err := Parse([]byte("models: [{id: m, generation: {mode: lorem, dataset_path: /nope}}]")); err != nil {
		t.Fatal(err)
	}
	// An inline profile that does not decode (reported first by
	// validateInline) is reported here too.
	c := &Config{Models: []ModelConfig{{ID: "m", Generation: map[string]any{"nope": 1}}}}
	if err := c.validateGenerations(); err == nil || !strings.Contains(err.Error(), `model "m" inline generation`) {
		t.Fatal(err)
	}
}

// TestReadDataset: the JSONL format of generation.dataset_path.
func TestReadDataset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.jsonl")
	writeDataset(t, p, "\n"+`{"prompt":"one","response":"r1"}`+"\n  \n"+`{"prompt":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}],"response":"r2"}`+"\n")
	got, err := ReadDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []DatasetEntry{
		{Prompt: []DatasetMessage{{Role: "user", Content: "one"}}, Response: "r1"},
		{Prompt: []DatasetMessage{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}, Response: "r2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	for _, tc := range []struct{ body, want string }{
		{"", "no entries"},
		{"\n\n", "no entries"},
		{`[1]`, "line 1: json"},
		{`{"prompt":"q"}`, `line 1: "response" must be a non-empty string`},
		{`{"prompt":"q","response":""}`, `"response" must be a non-empty string`},
		{`{"prompt":"q","response":1}`, "line 1: json"},
		{`{"response":"r"}`, `"prompt" must be a string`},
		{`{"prompt":null,"response":"r"}`, `"prompt" must be a string`},
		{`{"prompt":"","response":"r"}`, `"prompt" must be a string`},
		{`{"prompt":[],"response":"r"}`, `"prompt" must be a string`},
		{`{"prompt":{"a":1},"response":"r"}`, `"prompt" must be a string`},
		{`{"prompt":"q","response":"r"}` + "\n" + `{"prompt":3,"response":"r"}`, `line 2: "prompt" must be a string`},
		{strings.Repeat("x", maxDatasetLine+1), "token too long"},
	} {
		writeDataset(t, p, tc.body)
		if _, err := ReadDataset(p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%.40q: %v (want %q)", tc.body, err, tc.want)
		}
	}
	if _, err := ReadDataset(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func writeDataset(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGenerationForDefaults: an inline profile gets the same defaults as a
// named one, a partial tool_calls block is completed on a copy (the shared
// profile is never written) and a named profile resolves without
// allocating (task 268).
func TestGenerationForDefaults(t *testing.T) {
	shared := &ToolCallsGen{Probability: 10}
	c := &Config{Profiles: Profiles{Generation: map[string]GenerationProfile{"p": {ToolCalls: shared}}}}
	ApplyDefaults(c)
	if *shared != (ToolCallsGen{Probability: 10}) {
		t.Fatalf("ApplyDefaults wrote the shared tool_calls: %+v", *shared)
	}
	g, err := c.GenerationFor(&ModelConfig{Generation: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if tc := g.ToolCalls; tc.Probability != 10 || tc.ExtraCallProbability != 0 || tc.MaxInt != 100 || tc.MaxArrayLen != 5 || g.Mode != "random" || g.ToolArgsChunkBytes != 24 {
		t.Fatalf("named profile: %+v %+v", g, *g.ToolCalls)
	}

	inline, err := c.GenerationFor(&ModelConfig{Generation: map[string]any{"mode": "lorem"}})
	if err != nil {
		t.Fatal(err)
	}
	if inline.Mode != "lorem" || inline.OutputTokens == nil || inline.ReasoningTokens == nil || inline.ToolCalls.Probability != 35 || inline.ToolCalls.MinArrayLen != 1 || inline.ToolCalls.MaxNumber != 100 {
		t.Fatalf("inline profile: %+v %+v", inline, *inline.ToolCalls)
	}

	m := &ModelConfig{Generation: "p"}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = c.GenerationFor(m) }); allocs != 0 {
		t.Fatalf("GenerationFor of a named profile allocates: %v", allocs)
	}
}

// BenchmarkGenerationFor: the per-request lookup of a named profile (task 268).
func BenchmarkGenerationFor(b *testing.B) {
	c := &Config{}
	ApplyDefaults(c)
	m := &c.Models[0]
	b.ReportAllocs()
	for range b.N {
		if _, err := c.GenerationFor(m); err != nil {
			b.Fatal(err)
		}
	}
}

// TestToolCallsExplicitZero: a tool_calls probability written as 0 stays 0,
// and only the probabilities left out get the spec 003 defaults (35, 45,
// 50), in named and inline profiles and after a YAML round trip (task 463).
func TestToolCallsExplicitZero(t *testing.T) {
	cases := []struct {
		block               string
		prob, extra, option float64
	}{
		{"", 35, 45, 50},
		{"tool_calls: null", 35, 45, 50},
		{"tool_calls: {}", 35, 45, 50},
		{"tool_calls: {probability: 0}", 0, 45, 50},
		{"tool_calls: {extra_call_probability: 0}", 35, 0, 50},
		{"tool_calls: {optional_param_probability: 0, max_int: 7}", 35, 45, 0},
		{"tool_calls: {probability: 0, extra_call_probability: 0, optional_param_probability: 0}", 0, 0, 0},
		{"tool_calls: {probability: 10, extra_call_probability: 20, optional_param_probability: 30}", 10, 20, 30},
	}
	for _, tc := range cases {
		named := "profiles: {generation: {p: {mode: random, " + tc.block + "}}}\nmodels: [{id: n, generation: p}, {id: i, generation: {mode: random, " + tc.block + "}}]"
		c, err := Parse([]byte(named))
		if err != nil {
			t.Fatalf("%q: %v", tc.block, err)
		}
		for i := range c.Models {
			id := c.Models[i].ID
			g, err := c.GenerationFor(&c.Models[i])
			if err != nil {
				t.Fatalf("%q %s: %v", tc.block, id, err)
			}
			got := g.ToolCalls
			if got.Probability != tc.prob || got.ExtraCallProbability != tc.extra || got.OptionalParamProbability != tc.option {
				t.Fatalf("%q %s: probabilities %v/%v/%v, want %v/%v/%v", tc.block, id,
					got.Probability, got.ExtraCallProbability, got.OptionalParamProbability, tc.prob, tc.extra, tc.option)
			}
			if got.MaxInt == 0 || got.MaxArrayLen != 5 {
				t.Fatalf("%q %s: ranges not defaulted: %+v", tc.block, id, *got)
			}
			b, err := yaml.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			var back GenerationProfile
			if err := yaml.UnmarshalWithOptions(b, &back, yaml.Strict()); err != nil {
				t.Fatal(err)
			}
			if *back.ToolCalls != *got {
				t.Fatalf("%q %s: round trip %+v, want %+v", tc.block, id, *back.ToolCalls, *got)
			}
		}
	}

	if _, err := Parse([]byte("profiles: {generation: {p: {tool_calls: {probabilty: 0}}}}")); err == nil || !strings.Contains(err.Error(), "probabilty") {
		t.Fatalf("unknown tool_calls key: %v", err)
	}
	if _, err := Parse([]byte("profiles: {generation: {p: {tool_calls: {probability: x}}}}")); err == nil {
		t.Fatal("tool_calls.probability: x was accepted")
	}
}
