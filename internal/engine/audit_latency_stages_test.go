package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 383 (auditoria do DONE, task 362): R8 — cada etapa de latência cai no
// ponto certo do stream (spec 008). The real clock is used with one stage
// set at a time; only lower bounds are asserted, so the tests do not depend
// on scheduler speed. Between two frames the bound gives pacingSlack: the
// pacing counts from the planned end of the previous wait (ADR 0069), so a
// gap is shorter than the stage by the lateness of the earlier write.

type auditFrame struct {
	at   time.Time
	data string
}

// auditWriter records every SSE write with its wall time.
type auditWriter struct {
	*core.MemWriter
	mu     sync.Mutex
	frames []auditFrame
}

func newAuditWriter() *auditWriter { return &auditWriter{MemWriter: core.NewMemWriter()} }

func (w *auditWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "data:") {
		w.mu.Lock()
		w.frames = append(w.frames, auditFrame{time.Now(), string(p)})
		w.mu.Unlock()
	}
	return w.MemWriter.Write(p)
}

func (w *auditWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

// auditStream runs one streaming request and returns the frames and the
// start time.
func auditStream(t *testing.T, e *engine.Engine, drvName, path string, kind driver.Kind, headers map[string]string, body string) ([]auditFrame, time.Time) {
	t.Helper()
	drv, err := driver.New(drvName, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := driver.MapHeaders{}
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	w := newAuditWriter()
	t0 := time.Now()
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	return w.frames, t0
}

func auditOAIStream(t *testing.T, e *engine.Engine, headers map[string]string, body string) ([]auditFrame, time.Time) {
	t.Helper()
	return auditStream(t, e, "openai", "/v1/chat/completions", driver.KindChat, impHeaders(headers), body)
}

func auditAnthStream(t *testing.T, e *engine.Engine, headers map[string]string, body string) ([]auditFrame, time.Time) {
	t.Helper()
	h := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	for k, v := range headers {
		h[k] = v
	}
	return auditStream(t, e, "anthropic", "/v1/messages", driver.KindMessages, h, body)
}

// auditIndex returns the first frame at or after from whose payload contains
// sub, or fails.
func auditIndex(t *testing.T, fr []auditFrame, from int, sub string) int {
	t.Helper()
	for i := from; i < len(fr); i++ {
		if strings.Contains(fr[i].data, sub) {
			return i
		}
	}
	for i, f := range fr {
		t.Logf("frame %d: %q", i, f.data)
	}
	t.Fatalf("no frame containing %q from %d", sub, from)
	return -1
}

func auditCfg(stage string) string {
	return "server: {seed: 42}\nmodels:\n  - {id: m-stage, errors: none, latency: {" + stage + "}}\n"
}

const stageD = 40 * time.Millisecond

func atLeast(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-pacingSlack {
		t.Errorf("%s: %v, want >= %v", what, got, want)
	}
}

// pacingSlack is the lateness of one write that the next wait absorbs
// (ADR 0069); measured well under 1 ms on an idle machine.
const pacingSlack = 2 * time.Millisecond

const (
	oaiStreamBody  = `{"model":"m-stage","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	anthStreamBody = `{"model":"m-stage","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	oaiToolBody    = `{"model":"m-stage","stream":true,"messages":[{"role":"user","content":"hi"}],` + `"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	anthToolBody   = `{"model":"m-stage","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}],` + `"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	toolHeader     = `[{"name":"read_file","arguments":{"path":"/some/long/path/to/a/file.txt"}}]`
)

// TestAuditStageQueueAndTTFT: queue + ttft happen before the first byte.
func TestAuditStageQueueAndTTFT(t *testing.T) {
	e := newEngine(t, auditCfg("queue: 40ms, ttft: 40ms, itl: 0ms"))
	fr, t0 := auditOAIStream(t, e, map[string]string{"x-wwz-response": "a b c"}, oaiStreamBody)
	atLeast(t, "openai first byte", fr[0].at.Sub(t0), 2*stageD)
	fr, t0 = auditAnthStream(t, e, map[string]string{"x-wwz-response": "a b c"}, anthStreamBody)
	atLeast(t, "anthropic first byte", fr[0].at.Sub(t0), 2*stageD)
}

// TestAuditStageITL: itl separates the content deltas.
func TestAuditStageITL(t *testing.T) {
	e := newEngine(t, auditCfg("ttft: 0ms, itl: 40ms"))
	hdr := map[string]string{"x-wwz-response": "xalpha xbeta xgamma xomega"}
	// Pacing aims at the planned end (ADR 0069): a late first delta is
	// absorbed by the next waits, so the last one is measured from the start.
	fr, t0 := auditOAIStream(t, e, hdr, oaiStreamBody)
	a, d := auditIndex(t, fr, 0, "xalpha"), auditIndex(t, fr, 0, "xomega")
	if d <= a {
		t.Fatalf("openai: deltas out of order (%d, %d)", a, d)
	}
	atLeast(t, "openai content deltas", fr[d].at.Sub(t0), time.Duration(d-a)*stageD)

	fr, t0 = auditAnthStream(t, e, hdr, anthStreamBody)
	a, d = auditIndex(t, fr, 0, "xalpha"), auditIndex(t, fr, 0, "xomega")
	if d <= a {
		t.Fatalf("anthropic: deltas out of order (%d, %d)", a, d)
	}
	atLeast(t, "anthropic content deltas", fr[d].at.Sub(t0), time.Duration(d-a)*stageD)
}

// TestAuditStageReasoningITL: reasoning_itl separates the thinking deltas
// (visible on Anthropic) and precedes the first content delta; itl does not.
func TestAuditStageReasoningITL(t *testing.T) {
	e := newEngine(t, auditCfg("ttft: 0ms, itl: 0ms, reasoning_itl: 4ms"))
	body := `{"model":"m-stage","max_tokens":2000,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
	fr, _ := auditAnthStream(t, e, map[string]string{"x-wwz-response": "answer"}, body)
	first := auditIndex(t, fr, 0, "thinking_delta")
	last := first
	for i := first; i < len(fr); i++ {
		if strings.Contains(fr[i].data, "thinking_delta") {
			last = i
		}
	}
	if last == first {
		t.Fatalf("want several thinking deltas, got %d", last-first+1)
	}
	atLeast(t, "anthropic thinking deltas", fr[last].at.Sub(fr[first].at), time.Duration(last-first)*4*time.Millisecond)
	c := auditIndex(t, fr, last, "answer")
	if c <= last {
		t.Fatalf("content (%d) must come after the thinking (%d)", c, last)
	}

	// OpenAI hides the reasoning but still spends its time before the first
	// content delta.
	body = `{"model":"m-stage","stream":true,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`
	fr, t0 := auditOAIStream(t, e, map[string]string{"x-wwz-response": "answer"}, body)
	atLeast(t, "openai content after reasoning", fr[auditIndex(t, fr, 0, "answer")].at.Sub(t0), 20*time.Millisecond)
}

// TestAuditStageToolCallDelay: tool_call_delay is the pause before the first
// chunk of the tool call, and the content/ttft before it are not delayed by it.
func TestAuditStageToolCallDelay(t *testing.T) {
	e := newEngine(t, auditCfg("ttft: 0ms, itl: 0ms, tool_call_delay: 40ms"))
	hdr := map[string]string{"x-wwz-tool-calls": toolHeader}
	fr, _ := auditOAIStream(t, e, hdr, oaiToolBody)
	i := auditIndex(t, fr, 0, "read_file")
	atLeast(t, "openai tool call start", fr[i].at.Sub(fr[i-1].at), stageD)

	fr, _ = auditAnthStream(t, e, hdr, anthToolBody)
	i = auditIndex(t, fr, 0, "read_file")
	atLeast(t, "anthropic tool_use start", fr[i].at.Sub(fr[i-1].at), stageD)
}

// TestAuditStageToolArgsITL: tool_args_itl separates the argument fragments.
func TestAuditStageToolArgsITL(t *testing.T) {
	e := newEngine(t, auditCfg("ttft: 0ms, itl: 0ms, tool_args_itl: 40ms"))
	hdr := map[string]string{"x-wwz-tool-calls": toolHeader}

	fr, _ := auditOAIStream(t, e, hdr, oaiToolBody)
	start := auditIndex(t, fr, 0, "read_file")
	args := 0
	for i := start + 1; i < len(fr); i++ {
		if strings.Contains(fr[i].data, `"arguments"`) && !strings.Contains(fr[i].data, `"arguments":""`) {
			args++
			atLeast(t, "openai argument fragment", fr[i].at.Sub(fr[i-1].at), stageD)
		}
	}
	if args < 2 {
		t.Fatalf("openai: want several argument fragments, got %d", args)
	}

	fr, _ = auditAnthStream(t, e, hdr, anthToolBody)
	start = auditIndex(t, fr, 0, "read_file")
	args = 0
	for i := start + 1; i < len(fr); i++ {
		// The first partial_json is empty and goes out with the tool_use start.
		if strings.Contains(fr[i].data, "input_json_delta") && !strings.Contains(fr[i].data, `"partial_json":""`) {
			args++
			atLeast(t, "anthropic partial_json fragment", fr[i].at.Sub(fr[i-1].at), stageD)
		}
	}
	if args < 2 {
		t.Fatalf("anthropic: want several partial_json fragments, got %d", args)
	}
}

// TestAuditStageFinishDelay: finish_delay is the pause before the final event.
func TestAuditStageFinishDelay(t *testing.T) {
	e := newEngine(t, auditCfg("ttft: 0ms, itl: 0ms, finish_delay: 40ms"))
	hdr := map[string]string{"x-wwz-response": "xalpha xbeta"}

	fr, _ := auditOAIStream(t, e, hdr, oaiStreamBody)
	f := auditIndex(t, fr, 0, `"finish_reason":"stop"`)
	atLeast(t, "openai final chunk", fr[f].at.Sub(fr[f-1].at), stageD)

	fr, _ = auditAnthStream(t, e, hdr, anthStreamBody)
	last := auditIndex(t, fr, 0, "xbeta")
	f = auditIndex(t, fr, last, "message_delta")
	atLeast(t, "anthropic message_delta", fr[f].at.Sub(fr[last].at), stageD)
}

// auditHold keeps n streaming requests in flight (their writer blocks after
// the first frame) so a later request sees n active ones. release lets them
// finish.
func auditHold(t *testing.T, e *engine.Engine, model string, n int) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var wg sync.WaitGroup
	started := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &gateWriter{MemWriter: core.NewMemWriter(), gate: gate, started: started}
			impHandle(context.Background(), t, e, w, nil, impBody(model, `,"stream":true`))
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	return func() {
		close(gate)
		wg.Wait()
	}
}

// gateWriter blocks on its first data frame until gate is closed.
type gateWriter struct {
	*core.MemWriter
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (w *gateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "data:") {
		w.once.Do(func() {
			w.started <- struct{}{}
			<-w.gate
		})
	}
	return w.MemWriter.Write(p)
}

func (w *gateWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

const loadCfg = `
server: {seed: 42}
models:
  - id: m-load
    errors: none
    latency: {ttft: 100ms, itl: 0ms, concurrency: 4, load_factor: 5, clock: logical}
  - id: m-poly
    errors: none
    latency: {model: polynomial, prefill_overhead: 100ms, prefill_per_token: 0ms, itl: 0ms, clock: logical}
`

// TestAuditLoadFactor: spec 008 — the stages grow by
// 1 + (load_factor - 1) × active / concurrency, and the polynomial model by
// 1 + 0.05 × active. Logical clock: the Record carries queue + ttft.
func TestAuditLoadFactor(t *testing.T) {
	e := newEngine(t, loadCfg)

	impChatCall(t, e, nil, impBody("m-load", ""))
	if rec := impLastRecord(t, e); rec.TTFTMS != 100 {
		t.Fatalf("idle: ttft %d ms, want 100", rec.TTFTMS)
	}
	release := auditHold(t, e, "m-load", 2)
	impChatCall(t, e, nil, impBody("m-load", ""))
	rec := impLastRecord(t, e)
	release()
	// Two active of concurrency 4: 1 + 4 × 2/4 = 3.
	if rec.TTFTMS != 300 {
		t.Fatalf("2 active: ttft %d ms, want 300", rec.TTFTMS)
	}

	impChatCall(t, e, nil, impBody("m-poly", ""))
	if rec := impLastRecord(t, e); rec.TTFTMS != 100 {
		t.Fatalf("poly idle: ttft %d ms, want 100", rec.TTFTMS)
	}
	release = auditHold(t, e, "m-poly", 2)
	impChatCall(t, e, nil, impBody("m-poly", ""))
	rec = impLastRecord(t, e)
	release()
	// 100 × (1 + 0.05 × 2) = 110.
	if rec.TTFTMS != 110 {
		t.Fatalf("poly 2 active: ttft %d ms, want 110", rec.TTFTMS)
	}
}
