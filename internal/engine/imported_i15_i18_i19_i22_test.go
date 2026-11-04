package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I15 (relatório de resiliência), I18 (raciocínio inline_tag e signature),
// I19 (faults de transporte nomeados, lado do engine) e I22 (teto de chunks
// por stream, estatísticas finalizadas em drop de stream).

// --- I15 ---------------------------------------------------------------

func i15Engine(t *testing.T) *engine.Engine {
	t.Helper()
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, `
name: i15
steps:
  - id: s
    fail_first: {times: 1, error: rate_limit, retry_after_ms: 120}
    content: ok
`)
	return e
}

// TestI15RetryAfterEarly: I15 — a retry sent before Retry-After is counted
// as early, and the 429 carries retry-after-ms.
func TestI15RetryAfterEarly(t *testing.T) {
	e := i15Engine(t)
	h := impHeaders(map[string]string{"x-wwz-scenario": "i15", "x-wwz-conversation-id": "c-early", "x-test-id": "t-early"})
	w := call(t, e, "openai", "/v1/chat/completions", h, impBody("wwz-fast", ""))
	if w.StatusCode() != 429 || w.Headers()["retry-after-ms"] != "120" {
		t.Fatalf("first attempt: %d %v", w.StatusCode(), w.Headers())
	}
	if w = call(t, e, "openai", "/v1/chat/completions", h, impBody("wwz-fast", "")); w.StatusCode() != 200 {
		t.Fatalf("retry: %d %s", w.StatusCode(), w.Body())
	}
	r := e.Resilience.Report("t-early")
	if r == nil || r.Requests != 2 || r.Errors != 1 || r.Retried != 1 || r.RetryAfterSent != 1 || r.RetryAfterEarly != 1 || r.RetryAfterHonor != 0 {
		t.Fatalf("report %+v", r)
	}
	if r.Statuses[429] != 1 || r.Statuses[200] != 1 || len(r.Intervals) != 1 {
		t.Fatalf("statuses/intervals %+v", r)
	}
}

// TestI15RetryAfterHonoured: I15 — waiting the Retry-After counts as honoured.
func TestI15RetryAfterHonoured(t *testing.T) {
	e := i15Engine(t)
	h := impHeaders(map[string]string{"x-wwz-scenario": "i15", "x-wwz-conversation-id": "c-hon", "x-test-id": "t-hon"})
	call(t, e, "openai", "/v1/chat/completions", h, impBody("wwz-fast", ""))
	time.Sleep(125 * time.Millisecond)
	call(t, e, "openai", "/v1/chat/completions", h, impBody("wwz-fast", ""))
	r := e.Resilience.Report("t-hon")
	if r == nil || r.RetryAfterHonor != 1 || r.RetryAfterEarly != 0 {
		t.Fatalf("report %+v", r)
	}
	if r.Intervals[0] < 120 {
		t.Fatalf("interval %v ms", r.Intervals[0])
	}
}

// TestI15NoTestIDNoReport: I15 — requests without X-Test-Id are not tracked.
func TestI15NoTestIDNoReport(t *testing.T) {
	e := i15Engine(t)
	call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i15"}), impBody("wwz-fast", ""))
	if len(e.Resilience.List(0)) != 0 {
		t.Fatal("untracked request produced a report")
	}
}

// TestI15BackoffGrowing: I15 — growing gaps between retries flag an
// exponential-like backoff.
func TestI15BackoffGrowing(t *testing.T) {
	rs := engine.NewResilience()
	req := &core.Request{}
	er := &core.Error{Kind: core.ErrOverloaded}
	rs.Observe("t", req, 503, er)
	for _, gap := range []time.Duration{2 * time.Millisecond, 12 * time.Millisecond} {
		time.Sleep(gap)
		rs.Observe("t", req, 503, er)
	}
	time.Sleep(35 * time.Millisecond)
	rs.Observe("t", req, 200, nil)
	r := rs.Report("t")
	if r.Retried != 3 || !r.BackoffGrowing || r.RetryAfterSent != 0 {
		t.Fatalf("report %+v", r)
	}
	// Constant gaps are not growing.
	rs.Reset()
	for i := 0; i < 4; i++ {
		rs.Observe("c", req, 503, er)
	}
	if rs.Report("c").BackoffGrowing {
		t.Fatal("back-to-back retries flagged as growing backoff")
	}
}

