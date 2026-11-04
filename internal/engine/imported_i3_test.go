package engine_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// I3: precedência explícita header > passo de cenário > chave > modelo >
// global (spec 017 I3, spec 008 "Por chave e por modelo", ADR 0026).
// The latency profiles use clock: logical so nothing sleeps; the recorder
// exposes the planned TTFT.

const i3Cfg = `
server: {seed: 7}
keys:
  - {key: k-plain, label: plain}
  - {key: k-slow, label: slow, latency: keyprof, errors: keyerr}
  - {key: k-lat, label: lat, latency: keyprof}
profiles:
  latency:
    default: {ttft: 5ms, itl: 0ms, clock: logical}
    lg: {ttft: 11ms, itl: 0ms, clock: logical}
    keyprof: {ttft: 33ms, itl: 0ms, clock: logical}
  errors:
    keyerr: {rate: 100, types: [bad_gateway]}
models:
  - {id: m-lat, latency: lg, errors: none}
  - {id: m-def}
`

const i3Scenario = `
name: i3-step
steps:
  - id: s
    content: "ok"
    latency: {ttft: 22ms}
`

func i3TTFT(t *testing.T, key, model string, extra map[string]string) int64 {
	t.Helper()
	e := newEngine(t, i3Cfg)
	impAddScenario(t, e, i3Scenario)
	h := map[string]string{"Authorization": "Bearer " + key}
	for k, v := range extra {
		h[k] = v
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, impBody(model, ""))
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	return impLastRecord(t, e).TTFTMS
}

// TestI3GlobalDefault: a model without its own profile uses the global default.
func TestI3GlobalDefault(t *testing.T) {
	if got := i3TTFT(t, "k-plain", "m-def", nil); got != 5 {
		t.Fatalf("ttft %d ms, want 5 (global default)", got)
	}
}

// TestI3KeyOverGlobal: keys[].latency overrides the global default.
func TestI3KeyOverGlobal(t *testing.T) {
	if got := i3TTFT(t, "k-lat", "m-def", nil); got != 33 {
		t.Fatalf("ttft %d ms, want 33 (key profile)", got)
	}
}

// TestI3KeyErrorsOverGlobal: keys[].errors overrides the global default.
func TestI3KeyErrorsOverGlobal(t *testing.T) {
	e := newEngine(t, i3Cfg)
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-slow"}, impBody("m-def", ""))
	if w.StatusCode() != 502 {
		t.Fatalf("status %d, want 502 (key errors profile)", w.StatusCode())
	}
}

// TestI3KeyOverModel: the key's profile beats the model's own (ADR 0026),
// for latency and errors (m-lat declares errors: none).
func TestI3KeyOverModel(t *testing.T) {
	if got := i3TTFT(t, "k-lat", "m-lat", nil); got != 33 {
		t.Fatalf("ttft %d ms, want 33 (key beats model)", got)
	}
	e := newEngine(t, i3Cfg)
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-slow"}, impBody("m-lat", ""))
	if w.StatusCode() != 502 {
		t.Fatalf("status %d, want 502 (key errors beat model errors: none)", w.StatusCode())
	}
}

// TestI3StepOverKey: the scenario step and the header beat the key's profile.
func TestI3StepOverKey(t *testing.T) {
	if got := i3TTFT(t, "k-lat", "m-lat", map[string]string{"x-wwz-scenario": "i3-step"}); got != 22 {
		t.Fatalf("ttft %d ms, want 22 (step beats key)", got)
	}
	if got := i3TTFT(t, "k-lat", "m-lat", map[string]string{"x-wwz-delay-ms": "4"}); got != 4 {
		t.Fatalf("ttft %d ms, want 4 (header beats key)", got)
	}
}

