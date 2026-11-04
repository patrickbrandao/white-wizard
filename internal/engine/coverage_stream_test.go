package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

const covStreamCfg = `
server: {seed: 5}
profiles:
  latency:
    default: {ttft: 0ms, itl: 0ms, clock: logical}
    real: {ttft: 300ms, itl: 40ms}
    slowfin: {ttft: 0ms, itl: 0ms, finish_delay: 400ms}
  errors:
    mid-event: {mid_stream_rate: 100, mid_stream_types: [stream_error_event], after_chunks: 1}
    mid-trunc: {mid_stream_rate: 100, mid_stream_types: [stream_truncated], after_chunks: 1}
    mid-reset: {mid_stream_rate: 100, mid_stream_types: [stream_reset], after_chunks: 1}
    mid-hang: {mid_stream_rate: 100, mid_stream_types: [stream_hang], after_chunks: 1, hang_duration: 1ms}
    mid-hang-long: {mid_stream_rate: 100, mid_stream_types: [stream_hang], after_chunks: 1, hang_duration: 1h}
    mid-malformed: {mid_stream_rate: 100, mid_stream_types: [stream_malformed], after_chunks: 1}
    mid-dup: {mid_stream_rate: 100, mid_stream_types: [duplicate_frames], after_chunks: 1}
    mid-slow: {mid_stream_rate: 100, mid_stream_types: [stream_slow_tail], after_chunks: 1}
    mid-usage: {mid_stream_rate: 100, mid_stream_types: [usage_missing], after_chunks: 1}
    mid-ratio: {mid_stream_rate: 100, mid_stream_types: [stream_truncated], after_ratio: 0.0001}
models:
  - {id: s-plain}
  - {id: s-event, errors: mid-event}
  - {id: s-trunc, errors: mid-trunc}
  - {id: s-reset, errors: mid-reset}
  - {id: s-hang, errors: mid-hang}
  - {id: s-hang-long, errors: mid-hang-long}
  - {id: s-malformed, errors: mid-malformed}
  - {id: s-dup, errors: mid-dup}
  - {id: s-slow, errors: mid-slow}
  - {id: s-usage, errors: mid-usage}
  - {id: s-ratio, errors: mid-ratio}
  - {id: s-real, latency: real}
  - {id: s-fin, latency: slowfin}
`

const covStreamBody = `,"stream":true,"max_tokens":60`

func TestCoverageMidStreamFaultsOpenAI(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	for _, id := range []string{"s-event", "s-trunc", "s-reset", "s-hang", "s-malformed", "s-dup", "s-slow", "s-usage", "s-ratio"} {
		w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody(id, covStreamBody+`,"stream_options":{"include_usage":true}`))
		if w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", id, w.StatusCode(), w.Body())
		}
		rec := impLastRecord(t, e)
		if id == "s-usage" || id == "s-slow" || id == "s-dup" || id == "s-malformed" {
			continue
		}
		if rec.MidStream == "" && rec.Error == "" {
			t.Errorf("%s: record %+v", id, rec)
		}
	}
}