// TestI15RetryAfterConsumedStream: I15 — the report says whether the client
// re-sent a request after consuming a stream.
func TestI15RetryAfterConsumedStream(t *testing.T) {
	b, _ := json.Marshal(engine.TestReport{})
	if !strings.Contains(string(b), `"resent_after_stream"`) || !strings.Contains(string(b), `"streams_consumed"`) {
		t.Fatalf("report fields missing: %s", b)
	}
	e := newEngine(t, fastCfg)
	h := impHeaders(map[string]string{"x-test-id": "t-resend"})
	stream := impBody("wwz-fast", `,"stream":true`)
	call(t, e, "openai", "/v1/chat/completions", h, stream)
	call(t, e, "openai", "/v1/chat/completions", h, stream)
	// A different body (next turn) is not a re-send.
	call(t, e, "openai", "/v1/chat/completions", h, `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"other"}]}`)
	r := e.Resilience.Report("t-resend")
	if r == nil || r.StreamsConsumed != 3 || r.ResentAfterStream != 1 {
		t.Fatalf("report %+v", r)
	}
}

// TestI15ResendAfterBrokenStream: I15 — a stream cut by a mid-stream error
// event still counts as consumed, and the re-send is reported.
func TestI15ResendAfterBrokenStream(t *testing.T) {
	e := i19Engine(t, "{mid_stream_rate: 100, mid_stream_types: [stream_error_event], after_chunks: 1}")
	h := impHeaders(map[string]string{"x-test-id": "t-broken"})
	body := impBody("m-f", `,"stream":true`)
	call(t, e, "openai", "/v1/chat/completions", h, body)
	call(t, e, "openai", "/v1/chat/completions", h, body)
	r := e.Resilience.Report("t-broken")
	if r == nil || r.StreamsConsumed != 2 || r.ResentAfterStream != 1 {
		t.Fatalf("report %+v", r)
	}
}

// TestI15ObserveWithoutRaw: the fingerprint falls back to the decoded request
// when the raw body is absent; a non-stream request clears the marker.
func TestI15ObserveWithoutRaw(t *testing.T) {
	rs := engine.NewResilience()
	s := &core.Request{Model: "m", Stream: true, Messages: []core.Message{{Role: "user"}}}
	rs.Observe("t", s, 200, nil)
	rs.Observe("t", s, 200, nil)
	rs.Observe("t", &core.Request{Model: "m"}, 200, nil)
	rs.Observe("t", s, 200, nil)
	rs.Observe("t", nil, 200, nil)
	if r := rs.Report("t"); r.StreamsConsumed != 3 || r.ResentAfterStream != 1 || r.Requests != 5 {
		t.Fatalf("report %+v", r)
	}
}

// --- I18 ---------------------------------------------------------------

const i18Body = `{"model":"wwz-fast","reasoning_effort":"high","messages":[{"role":"user","content":"think"}]}`

// TestI18InlineTag: I18 — reasoning_field inline_tag puts <thinking>…</thinking>
// at the start of content, with no separate reasoning field.
func TestI18InlineTag(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := core.NewMemWriter()
	callWith(t, e, w, "opencode", map[string]string{"reasoning_field": "inline_tag"}, "/zen/go/v1/chat/completions", i18Body)
	var b reasoningBody
	if err := json.Unmarshal(w.Body(), &b); err != nil || len(b.Choices) == 0 {
		t.Fatalf("%v %s", err, w.Body())
	}
	msg := b.Choices[0].Message
	content, _ := msg["content"].(string)
	if !strings.HasPrefix(content, "<thinking>") || !strings.Contains(content, "</thinking>") || hasReasoningField(msg) {
		t.Fatalf("message %v", msg)
	}
	if b.Usage == nil || b.Usage.CompletionTokensDetails.ReasoningTokens <= 0 {
		t.Fatalf("usage %+v", b.Usage)
	}
}

