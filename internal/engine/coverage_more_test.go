package engine_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

const covMoreCfg = `
server: {seed: 5}
profiles:
  latency:
    default: {ttft: 0ms, itl: 0ms, clock: logical}
    real: {ttft: 300ms, itl: 40ms}
  generation:
    echo: {mode: echo}
    long: {output_tokens: {dist: fixed, value: 1000}}
    tools: {tool_calls: {probability: 100}}
  errors:
    tm-tool-hit: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], tool: read_file}}
    mid-hang-long: {mid_stream_rate: 100, mid_stream_types: [stream_hang], after_chunks: 1, hang_duration: 1h}
    mid-reset: {mid_stream_rate: 100, mid_stream_types: [stream_reset], after_chunks: 1}
models:
  - {id: x-plain}
  - {id: x-echo, generation: echo}
  - {id: x-noout, max_output: -1}
  - {id: x-long, generation: long}
  - {id: x-tools, generation: tools}
  - {id: x-tool-hit, errors: tm-tool-hit}
  - {id: x-hang, latency: real, errors: mid-hang-long}
  - {id: x-real, latency: real}
  - {id: x-reset, errors: mid-reset}
`

func TestCoveragePodAndOutputLimits(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	e.SetPodName("pod-7")
	_, w := impChatCall(t, e, nil, impBody("x-noout", ""))
	if w.Headers()["x-wwz-pod"] != "pod-7" {
		t.Fatalf("headers %v", w.Headers())
	}
	impChatCall(t, e, nil, impBody("x-long", `,"max_tokens":5`))
	// A tool-scoped misbehaviour without tools in the request breaks nothing.
	impChatCall(t, e, nil, impBody("x-tool-hit", ""))
	// A parameters value that is not a schema cannot be broken.
	impChatCall(t, e, nil, impBody("x-tool-hit", `,"tools":[{"type":"function","function":{"name":"read_file","parameters":"bogus"}}],"tool_choice":"required"`))
}

func TestCoverageResponsesUsageClamps(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	tools := `"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{}}}]`
	w := call(t, e, "openai", "/v1/responses", impHeaders(nil), `{"model":"x-plain","input":"hi","tool_choice":"none",`+tools+`}`)
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "openai", "/v1/responses", impHeaders(nil), `{"model":"x-long","input":"hi","max_output_tokens":16}`)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), "incomplete") {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
}

// A reply that fits the budget by one token, once the Responses API adds its
// own token, ends incomplete instead of completed.
func TestCoverageResponsesBudgetByOne(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	for max := 1; max <= 20; max++ {
		body := `{"model":"x-echo","input":"one two three four five six seven","max_output_tokens":` + strconv.Itoa(max) + `}`
		if w := call(t, e, "openai", "/v1/responses", impHeaders(nil), body); w.StatusCode() != 200 {
			t.Fatalf("max %d: %d %s", max, w.StatusCode(), w.Body())
		}
	}
}

func TestCoverageGeminiImplicitCache(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1500)
	body := `{"contents":[{"role":"user","parts":[{"text":"` + text + `"}]}]}`
	for i := 0; i < 2; i++ {
		in, _ := covInput(t, "gemini", driver.KindGenerate, "POST", "/v1beta/models/x-plain:generateContent", "", body, map[string]string{"x-goog-api-key": "wwz-key-01"})
		w := core.NewMemWriter()
		e.Handle(context.Background(), in, w)
		if w.StatusCode() != 200 {
			t.Fatalf("%d %s", w.StatusCode(), w.Body())
		}
	}
}

type noFaultWriter struct{ *core.MemWriter }

func (w *noFaultWriter) Fault(core.TransportFault) bool      { return false }
func (w *noFaultWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

func TestCoverageResetWithoutTransportFault(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	w := &noFaultWriter{core.NewMemWriter()}
	impHandle(context.Background(), t, e, w, nil, impBody("x-reset", covStreamBody))
}

func TestCoverageRealClockCancellations(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	impAddScenario(t, e, `
name: cov-stall-real
steps:
  - id: s
    content: {tokens: 40}
    stall: {after_tokens: 1, duration: 1h}
`)
	impAddScenario(t, e, `
name: cov-delay-real
steps:
  - id: s
    timeline:
      - {type: text, text: "A"}
      - {type: delay, ms: 2000}
      - {type: text, text: "B"}
`)
	run := func(model string, h map[string]string, timeout time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		impHandle(ctx, t, e, core.NewMemWriter(), h, impBody(model, covStreamBody))
	}
	run("x-hang", nil, 450*time.Millisecond)
	run("x-real", map[string]string{"x-wwz-scenario": "cov-stall-real"}, 450*time.Millisecond)
	run("x-real", map[string]string{"x-wwz-scenario": "cov-delay-real"}, 450*time.Millisecond)
}

func TestCoverageReasoningWriteFailure(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	impAddScenario(t, e, `
name: cov-reason
steps:
  - id: s
    reasoning: "think hard about it"
    content: "ok"
`)
	for n := 0; n < 16; n++ {
		w := &failWriter{MemWriter: core.NewMemWriter(), n: n}
		impHandle(context.Background(), t, e, w, map[string]string{"x-wwz-scenario": "cov-reason"}, impBody("x-plain", `,"stream":true,"reasoning_effort":"high"`))
		// Anthropic streams the thinking block.
		in, _ := covInput(t, "anthropic", driver.KindMessages, "POST", "/v1/messages", "",
			`{"model":"x-plain","max_tokens":200,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "x-wwz-scenario": "cov-reason"})
		aw := &failWriter{MemWriter: core.NewMemWriter(), n: n}
		e.Handle(context.Background(), in, aw)
		// OpenRouter streams the reasoning field.
		in, _ = covInput(t, "openrouter", driver.KindChat, "POST", "/api/v1/chat/completions", "",
			impBody("x-plain", `,"stream":true,"reasoning":{"effort":"high"}`),
			map[string]string{"x-wwz-scenario": "cov-reason"})
		ow := &failWriter{MemWriter: core.NewMemWriter(), n: n}
		e.Handle(context.Background(), in, ow)
	}
}

func TestCoverageTimelineMoreToolCallsThanTools(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	impAddScenario(t, e, `
name: cov-tl-refusal
steps:
  - id: s
    moderation: refusal
    timeline:
      - {type: text, text: "A"}
      - {type: tool_call, name: read_file}
`)
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "cov-tl-refusal"}), impBody("x-plain", ","+impTools+covStreamBody))
	if w.StatusCode() != 200 {
		t.Fatalf("%d", w.StatusCode())
	}
}

func TestCoverageDefaultToolArgChunks(t *testing.T) {
	e := newEngine(t, covMoreCfg)
	s, _ := impStreamCall(t, e, nil, impBody("x-tools", ","+impTools+`,"stream":true,"tool_choice":"required"`))
	if s.argFrames == 0 {
		t.Fatal("tool arguments streamed")
	}
}
