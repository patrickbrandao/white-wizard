package latency

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// I5 (pacing explícito) and I20 (latência polinomial por carga,
// speedup_ratio) at the plan level (spec 017, spec 008).

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func fixed(d time.Duration) *config.Dist { return &config.Dist{Kind: "fixed", Value: d} }

// TestI5TokensPerSecondPlan: I5 — tokens_per_second sets ITL (content,
// reasoning and tool args) to 1/rate; jitter stays within ±jitter.
func TestI5TokensPerSecondPlan(t *testing.T) {
	r := generate.NewRNG(1, "i5")
	p := Build(&config.LatencyProfile{ITL: fixed(ms(99)), Pacing: &config.Pacing{TokensPerSecond: 50}}, r, 10, 0, 1)
	if p.ITL != ms(20) || p.ReasoningITL != ms(20) || p.ToolArgsITL != ms(20) {
		t.Fatalf("itl %v reasoning %v args %v, want 20ms", p.ITL, p.ReasoningITL, p.ToolArgsITL)
	}
	for i := 0; i < 200; i++ {
		p := Build(&config.LatencyProfile{Pacing: &config.Pacing{TokensPerSecond: 100, Jitter: 0.2}}, r, 10, 0, 1)
		if p.ITL < ms(8) || p.ITL > ms(12) {
			t.Fatalf("jittered itl %v outside 10ms ±20%%", p.ITL)
		}
	}
}

// TestI5InterChunkDelaysPlan: I5 — explicit delays per chunk; the last one
// repeats past the end of the list.
func TestI5InterChunkDelaysPlan(t *testing.T) {
	p := Build(&config.LatencyProfile{Pacing: &config.Pacing{InterChunkDelays: []time.Duration{ms(1), ms(2), ms(3)}}}, generate.NewRNG(1), 0, 0, 1)
	for i, want := range []time.Duration{ms(1), ms(2), ms(3), ms(3), ms(3)} {
		if got := p.ChunkDelay(i, 5, ms(50)); got != want {
			t.Fatalf("chunk %d: %v, want %v", i, got, want)
		}
	}
}

// TestI5TotalDurationPlan: I5 — total_duration is spread evenly and is the
// whole non-streaming hold time (plus queue/ttft/finish).
func TestI5TotalDurationPlan(t *testing.T) {
	p := Build(&config.LatencyProfile{TTFT: fixed(ms(5)), ITL: fixed(ms(50)), Pacing: &config.Pacing{TotalDuration: ms(100)}}, generate.NewRNG(1), 0, 0, 1)
	if got := p.ChunkDelay(0, 4, p.ITL); got != ms(25) {
		t.Fatalf("chunk delay %v, want 25ms", got)
	}
	if got := p.Total(0, 400, 0, 0); got != ms(105) {
		t.Fatalf("total %v, want 105ms regardless of token count", got)
	}
}

// TestI5ChunkRunesAndDribblePlan: I5 — chunk_runes is carried to the plan;
// dribble builds N equal delays summing to dribble_total.
func TestI5ChunkRunesAndDribblePlan(t *testing.T) {
	p := Build(&config.LatencyProfile{Pacing: &config.Pacing{ChunkRunes: 4, DribbleChunks: 4, DribbleTotal: ms(100)}}, generate.NewRNG(1), 0, 0, 1)
	if p.ChunkRunes != 4 {
		t.Fatalf("chunk runes %d", p.ChunkRunes)
	}
	var sum time.Duration
	for _, d := range p.InterChunkDelays {
		if d != ms(25) {
			t.Fatalf("dribble delay %v", d)
		}
		sum += d
	}
	if len(p.InterChunkDelays) != 4 || sum != ms(100) {
		t.Fatalf("dribble delays %v", p.InterChunkDelays)
	}
}

// TestI5NoPacingKeepsDistributions: I5 — pacing is an alternative; without
// it the ttft/itl distributions apply unchanged.
func TestI5NoPacingKeepsDistributions(t *testing.T) {
	p := Build(&config.LatencyProfile{TTFT: fixed(ms(7)), ITL: fixed(ms(3))}, generate.NewRNG(1), 0, 0, 1)
	if p.TTFT != ms(7) || p.ITL != ms(3) || p.InterChunkDelays != nil || p.TotalDuration != 0 || p.ChunkDelay(0, 3, p.ITL) != ms(3) {
		t.Fatalf("plan %+v", p)
	}
}

