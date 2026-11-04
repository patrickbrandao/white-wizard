package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I5: pacing explícito (latency.pacing: tokens_per_second,
// inter_chunk_delays, total_duration, chunk_runes, subword_streaming,
// dribble). Real sleeps are kept to a few tens of milliseconds; the unit
// side lives in internal/latency/imported_i5_i20_test.go.

func i5Engine(t *testing.T, pacing string) *engine.Engine {
	t.Helper()
	return newEngine(t, `
server: {seed: 5}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - id: m-pace
    errors: none
    latency: {ttft: 0ms, itl: 0ms, pacing: `+pacing+`}
`)
}

// TestI5ChunkRunes: pacing.chunk_runes splits the text in N-rune chunks.
func TestI5ChunkRunes(t *testing.T) {
	e := i5Engine(t, "{chunk_runes: 3}")
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": "abcdéfghij"}, impBody("m-pace", `,"stream":true`))
	want := []string{"abc", "déf", "ghi", "j"}
	if strings.Join(s.texts, "|") != strings.Join(want, "|") {
		t.Fatalf("chunks %q, want %q", s.texts, want)
	}
}

// TestI5TotalDuration: pacing.total_duration spreads the content over the
// given time (stream and non-stream).
func TestI5TotalDuration(t *testing.T) {
	e := i5Engine(t, "{total_duration: 40ms}")
	t0 := time.Now()
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": "one two three four"}, impBody("m-pace", `,"stream":true`))
	if d := time.Since(t0); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("stream took %v, want ≈ 40ms", d)
	}
	if !s.done {
		t.Fatal("stream not finished")
	}
	t0 = time.Now()
	impChatCall(t, e, map[string]string{"x-wwz-response": "one two three four"}, impBody("m-pace", ""))
	if d := time.Since(t0); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("non-stream took %v, want ≈ 40ms", d)
	}
}

// TestI5InterChunkDelays: the explicit list sets the gap before each chunk.
func TestI5InterChunkDelays(t *testing.T) {
	e := i5Engine(t, "{inter_chunk_delays: [0ms, 45ms, 0ms]}")
	w := newStampWriter()
	impHandle(context.Background(), t, e, w, map[string]string{"x-wwz-response": "a b c"}, impBody("m-pace", `,"stream":true`))
	s := impParseStream(t, w.Body())
	if len(s.texts) != 3 {
		t.Fatalf("texts %q", s.texts)
	}
	// Frames: role/start, "a", " b", " c", finish... The 45 ms gap sits
	// between the first and second text frames.
	var gaps []time.Duration
	for i := 1; i < len(w.stamps); i++ {
		gaps = append(gaps, w.stamps[i].Sub(w.stamps[i-1]))
	}
	big := 0
	for _, g := range gaps {
		if g >= 40*time.Millisecond {
			big++
		}
	}
	if big != 1 {
		t.Fatalf("want exactly one ≈45ms gap, got %v", gaps)
	}
}

// TestI5TokensPerSecond: tokens_per_second paces each token.
func TestI5TokensPerSecond(t *testing.T) {
	e := i5Engine(t, "{tokens_per_second: 100}")
	t0 := time.Now()
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": "a b c d e"}, impBody("m-pace", `,"stream":true`))
	if d := time.Since(t0); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("5 tokens at 100 tok/s took %v, want ≈ 50ms", d)
	}
	if len(s.texts) != 5 {
		t.Fatalf("texts %q", s.texts)
	}
	if rec := impLastRecord(t, e); rec.ITLMS != 10 {
		t.Fatalf("record itl %v ms, want 10", rec.ITLMS)
	}
}

// TestI5Dribble: I5 — dribble {chunks, total} delivers the content in that
// many chunks spread over total.
func TestI5Dribble(t *testing.T) {
	e := i5Engine(t, "{dribble_chunks: 2, dribble_total: 40ms}")
	t0 := time.Now()
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": "a b c d e f"}, impBody("m-pace", `,"stream":true`))
	d := time.Since(t0)
	if d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("dribble took %v, want ≥ 40ms", d)
	}
	if strings.Join(s.texts, "") != "a b c d e f" {
		t.Fatalf("content %q", s.texts)
	}
	if len(s.texts) != 2 || d > time.Second {
		t.Fatalf("dribble: %d chunks in %v, want 2 chunks over ≈ 40ms", len(s.texts), d)
	}
	// Non-stream holds dribble_total too.
	t0 = time.Now()
	impChatCall(t, e, map[string]string{"x-wwz-response": "a b c d e f"}, impBody("m-pace", ""))
	if d := time.Since(t0); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("non-stream dribble took %v, want ≥ 40ms", d)
	}
	// Fewer pieces than dribble_chunks: content is not split further.
	s, _ = impStreamCall(t, e, map[string]string{"x-wwz-response": "a"}, impBody("m-pace", `,"stream":true`))
	if len(s.texts) != 1 || s.texts[0] != "a" {
		t.Fatalf("short dribble %q", s.texts)
	}
}

// TestI5SubwordStreaming: I5 — subword_streaming streams sub-word pieces
// (more chunks than words).
func TestI5SubwordStreaming(t *testing.T) {
	const text = "internationalization extraordinarily"
	plain, _ := impStreamCall(t, i5Engine(t, "{}"), map[string]string{"x-wwz-response": text}, impBody("m-pace", `,"stream":true`))
	e := i5Engine(t, "{subword_streaming: true}")
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": text}, impBody("m-pace", `,"stream":true`))
	if len(s.texts) <= len(plain.texts) || strings.Join(s.texts, "") != text {
		t.Fatalf("subword_streaming: %q, want more pieces than %q with the same text", s.texts, plain.texts)
	}
	for _, c := range s.texts {
		if len(strings.TrimSpace(c)) > 3 {
			t.Fatalf("subword piece %q longer than 3 letters", c)
		}
	}
}
