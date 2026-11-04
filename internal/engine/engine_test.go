package engine_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

func newEngine(t *testing.T, yaml string) *engine.Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	reg := scenario.NewRegistry()
	if _, errs := reg.LoadDir("../../scenarios"); len(errs) > 0 {
		t.Fatal(errs)
	}
	m := engine.NewMetrics(metrics.New(1000), nil)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := engine.New(cfg, limits.NewMemory(), reg, m, log)
	e.Recorder = engine.NewRecorder(100)
	return e
}

func call(t *testing.T, e *engine.Engine, drvName, path string, headers map[string]string, body string) *core.MemWriter {
	t.Helper()
	drv, err := driver.New(drvName, nil)
	if err != nil {
		t.Fatal(err)
	}
	var kind driver.Kind
	for _, rt := range drv.Routes() {
		if rt.Path == path {
			kind = rt.Kind
		}
	}
	if kind == "" {
		t.Fatalf("no route for %s", path)
	}
	h := driver.MapHeaders{}
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	if drvName == "opencode" && h["x-opencode-session"] == "" {
		h["x-opencode-session"] = "ses_1" // required, ADR 0059
	}
	w := core.NewMemWriter()
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
	return w
}

const fastCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`

func TestOpenAIChatNonStream(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"},
		`{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d body %s", w.StatusCode(), w.Body())
	}
	var resp struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body(), &resp); err != nil {
		t.Fatal(err, string(w.Body()))
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content == "" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("bad response: %s", w.Body())
	}
	if resp.Usage.PromptTokens == 0 || resp.Usage.CompletionTokens == 0 {
		t.Fatalf("usage missing: %s", w.Body())
	}
	if w.Headers()["x-request-id"] == "" {
		t.Fatalf("missing x-request-id: %v", w.Headers())
	}
}