// TestI20PerToken: I20 — per_token: TTFT = prefill_overhead + tokens ×
// prefill_per_token, independent of load.
func TestI20PerToken(t *testing.T) {
	prof := &config.LatencyProfile{Model: "per_token", PrefillOverhead: fixed(ms(10)), PrefillPerToken: fixed(time.Millisecond)}
	r := generate.NewRNG(1)
	if p := Build(prof, r, 100, 0, 1); p.TTFT != ms(110) {
		t.Fatalf("ttft %v, want 110ms", p.TTFT)
	}
	if p := Build(prof, r, 100, 8, 1); p.TTFT != ms(110) {
		t.Fatalf("per_token must ignore load: ttft %v", p.TTFT)
	}
	// Without prefill keys it falls back to ttft.
	if p := Build(&config.LatencyProfile{Model: "per_token", TTFT: fixed(ms(9))}, r, 100, 0, 1); p.TTFT != ms(9) {
		t.Fatalf("fallback ttft %v", p.TTFT)
	}
}

// TestI20PolynomialPrefill: I20 — polynomial grows the prefill with the
// active requests.
func TestI20PolynomialPrefill(t *testing.T) {
	prof := &config.LatencyProfile{Model: "polynomial", PrefillOverhead: fixed(ms(10)), PrefillPerToken: fixed(time.Millisecond)}
	r := generate.NewRNG(1)
	idle := Build(prof, r, 100, 0, 1).TTFT
	busy := Build(prof, r, 100, 4, 1).TTFT
	if idle != ms(110) || busy <= idle {
		t.Fatalf("idle %v busy %v", idle, busy)
	}
	if busy2 := Build(prof, r, 100, 8, 1).TTFT; busy2 <= busy {
		t.Fatalf("ttft must keep growing with load: %v then %v", busy, busy2)
	}
}

// TestI20PolynomialDecode: I20 — decode (ITL) grows with active KV under
// the polynomial model.
func TestI20PolynomialDecode(t *testing.T) {
	prof := &config.LatencyProfile{Model: "polynomial", ITL: fixed(ms(10))}
	r := generate.NewRNG(1)
	idle := Build(prof, r, 100, 0, 1).ITL
	busy := Build(prof, r, 100, 8, 1).ITL
	if idle != ms(10) || busy <= idle {
		t.Fatalf("polynomial decode: itl %v idle, %v with 8 active", idle, busy)
	}
	p := Build(&config.LatencyProfile{Model: "polynomial", ITL: fixed(ms(10)), ReasoningITL: fixed(ms(20)), ToolArgsITL: fixed(ms(30))}, r, 0, 4, 1)
	if p.ITL != ms(12) || p.ReasoningITL != ms(24) || p.ToolArgsITL != ms(36) {
		t.Fatalf("polynomial decode with 4 active: %v %v %v", p.ITL, p.ReasoningITL, p.ToolArgsITL)
	}
	if p := Build(&config.LatencyProfile{Model: "per_token", ITL: fixed(ms(10))}, r, 0, 8, 1); p.ITL != ms(10) {
		t.Fatalf("per_token must not slow the decode: %v", p.ITL)
	}
}