func TestCoverageMidStreamFaultsAnthropic(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	for _, id := range []string{"s-event", "s-trunc", "s-malformed", "s-dup"} {
		w := call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"},
			`{"model":"`+id+`","max_tokens":60,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", id, w.StatusCode(), w.Body())
		}
	}
}

func TestCoverageMidFaultOnNonStreaming(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	for _, id := range []string{"s-event", "s-trunc", "s-reset"} {
		w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody(id, `,"max_tokens":60`))
		_ = w
	}
}

func TestCoverageHangCancelled(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	w := core.NewMemWriter()
	impHandle(ctx, t, e, w, nil, impBody("s-hang-long", covStreamBody))
}

func TestCoverageCancelDuringStreamPhases(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	for _, model := range []string{"s-real", "s-fin"} {
		for _, d := range []time.Duration{10 * time.Millisecond, 200 * time.Millisecond} {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			w := core.NewMemWriter()
			impHandle(ctx, t, e, w, nil, impBody(model, covStreamBody))
			cancel()
		}
	}
	// Non-streaming cancelled while waiting for the plan.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	impHandle(ctx, t, e, core.NewMemWriter(), nil, impBody("s-real", `,"max_tokens":60`))
}

// failWriter fails the n-th Write onwards.
type failWriter struct {
	*core.MemWriter
	n, count int
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.count >= w.n {
		return 0, errors.New("broken pipe")
	}
	w.count++
	return w.MemWriter.Write(p)
}

func (w *failWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

func TestCoverageWriteFailures(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	for n := 0; n < 8; n++ {
		w := &failWriter{MemWriter: core.NewMemWriter(), n: n}
		impHandle(context.Background(), t, e, w, nil, impBody("s-plain", ","+impTools+covStreamBody+`,"tool_choice":"required"`))
	}
	for n := 0; n < 2; n++ {
		w := &failWriter{MemWriter: core.NewMemWriter(), n: n}
		impHandle(context.Background(), t, e, w, nil, impBody("s-plain", `,"max_tokens":30`))
	}
}

func TestCoverageChunkCap(t *testing.T) {
	e := newEngine(t, `
server: {seed: 5, max_chunks_per_stream: 2, ping_interval: 5ms}
profiles:
  latency:
    default: {ttft: 0ms, itl: 12ms}
`)
	s, _ := impStreamCall(t, e, nil, impBody("wwz-fast", ","+impTools+`,"stream":true,"tool_choice":"required"`))
	if s.raw == "" {
		t.Fatal("empty stream")
	}
}

const covTimelineScenarios = `
name: cov-timeline
steps:
  - id: s
    timeline:
      - {type: reasoning, text: "think"}
      - {type: text, text: "A"}
      - {type: ping}
      - {type: delay, ms: 1}
      - {type: usage}
      - {type: error}
`

func TestCoverageTimelineEvents(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	impAddScenario(t, e, covTimelineScenarios)
	impAddScenario(t, e, `
name: cov-timeline-err
steps:
  - id: s
    timeline:
      - {type: text, text: "A"}
      - {type: error, error: overloaded, text: "boom"}
`)
	impAddScenario(t, e, `
name: cov-timeline-nofinish
steps:
  - id: s
    timeline:
      - {type: text, text: "A"}
`)
	impAddScenario(t, e, `
name: cov-timeline-tool
steps:
  - id: s
    timeline:
      - {type: tool_call, name: read_file}
      - {type: tool_call, name: read_file}
`)
	impAddScenario(t, e, `
name: cov-timeline-delay
steps:
  - id: s
    timeline:
      - {type: delay, ms: 500}
      - {type: text, text: "A"}
`)
	for _, sc := range []string{"cov-timeline", "cov-timeline-err", "cov-timeline-nofinish", "cov-timeline-tool"} {
		w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": sc}), impBody("s-plain", ","+impTools+covStreamBody))
		if w.StatusCode() != 200 {
			t.Fatalf("%s stream: %d", sc, w.StatusCode())
		}
		w = call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": sc}), impBody("s-plain", ","+impTools))
		if sc == "cov-timeline" || sc == "cov-timeline-err" {
			if w.StatusCode() != 500 && w.StatusCode() != 503 {
				t.Errorf("%s non-stream error status %d", sc, w.StatusCode())
			}
		}
	}
	// A delay event cut by a gone client.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	impHandle(ctx, t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-timeline-delay"}, impBody("s-real", covStreamBody))
}

func TestCoverageScenarioStallAndCut(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	impAddScenario(t, e, `
name: cov-stall
steps:
  - id: s
    content: {tokens: 40}
    reasoning: "deep thought"
    stall: {after_tokens: 2, duration: 1h}
    headers: {x-extra: "1"}
    latency: {ttft: 0ms}
    moderation: finish_reason
    tool_misbehavior: unknown_tool
`)
	impAddScenario(t, e, `
name: cov-cut
steps:
  - id: s
    content: {tokens: 40}
    cut_after_tokens: 3
`)
	impAddScenario(t, e, `
name: cov-stall-short
steps:
  - id: s
    content: {tokens: 40}
    stall: {after_tokens: 1, ms: 5}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	impHandle(ctx, t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-stall"}, impBody("s-plain", ","+impTools+covStreamBody))
	impHandle(context.Background(), t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-cut"}, impBody("s-plain", covStreamBody))
	impHandle(context.Background(), t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-stall-short"}, impBody("s-plain", covStreamBody))
	impHandle(context.Background(), t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-stall-short"}, impBody("s-plain", `,"max_tokens":60`))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel2()
	impHandle(ctx2, t, e, core.NewMemWriter(), map[string]string{"x-wwz-scenario": "cov-stall"}, impBody("s-plain", `,"max_tokens":60`))
}

func TestCoverageResponsesStreamTools(t *testing.T) {
	e := newEngine(t, covStreamCfg)
	body := `{"model":"s-plain","input":"do it","stream":true,"tool_choice":"required","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}]}`
	w := call(t, e, "openai", "/v1/responses", impHeaders(nil), body)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), "read_file") {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
}

var _ = engine.Record{}