// TestI3KeyUnknownProfile: a key naming a missing profile is rejected when
// the configuration loads (task 099). A configuration that bypasses the
// validation still fails the request with a server error instead of
// silently using the model's profile.
func TestI3KeyUnknownProfile(t *testing.T) {
	for _, k := range []string{"{key: k-bad, latency: nope}", "{key: k-baderr, errors: nope}"} {
		yaml := strings.Replace(i3Cfg, "keys:\n", "keys:\n  - "+k+"\n", 1)
		if _, err := config.Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("%s: load err %v, want unknown profile", k, err)
		}
	}
	e := newEngine(t, i3Cfg)
	cfg, err := config.Parse([]byte(i3Cfg))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Keys = append(cfg.Keys, config.KeyConfig{Key: "k-bad", Label: "bad", Latency: "nope"}, config.KeyConfig{Key: "k-baderr", Label: "baderr", Errors: "nope"})
	e.SetConfig(cfg)
	for _, k := range []string{"k-bad", "k-baderr"} {
		w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + k}, impBody("m-def", ""))
		if w.StatusCode() != 500 {
			t.Fatalf("%s: status %d, want 500: %s", k, w.StatusCode(), w.Body())
		}
	}
}

// TestI3ModelOverGlobal: the model's profile beats the global default.
func TestI3ModelOverGlobal(t *testing.T) {
	if got := i3TTFT(t, "k-plain", "m-lat", nil); got != 11 {
		t.Fatalf("ttft %d ms, want 11 (model)", got)
	}
}

// TestI3StepOverModel: the scenario step's latency beats the model's.
func TestI3StepOverModel(t *testing.T) {
	if got := i3TTFT(t, "k-plain", "m-lat", map[string]string{"x-wwz-scenario": "i3-step"}); got != 22 {
		t.Fatalf("ttft %d ms, want 22 (step)", got)
	}
}

// TestI3HeaderOverStep: x-wwz-delay-ms beats the step.
func TestI3HeaderOverStep(t *testing.T) {
	if got := i3TTFT(t, "k-plain", "m-lat", map[string]string{"x-wwz-scenario": "i3-step", "x-wwz-delay-ms": "3"}); got != 3 {
		t.Fatalf("ttft %d ms, want 3 (header)", got)
	}
}

// TestI3SeedHeaderOverGlobal: x-wwz-seed beats server.seed and is echoed.
func TestI3SeedHeaderOverGlobal(t *testing.T) {
	e := newEngine(t, i3Cfg)
	h := map[string]string{"Authorization": "Bearer k-plain"}
	w := call(t, e, "openai", "/v1/chat/completions", h, impBody("m-lat", ""))
	if got := w.Headers()["x-wwz-seed"]; got != "7" {
		t.Fatalf("global seed: x-wwz-seed %q, want 7", got)
	}
	if rec := impLastRecord(t, e); rec.Seed != 7 {
		t.Fatalf("record seed %d", rec.Seed)
	}
	h["x-wwz-seed"] = "99"
	w = call(t, e, "openai", "/v1/chat/completions", h, impBody("m-lat", ""))
	if got := w.Headers()["x-wwz-seed"]; got != "99" {
		t.Fatalf("header seed: x-wwz-seed %q, want 99", got)
	}
	// Same seed header → same content.
	w2 := call(t, e, "openai", "/v1/chat/completions", h, impBody("m-lat", ""))
	if string(decodeContent(t, w.Body())) != string(decodeContent(t, w2.Body())) {
		t.Fatal("x-wwz-seed must make the content reproducible")
	}
}

// TestI3ErrorHeaderOverStep: x-wwz-return-error beats a scenario step error.
func TestI3ErrorHeaderOverStep(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	h := map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-scenario": stepErrScenario, "x-wwz-return-error": "server_error"}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 500 {
		t.Fatalf("header error should beat the step (429): got %d %s", w.StatusCode(), w.Body())
	}
}

func decodeContent(t *testing.T, b []byte) string {
	t.Helper()
	w := decode(t, b)
	if len(w.Choices) == 0 {
		t.Fatalf("no choices: %s", b)
	}
	s, _ := w.Choices[0].Message["content"].(string)
	return s
}