// TestI18InlineTagStream: I18 — the stream opens the tag on the first
// reasoning delta and closes it before the answer.
func TestI18InlineTagStream(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := core.NewMemWriter()
	body := strings.Replace(i18Body, `"messages"`, `"stream":true,"messages"`, 1)
	callWith(t, e, w, "opencode", map[string]string{"reasoning_field": "inline_tag"}, "/zen/go/v1/chat/completions", body)
	var text strings.Builder
	for _, c := range streamChunks(t, w.Body()) {
		for _, ch := range c.Choices {
			if hasReasoningField(ch.Delta) {
				t.Fatalf("separate reasoning field in inline mode: %v", ch.Delta)
			}
			if s, ok := ch.Delta["content"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	got := text.String()
	if !strings.HasPrefix(got, "<thinking>") || strings.Count(got, "</thinking>") != 1 || strings.HasSuffix(got, "</thinking>") {
		t.Fatalf("inline stream content %q", got)
	}
}

// TestI18AnthropicSignature: I18 — the Anthropic thinking block carries a
// signature (non-stream) and a signature_delta (stream).
func TestI18AnthropicSignature(t *testing.T) {
	e := newEngine(t, fastCfg)
	h := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	body := `{"model":"wwz-fast","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
	w := call(t, e, "anthropic", "/v1/messages", h, body)
	var m struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(w.Body(), &m); err != nil || len(m.Content) == 0 {
		t.Fatalf("%v %s", err, w.Body())
	}
	if m.Content[0]["type"] != "thinking" || m.Content[0]["signature"] == "" || m.Content[0]["signature"] == nil {
		t.Fatalf("thinking block %v", m.Content[0])
	}
	w = call(t, e, "anthropic", "/v1/messages", h, strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1))
	if !strings.Contains(string(w.Body()), `"signature_delta"`) {
		t.Fatalf("no signature_delta:\n%s", w.Body())
	}
}

// --- I19 (engine side) --------------------------------------------------

func i19Engine(t *testing.T, errors string) *engine.Engine {
	t.Helper()
	return newEngine(t, `
server: {seed: 19}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: m-f, latency: fast, errors: `+errors+`}
`)
}

// TestI19TransportTypesReachWriter: I19 — every named transport fault in
// errors.types is handed to the transport and no envelope is written.
func TestI19TransportTypesReachWriter(t *testing.T) {
	for _, kind := range []string{"empty_response", "malformed_chunk", "random_data_then_close", "connection_reset", "corrupt_body"} {
		t.Run(kind, func(t *testing.T) {
			e := i19Engine(t, "{rate: 100, types: ["+kind+"]}")
			w := call(t, e, "openai", "/v1/chat/completions", impAuth, impBody("m-f", ""))
			f := w.Faults()
			if len(f) != 1 || string(f[0]) != kind || len(w.Body()) != 0 {
				t.Fatalf("faults %v body %q", f, w.Body())
			}
		})
	}
}

// TestI19DuplicateFrames: I19 — duplicate_frames repeats the last SSE frame.
func TestI19DuplicateFrames(t *testing.T) {
	e := i19Engine(t, "{mid_stream_rate: 100, mid_stream_types: [duplicate_frames], after_chunks: 2}")
	w := call(t, e, "openai", "/v1/chat/completions", impAuth, impBody("m-f", `,"stream":true`))
	lines := []string{}
	for _, l := range strings.Split(string(w.Body()), "\n") {
		if strings.HasPrefix(l, "data: {") {
			lines = append(lines, l)
		}
	}
	dup := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == lines[i-1] {
			dup++
		}
	}
	if dup != 1 || !strings.Contains(string(w.Body()), "data: [DONE]") {
		t.Fatalf("want one duplicated frame and a complete stream, got %d:\n%s", dup, w.Body())
	}
}

// TestI19DuplicateFramesInErrorTypes: I19 — the spec lists duplicate_frames
// among the new errors.types; naming it there must not degrade into a
// generic 500.
func TestI19DuplicateFramesInErrorTypes(t *testing.T) {
	e := i19Engine(t, "{rate: 100, types: [duplicate_frames], after_chunks: 2}")
	w := call(t, e, "openai", "/v1/chat/completions", impAuth, impBody("m-f", `,"stream":true`))
	if w.StatusCode() != 200 {
		t.Fatalf("errors.types: [duplicate_frames] answered %d: %s", w.StatusCode(), w.Body())
	}
	lines := []string{}
	for _, l := range strings.Split(string(w.Body()), "\n") {
		if strings.HasPrefix(l, "data: {") {
			lines = append(lines, l)
		}
	}
	dup := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == lines[i-1] {
			dup++
		}
	}
	if dup != 1 || !strings.Contains(string(w.Body()), "data: [DONE]") {
		t.Fatalf("want one duplicated frame and a complete stream, got %d:\n%s", dup, w.Body())
	}
	// Non-streaming: the reply is left intact.
	w = call(t, e, "openai", "/v1/chat/completions", impAuth, impBody("m-f", ""))
	if w.StatusCode() != 200 || !json.Valid(w.Body()) {
		t.Fatalf("non-stream: %d %s", w.StatusCode(), w.Body())
	}
}

// TestI19MalformedChunkAnthropic: I19 — the malformed frame follows the
// dialect of the driver (Anthropic event + data), not OpenAI's chunk.
func TestI19MalformedChunkAnthropic(t *testing.T) {
	e := i19Engine(t, "{mid_stream_rate: 100, mid_stream_types: [stream_malformed], after_chunks: 1}")
	h := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	for k, v := range impAuth {
		h[k] = v
	}
	w := call(t, e, "anthropic", "/v1/messages", h, `{"model":"m-f","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := string(w.Body())
	if strings.Contains(body, "chatcmpl") {
		t.Fatalf("OpenAI frame in an Anthropic stream:\n%s", body)
	}
	bad := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "data: {") && !json.Valid([]byte(l[6:])) {
			bad++
			if !strings.Contains(l, "content_block_delta") {
				t.Fatalf("malformed frame not in Anthropic shape: %q", l)
			}
		}
	}
	if bad == 0 {
		t.Fatalf("no malformed frame:\n%s", body)
	}
}

// TestI19MalformedChunkMidStream: I19 — stream_malformed writes one frame
// that is not valid JSON, then the stream goes on.
func TestI19MalformedChunkMidStream(t *testing.T) {
	e := i19Engine(t, "{mid_stream_rate: 100, mid_stream_types: [stream_malformed], after_chunks: 1}")
	w := call(t, e, "openai", "/v1/chat/completions", impAuth, impBody("m-f", `,"stream":true`))
	bad := 0
	for _, l := range strings.Split(string(w.Body()), "\n") {
		if strings.HasPrefix(l, "data: {") && !json.Valid([]byte(l[6:])) {
			bad++
		}
	}
	if bad == 0 {
		t.Fatalf("no malformed frame:\n%s", w.Body())
	}
}

// --- I22 ---------------------------------------------------------------

func i22Engine(t *testing.T, max int, itl string) *engine.Engine {
	t.Helper()
	return newEngine(t, `
server: {seed: 22, max_chunks_per_stream: `+itoa(max)+`}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: m-c, errors: none, latency: {ttft: 0ms, itl: `+itl+`}}
`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestI22MaxChunksText: I22 — server.max_chunks_per_stream caps the text
// frames by merging chunks, without losing content.
func TestI22MaxChunksText(t *testing.T) {
	e := i22Engine(t, 5, "0ms")
	words := strings.TrimSpace(strings.Repeat("word ", 40))
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": words}, impBody("m-c", `,"stream":true`))
	if strings.Join(s.texts, "") != words || !s.done {
		t.Fatalf("content changed: %q", strings.Join(s.texts, ""))
	}
	if len(s.texts) > 5 {
		t.Fatalf("%d text frames, cap 5", len(s.texts))
	}
}

// TestI22MaxChunksToolArgs: I22 — the cap also holds when the frames are
// tool-argument deltas.
func TestI22MaxChunksToolArgs(t *testing.T) {
	e := i22Engine(t, 5, "0ms")
	long := strings.Repeat("x", 600)
	hdr := `[{"name":"write_file","arguments":{"path":"/a","content":"` + long + `"}}]`
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-tool-calls": hdr}, impBody("m-c", ","+impTools+`,"stream":true`))
	if s.argFrames > 5 || s.argFrames == 0 || !s.done {
		t.Fatalf("%d tool-argument frames, cap 5 (done %v)", s.argFrames, s.done)
	}
	if strings.Count(s.raw, "x") < len(long) {
		t.Fatalf("arguments lost:\n%s", s.raw)
	}
}

// TestI22StreamDropFinalises: I22 — when the client drops mid-stream the
// request is finalised: gauges return to zero and the store gets the event.
func TestI22StreamDropFinalises(t *testing.T) {
	e := i22Engine(t, 0, "15ms")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newStampWriter()
	w.onData = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	words := strings.TrimSpace(strings.Repeat("word ", 30))
	impHandle(ctx, t, e, w, map[string]string{"x-wwz-response": words, "x-test-id": "t-drop"}, impBody("m-c", `,"stream":true`))
	if strings.Contains(string(w.Body()), "[DONE]") {
		t.Fatal("stream finished despite the client leaving")
	}
	if v := e.M.Streams.Value("openai"); v != 0 {
		t.Fatalf("streams gauge %d after drop", v)
	}
	if v := e.M.InFlight.Value("openai"); v != 0 {
		t.Fatalf("in-flight gauge %d after drop", v)
	}
	evs, err := e.Store.Events(context.Background(), 1)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	if evs[0].TestID != "t-drop" || evs[0].Error != "client_gone" {
		t.Fatalf("finish event %+v", evs[0])
	}
}