func TestOpenAIStreamWithTools(t *testing.T) {
	e := newEngine(t, fastCfg)
	body := `{"model":"wwz-fast","stream":true,"stream_options":{"include_usage":true},"tool_choice":"required",
	  "messages":[{"role":"user","content":"read the file"}],
	  "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"lines":{"type":"integer","minimum":1,"maximum":50}}}}}]}`
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, body)
	out := string(w.Body())
	if w.StatusCode() != 200 || !strings.HasPrefix(w.Headers()["Content-Type"], "text/event-stream") {
		t.Fatalf("status %d headers %v body %s", w.StatusCode(), w.Headers(), out)
	}
	if !strings.Contains(out, `"name":"read_file"`) || !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("stream missing tool call / finish / DONE:\n%s", out)
	}
	// Reassemble the arguments and check they are valid JSON with the required key.
	var args strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct{ Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line[6:]), &chunk) == nil && len(chunk.Choices) > 0 {
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args.String()), &m); err != nil || m["path"] == nil {
		t.Fatalf("tool args invalid: %q err=%v", args.String(), err)
	}
	if !strings.Contains(out, `"usage":{`) {
		t.Fatalf("usage chunk missing")
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	e := newEngine(t, fastCfg)
	body := `{"model":"wwz-fast","max_tokens":2048,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
	w := call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}, body)
	out := string(w.Body())
	if w.StatusCode() != 200 {
		t.Fatalf("status %d body %s", w.StatusCode(), out)
	}
	for _, ev := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, ev) {
			t.Fatalf("missing %s in\n%s", ev, out)
		}
	}
	if !strings.Contains(out, `"thinking_delta"`) {
		t.Fatalf("thinking expected:\n%s", out)
	}
}

func TestAuthAndModelErrors(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := call(t, e, "openai", "/v1/chat/completions", nil, `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`)
	if w.StatusCode() != 401 || !strings.Contains(string(w.Body()), "invalid_api_key") && !strings.Contains(string(w.Body()), "API key") {
		t.Fatalf("expected 401: %d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	if w.StatusCode() != 404 || !strings.Contains(string(w.Body()), "model_not_found") {
		t.Fatalf("expected 404 model_not_found: %d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-return-error": "rate_limit"}, `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`)
	if w.StatusCode() != 429 || w.Headers()["retry-after"] == "" {
		t.Fatalf("expected 429 with Retry-After: %d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
}

func TestRateLimitRPM(t *testing.T) {
	cfg := fastCfg + `
keys:
  - {key: k-limited, label: limited, limits: {rpm: 2}}
`
	e := newEngine(t, cfg)
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`
	h := map[string]string{"Authorization": "Bearer k-limited"}
	for i := 0; i < 2; i++ {
		if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 200 {
			t.Fatalf("req %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, body)
	if w.StatusCode() != 429 {
		t.Fatalf("expected 429: %d %s", w.StatusCode(), w.Body())
	}
	if w.Headers()["x-ratelimit-limit-requests"] != "2" {
		t.Fatalf("rate limit headers: %v", w.Headers())
	}
}

// TestRateLimitDenialEvent: a request refused by the store (429 rpm, 403
// ban) shows up as an error event (spec 012) without releasing counters.
func TestRateLimitDenialEvent(t *testing.T) {
	cfg := fastCfg + `
keys:
  - {key: k-limited, label: limited, limits: {rpm: 1}}
`
	e := newEngine(t, cfg)
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`
	h := map[string]string{"Authorization": "Bearer k-limited", "x-test-id": "t-deny"}
	if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 200 {
		t.Fatalf("first: %d %s", w.StatusCode(), w.Body())
	}
	if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 429 {
		t.Fatalf("expected 429: %d %s", w.StatusCode(), w.Body())
	}
	evs, err := e.Store.Events(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want request + error", evs)
	}
	ev := evs[0]
	if ev.Type != "error" || ev.Status != 429 || ev.Error != "rate_limit" || ev.TestID != "t-deny" || ev.Driver != "openai" || ev.Fields["limit"] != "rpm" {
		t.Fatalf("denial event: %+v", ev)
	}
	if ev.Entity == nil || ev.Entity.Kind != limits.KindKey {
		t.Fatalf("denial event entity: %+v", ev.Entity)
	}
	if evs[1].Type != "request" || evs[1].Status != 200 {
		t.Fatalf("request event: %+v", evs[1])
	}
	if err := e.Store.Ban(context.Background(), *ev.Entity, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	if w := call(t, e, "openai", "/v1/chat/completions", h, body); w.StatusCode() != 403 {
		t.Fatalf("expected 403 after ban: %d %s", w.StatusCode(), w.Body())
	}
	evs, err = e.Store.Events(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if ev := evs[0]; ev.Type != "error" || ev.Status != 403 || ev.Error != string(core.ErrAccountBanned) {
		t.Fatalf("ban denial event: %+v", ev)
	}
}

func TestScenarioFileAgentFlow(t *testing.T) {
	e := newEngine(t, fastCfg)
	h := map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-scenario": "file-agent-flow", "x-wwz-conversation-id": "c1"}
	tools := `"tools":[
	  {"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}},
	  {"type":"function","function":{"name":"write_file","parameters":{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}}},
	  {"type":"function","function":{"name":"edit_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}},
	  {"type":"function","function":{"name":"delete_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]`
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"do the file task"}],` + tools + `}`
	w := call(t, e, "openai", "/v1/chat/completions", h, body)
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	if !strings.Contains(string(w.Body()), `"name": "read_file"`) {
		t.Fatalf("first step should call read_file: %s", w.Body())
	}
	if w.Headers()["x-wwz-scenario"] != "file-agent-flow" || w.Headers()["x-wwz-step"] == "" {
		t.Fatalf("scenario headers: %v", w.Headers())
	}
}

func TestModelsList(t *testing.T) {
	e := newEngine(t, fastCfg)
	drv, _ := driver.New("openai", nil)
	w := core.NewMemWriter()
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: driver.KindModels, Method: "GET", Path: "/v1/models", Headers: driver.MapHeaders{"authorization": "Bearer wwz-key-01"}}, w)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"wwz-fast"`) {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
}
