package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I6: timing por passo de cenário — latency, speed, stall, cut_after_tokens
// (spec 017 I6, spec 011).

const i6Cfg = `
server: {seed: 6}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
    lg: {ttft: 10ms, itl: 0ms, clock: logical}
models:
  - {id: m-log, latency: lg, errors: none}
  - {id: m-real, latency: fast, errors: none}
`

func i6Engine(t *testing.T, step string) *engine.Engine {
	t.Helper()
	e := newEngine(t, i6Cfg)
	impAddScenario(t, e, `
name: i6
steps:
  - id: s
    content: "alpha beta gamma delta epsilon zeta"
`+step)
	return e
}

// TestI6StepLatency: I6 — the step's latency overrides the model's TTFT.
func TestI6StepLatency(t *testing.T) {
	e := i6Engine(t, "    latency: {ttft: 40ms}\n")
	impChatCall(t, e, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-log", ""))
	if rec := impLastRecord(t, e); rec.TTFTMS != 40 || rec.Step != "s" {
		t.Fatalf("record %+v, want ttft 40 on step s", rec)
	}
}

// TestI6StepSpeed: I6 — speed divides every stage; the marker's speed= wins.
func TestI6StepSpeed(t *testing.T) {
	e := i6Engine(t, "    latency: {ttft: 40ms}\n    speed: 2\n")
	impChatCall(t, e, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-log", ""))
	if rec := impLastRecord(t, e); rec.TTFTMS != 20 {
		t.Fatalf("speed 2: ttft %d ms, want 20", rec.TTFTMS)
	}
	body := `{"model":"m-log","messages":[{"role":"user","content":"go [[sim:i6 speed=4]]"}]}`
	impChatCall(t, e, nil, body)
	if rec := impLastRecord(t, e); rec.TTFTMS != 10 {
		t.Fatalf("marker speed 4: ttft %d ms, want 10", rec.TTFTMS)
	}
}

// TestI6Stall: I6 — stall pauses the stream once after N tokens.
func TestI6Stall(t *testing.T) {
	e := i6Engine(t, "    stall: {after_tokens: 2, ms: 40}\n")
	w := newStampWriter()
	impHandle(context.Background(), t, e, w, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-real", `,"stream":true`))
	s := impParseStream(t, w.Body())
	if !s.done || strings.Join(s.texts, "") != "alpha beta gamma delta epsilon zeta" {
		t.Fatalf("stream incomplete: %q", s.texts)
	}
	big := 0
	for i := 1; i < len(w.stamps); i++ {
		if w.stamps[i].Sub(w.stamps[i-1]) >= 35*time.Millisecond {
			big++
		}
	}
	if big != 1 {
		t.Fatalf("want one stall gap, got %d", big)
	}
	// Non-streaming: the stall is added to the held time.
	t0 := time.Now()
	impChatCall(t, e, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-real", ""))
	if d := time.Since(t0); d < 40*time.Millisecond {
		t.Fatalf("non-stream with stall took %v", d)
	}
}

// TestI6StallDuration: I6 — stall.duration (Go duration) is accepted too.
func TestI6StallDuration(t *testing.T) {
	e := i6Engine(t, "    stall: {after_tokens: 1, duration: 30ms}\n")
	t0 := time.Now()
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-real", `,"stream":true`))
	if d := time.Since(t0); d < 30*time.Millisecond || !s.done {
		t.Fatalf("stall.duration: took %v done %v", d, s.done)
	}
}

// TestI6StallDurationNonStream: I6 — a stall given as duration also holds a
// non-streaming reply.
func TestI6StallDurationNonStream(t *testing.T) {
	e := i6Engine(t, "    stall: {after_tokens: 1, duration: 40ms}\n")
	t0 := time.Now()
	impChatCall(t, e, map[string]string{"x-wwz-scenario": "i6"}, impBody("m-real", ""))
	if d := time.Since(t0); d < 40*time.Millisecond {
		t.Fatalf("non-stream with stall.duration took %v", d)
	}
}

// TestI6CutAfterTokens: I6 — the stream ends abruptly after N tokens,
// without finish_reason nor [DONE], and the store sees the cut.
func TestI6CutAfterTokens(t *testing.T) {
	e := i6Engine(t, "    cut_after_tokens: 2\n")
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-scenario": "i6", "x-test-id": "t-cut"}, impBody("m-real", `,"stream":true`))
	if s.done || s.finish != "" {
		t.Fatalf("cut stream must not finish: %s", s.raw)
	}
	if len(s.texts) == 0 || len(s.texts) > 2 {
		t.Fatalf("want 1–2 text chunks before the cut, got %q", s.texts)
	}
	evs, err := e.Store.Events(context.Background(), 1)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	if evs[0].Error != "cut_after_tokens" || evs[0].TestID != "t-cut" {
		t.Fatalf("finish event %+v", evs[0])
	}
}