// TestI20PrefillCachedTokens: spec 008 — prefill_per_token respects
// cached_tokens: cached prompt tokens are not prefilled.
func TestI20PrefillCachedTokens(t *testing.T) {
	prof := &config.LatencyProfile{Model: "per_token", PrefillOverhead: fixed(ms(10)), PrefillPerToken: fixed(time.Millisecond)}
	r := generate.NewRNG(1)
	p := BuildCached(prof, r, 100, 60, 0, 1)
	if p.TTFT != ms(50) || p.CachedTokens != 60 || !p.CacheHit() {
		t.Fatalf("ttft %v cached %d hit %v, want 50ms/60/true", p.TTFT, p.CachedTokens, p.CacheHit())
	}
	// Clamped to the prompt; negative is none.
	if p := BuildCached(prof, r, 100, 500, 0, 1); p.TTFT != ms(10) || p.CachedTokens != 100 {
		t.Fatalf("all cached: ttft %v cached %d", p.TTFT, p.CachedTokens)
	}
	if p := BuildCached(prof, r, 100, -3, 0, 1); p.TTFT != ms(110) || p.CachedTokens != 0 || p.CacheHit() {
		t.Fatalf("none cached: ttft %v cached %d", p.TTFT, p.CachedTokens)
	}
	// Polynomial applies the load term to the uncached prefill.
	poly := *prof
	poly.Model = "polynomial"
	if p := BuildCached(&poly, r, 100, 60, 2, 1); p.TTFT != ms(55) {
		t.Fatalf("polynomial cached ttft %v, want 50ms × 1.1", p.TTFT)
	}
	// The cache never shrinks the overhead (spec 008).
	if p := BuildCached(prof, r, 100, 100, 0, 1); p.TTFT != ms(10) {
		t.Fatalf("overhead kept: ttft %v", p.TTFT)
	}
}

// TestI5ShapeSubwordAndDribble: subword_streaming splits words into pieces
// of at most 3 letters; dribble regroups into exactly N chunks.
func TestI5ShapeSubwordAndDribble(t *testing.T) {
	sub := &Plan{Subword: true}
	got := sub.Shape([]string{"intern", "ationa", " extrao", ", ok", "!!"}, true)
	want := []string{"int", "ern", "ati", "ona", " ext", "rao", ", ok", "!!"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("subword %q, want %q", got, want)
	}
	if got := sub.Shape([]string{"intern"}, false); len(got) != 1 {
		t.Fatalf("subword off when not allowed: %q", got)
	}
	if got := (&Plan{Subword: true, ChunkRunes: 4}).Shape([]string{"intern"}, true); len(got) != 1 {
		t.Fatalf("chunk_runes wins over subword: %q", got)
	}
	dr := &Plan{DribbleChunks: 3}
	if got := dr.Shape([]string{"a", " b", " c", " d", " e", " f", " g"}, true); strings.Join(got, "|") != "a b| c d| e f g" {
		t.Fatalf("dribble %q", got)
	}
	if got := dr.Shape([]string{"a", " b"}, true); len(got) != 2 {
		t.Fatalf("dribble never splits: %q", got)
	}
	if got := (&Plan{}).Shape([]string{"a", " b"}, true); len(got) != 2 {
		t.Fatalf("no pacing: %q", got)
	}
	p := Build(&config.LatencyProfile{Pacing: &config.Pacing{SubwordStreaming: true, DribbleChunks: 2, DribbleTotal: ms(40)}}, generate.NewRNG(1), 0, 0, 1)
	if !p.Subword || p.DribbleChunks != 2 || p.TotalDuration != ms(40) || p.Total(0, 99, 0, 0) != ms(40) {
		t.Fatalf("plan %+v", p)
	}
}

// TestI20SpeedupRatio: I20 — speedup_ratio divides every stage and combines
// with the per-request speed.
func TestI20SpeedupRatio(t *testing.T) {
	prof := &config.LatencyProfile{Queue: fixed(ms(8)), TTFT: fixed(ms(40)), ITL: fixed(ms(4)), ToolCallDelay: fixed(ms(8)), FinishDelay: fixed(ms(8)), SpeedupRatio: 2}
	p := Build(prof, generate.NewRNG(1), 0, 0, 1)
	if p.Queue != ms(4) || p.TTFT != ms(20) || p.ITL != ms(2) || p.ReasoningITL != ms(2) || p.ToolArgsITL != ms(2) || p.ToolCallDelay != ms(4) || p.FinishDelay != ms(4) {
		t.Fatalf("plan %+v", p)
	}
	if p := Build(prof, generate.NewRNG(1), 0, 0, 2); p.TTFT != ms(10) {
		t.Fatalf("speedup 2 × speed 2: ttft %v", p.TTFT)
	}
	slow := *prof
	slow.SpeedupRatio = 0.5
	if p := Build(&slow, generate.NewRNG(1), 0, 0, 1); p.TTFT != ms(80) {
		t.Fatalf("speedup 0.5: ttft %v", p.TTFT)
	}
}
