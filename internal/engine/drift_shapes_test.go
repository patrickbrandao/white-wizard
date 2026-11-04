package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// Task 171 (T-ENG of the 2026-10-31 drift triage, ADR 0049): request id
// header per driver, length at max_tokens, chat template overhead in
// prompt_tokens, the dated snapshot in model, retires_at and the order of
// the key and body checks.

const shapesCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    long: {output_tokens: {dist: fixed, value: 400}}
    short: {output_tokens: {dist: fixed, value: 5}}
models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, aliases: [mini-alias], latency: fast, generation: long, retires_at: "2027-01-14", deprecated_at: "2026-07-14"}
  - {id: claude-haiku-4-5-20251001, aliases: [claude-haiku-4-5], latency: fast, generation: long}
  - {id: short-model, latency: fast, generation: short}
  - {id: old-model, latency: fast, retires_at: "2026-10-23"}
keys:
  - {key: k-shapes, label: shapes}
`

var (
	shapesOAI  = map[string]string{"Authorization": "Bearer k-shapes"}
	shapesAnth = map[string]string{"x-api-key": "k-shapes", "anthropic-version": "2023-06-01"}
)

func shapesEngine(t *testing.T) func(drv, path string, h map[string]string, body string) (int, map[string]string, map[string]any) {
	t.Helper()
	e := newEngine(t, shapesCfg)
	e.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	return func(drv, path string, h map[string]string, body string) (int, map[string]string, map[string]any) {
		t.Helper()
		w := call(t, e, drv, path, h, body)
		var out map[string]any
		_ = json.Unmarshal(w.Body(), &out) // nil for a stream
		return w.StatusCode(), w.Headers(), out
	}
}

func finishReason(out map[string]any) string {
	if s, ok := out["stop_reason"].(string); ok {
		return s
	}
	ch, _ := out["choices"].([]any)
	if len(ch) == 0 {
		return ""
	}
	s, _ := ch[0].(map[string]any)["finish_reason"].(string)
	return s
}

func usageInt(out map[string]any, name string) int {
	u, _ := out["usage"].(map[string]any)
	f, _ := u[name].(float64)
	return int(f)
}

// TestRequestIDHeaderPerDriver: OpenAI sends x-request-id, Anthropic only
// request-id, OpenRouter and OpenCode neither, on success and on error.
func TestRequestIDHeaderPerDriver(t *testing.T) {
	do := shapesEngine(t)
	cases := []struct {
		drv, path string
		h         map[string]string
		model     string
		want      string // the request id header, "" for none
	}{
		{"openai", "/v1/chat/completions", shapesOAI, "gpt-4o-mini", "x-request-id"},
		{"anthropic", "/v1/messages", shapesAnth, "claude-haiku-4-5", "request-id"},
		{"openrouter", "/api/v1/chat/completions", shapesOAI, "gpt-4o-mini", ""},
		{"opencode", "/zen/go/v1/chat/completions", shapesOAI, "gpt-4o-mini", ""},
	}
	for _, c := range cases {
		for _, model := range []string{c.model, "no-such-model"} {
			body := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
			status, h, _ := do(c.drv, c.path, c.h, body)
			if (model == c.model) != (status == 200) {
				t.Fatalf("%s %s: status %d", c.drv, model, status)
			}
			for _, name := range []string{"x-request-id", "request-id"} {
				_, has := h[name]
				if has != (name == c.want) {
					t.Errorf("%s %s (status %d): header %s present=%v, want %v (headers %v)", c.drv, model, status, name, has, name == c.want, h)
				}
			}
		}
	}
}

// TestFinishLengthAtMaxTokens: output that reaches max_tokens ends with
// length / max_tokens; below the ceiling it ends with stop / end_turn.
func TestFinishLengthAtMaxTokens(t *testing.T) {
	do := shapesEngine(t)
	for _, stream := range []bool{false, true} {
		sfx := ""
		if stream {
			sfx = `,"stream":true,"stream_options":{"include_usage":true}`
		}
		// OpenAI: the profile asks for 400 tokens, max_tokens is 16.
		status, _, out := do("openai", "/v1/chat/completions", shapesOAI, `{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`+sfx+`}`)
		if status != 200 {
			t.Fatalf("status %d", status)
		}
		if !stream {
			if got := finishReason(out); got != "length" {
				t.Errorf("openai finish_reason %q, want length", got)
			}
			if got := usageInt(out, "completion_tokens"); got != 16 {
				t.Errorf("completion_tokens %d, want 16", got)
			}
		}
		// Below the ceiling: stop.
		_, _, out = do("openai", "/v1/chat/completions", shapesOAI, `{"model":"short-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if got := finishReason(out); got != "stop" {
			t.Errorf("short output finish_reason %q, want stop", got)
		}
	}
	_, _, out := do("anthropic", "/v1/messages", shapesAnth, `{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if got := finishReason(out); got != "max_tokens" {
		t.Errorf("anthropic stop_reason %q, want max_tokens", got)
	}
	if got := usageInt(out, "output_tokens"); got != 16 {
		t.Errorf("output_tokens %d, want 16", got)
	}
}

// TestFinishLengthStream: the stream ends with the same finish reason.
func TestFinishLengthStream(t *testing.T) {
	e := newEngine(t, shapesCfg)
	w := call(t, e, "openai", "/v1/chat/completions", shapesOAI, `{"model":"gpt-4o-mini","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(string(w.Body()), `"finish_reason":"length"`) {
		t.Errorf("stream without finish_reason length: %s", w.Body())
	}
	w = call(t, e, "anthropic", "/v1/messages", shapesAnth, `{"model":"claude-haiku-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(string(w.Body()), `"stop_reason":"max_tokens"`) {
		t.Errorf("stream without stop_reason max_tokens: %s", w.Body())
	}
}

// TestPromptTemplateOverhead: prompt_tokens = 3 + role + content per message
// plus 3 per request, on every driver. gpt-4o-mini counts with o200k_base
// (tokenizer auto, ADR 0053): the drift probe ("Responda só: ok", 6 tokens)
// gives the real 13. claude-haiku has no public tokenizer and stays on the
// simulated one (5 tokens, 12).
func TestPromptTemplateOverhead(t *testing.T) {
	do := shapesEngine(t)
	if n := tokenize.O200k.Count("Responda só: ok"); n != 6 {
		t.Fatalf("o200k probe = %d, want 6", n)
	}
	if n := (generate.Simulated{}).Count("Responda só: ok"); n != 5 {
		t.Fatalf("simulated probe = %d, want 5", n)
	}
	probe := `"messages":[{"role":"user","content":"Responda só: ok"}]`
	for _, c := range []struct {
		drv, path, field string
		h                map[string]string
		model            string
		want             int
	}{
		{"openai", "/v1/chat/completions", "prompt_tokens", shapesOAI, "gpt-4o-mini", 13},
		{"openrouter", "/api/v1/chat/completions", "prompt_tokens", shapesOAI, "gpt-4o-mini", 13},
		{"opencode", "/zen/go/v1/chat/completions", "prompt_tokens", shapesOAI, "gpt-4o-mini", 13},
		{"anthropic", "/v1/messages", "input_tokens", shapesAnth, "claude-haiku-4-5", 12},
	} {
		status, _, out := do(c.drv, c.path, c.h, `{"model":"`+c.model+`","max_tokens":16,`+probe+`}`)
		if status != 200 {
			t.Fatalf("%s: status %d", c.drv, status)
		}
		if got := usageInt(out, c.field); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.drv, c.field, got, c.want)
		}
	}
	// Claude 4.5 counts a top-level system prompt by its text alone (task
	// 264, testdata/real/anthropic/template/).
	_, _, out := do("anthropic", "/v1/messages", shapesAnth, `{"model":"claude-haiku-4-5","max_tokens":16,"system":"ok",`+probe+`}`)
	if got := usageInt(out, "input_tokens"); got != 12+1 {
		t.Errorf("with system: input_tokens %d, want %d", got, 12+1)
	}
}

// TestResponseModelIsSnapshot: openai and anthropic answer models[].version
// (or models[].id) even when called by alias; the gateways keep the id.
func TestResponseModelIsSnapshot(t *testing.T) {
	do := shapesEngine(t)
	cases := []struct {
		drv, path string
		h         map[string]string
		asked     string
		want      string
	}{
		{"openai", "/v1/chat/completions", shapesOAI, "gpt-4o-mini", "gpt-4o-mini-2024-07-18"},
		{"openai", "/v1/chat/completions", shapesOAI, "mini-alias", "gpt-4o-mini-2024-07-18"},
		{"anthropic", "/v1/messages", shapesAnth, "claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"opencode", "/zen/go/v1/chat/completions", shapesOAI, "mini-alias", "gpt-4o-mini"},
	}
	for _, c := range cases {
		status, _, out := do(c.drv, c.path, c.h, `{"model":"`+c.asked+`","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`)
		if status != 200 {
			t.Fatalf("%s %s: status %d", c.drv, c.asked, status)
		}
		if got := out["model"]; got != c.want {
			t.Errorf("%s asked %s: model %v, want %s", c.drv, c.asked, got, c.want)
		}
	}
	// The stream carries the snapshot in every chunk and in message_start.
	e := newEngine(t, shapesCfg)
	w := call(t, e, "openai", "/v1/chat/completions", shapesOAI, `{"model":"mini-alias","max_tokens":4,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if b := string(w.Body()); strings.Contains(b, `"model":"gpt-4o-mini"`) || !strings.Contains(b, `"model":"gpt-4o-mini-2024-07-18"`) {
		t.Errorf("stream model: %s", b)
	}
	w = call(t, e, "anthropic", "/v1/messages", shapesAnth, `{"model":"claude-haiku-4-5","max_tokens":4,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if b := string(w.Body()); !strings.Contains(b, `"model":"claude-haiku-4-5-20251001"`) {
		t.Errorf("message_start model: %s", b)
	}
}

// TestRetiredModelIsUnknown: from retires_at on, the model answers like a
// model that does not exist; before it, it answers normally.
func TestRetiredModelIsUnknown(t *testing.T) {
	do := shapesEngine(t)
	status, _, out := do("openai", "/v1/chat/completions", shapesOAI, `{"model":"old-model","messages":[{"role":"user","content":"hi"}]}`)
	if status != 404 {
		t.Fatalf("retired model: status %d %v", status, out)
	}
	if msg, _ := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "old-model") {
		t.Errorf("message %q", msg)
	}
	if status, _, _ := do("anthropic", "/v1/messages", shapesAnth, `{"model":"old-model","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`); status != 404 {
		t.Errorf("anthropic retired: status %d", status)
	}
	if status, _, _ := do("openai", "/v1/chat/completions", shapesOAI, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`); status != 200 {
		t.Errorf("model before retires_at: status %d", status)
	}
}

// TestAuthBeforeBody: OpenAI, Anthropic and OpenRouter refuse a bad key
// before they look at the body (an unknown model or invalid JSON is a 401);
// OpenCode Go checks the session, then the model, then the key.
func TestAuthBeforeBody(t *testing.T) {
	do := shapesEngine(t)
	bad := map[string]string{"Authorization": "Bearer nope"}
	badAnth := map[string]string{"x-api-key": "nope", "anthropic-version": "2023-06-01"}
	unknown := `{"model":"no-such-model","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
	for _, c := range []struct {
		drv, path string
		h         map[string]string
	}{
		{"openai", "/v1/chat/completions", bad},
		{"anthropic", "/v1/messages", badAnth},
		{"openrouter", "/api/v1/chat/completions", bad},
	} {
		for _, body := range []string{unknown, `{not json`} {
			if status, _, out := do(c.drv, c.path, c.h, body); status != 401 {
				t.Errorf("%s bad key, body %q: status %d %v", c.drv, body, status, out)
			}
		}
	}
	// OpenCode: the model is checked before the key.
	status, _, out := do("opencode", "/zen/go/v1/chat/completions", bad, unknown)
	er, _ := out["error"].(map[string]any)
	if msg, _ := er["message"].(string); status != 400 || msg != "Upstream request failed: Model is unavailable." {
		t.Errorf("opencode bad key + unknown model: status %d %v, want the model error", status, out)
	}
	// A known model with a bad key is the key error.
	status, _, out = do("opencode", "/zen/go/v1/chat/completions", bad, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if er, _ := out["error"].(map[string]any); status != 401 || er["message"] != "Upstream request failed: Invalid credential" {
		t.Errorf("opencode bad key: status %d %v", status, out)
	}
}

// TestOpenCodeSessionFirst: a missing session wins over the body and the
// key, with OpenCode's MissingSessionID error (require_session is always on,
// ADR 0059).
func TestOpenCodeSessionFirst(t *testing.T) {
	e := newEngine(t, shapesCfg)
	drv, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{not json`, `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`} {
		w := core.NewMemWriter()
		h := driver.MapHeaders{"authorization": "Bearer nope"}
		e.Handle(context.Background(), engine.Input{Driver: drv, Kind: driver.KindChat, Method: "POST", Path: "/zen/go/v1/chat/completions", Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
		if w.StatusCode() != 400 || !strings.Contains(string(w.Body()), "MissingSessionID") {
			t.Errorf("body %q: status %d %s", body, w.StatusCode(), w.Body())
		}
	}
}
