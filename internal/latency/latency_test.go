package latency

import (
	"math"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestDistributions(t *testing.T) {
	r := generate.NewRNG(9, "d")
	cases := []struct {
		d      *config.Dist
		lo, hi time.Duration
	}{
		{&config.Dist{Kind: "fixed", Value: 10 * time.Millisecond}, 10 * time.Millisecond, 10 * time.Millisecond},
		{&config.Dist{Kind: "uniform", Min: 5 * time.Millisecond, Max: 15 * time.Millisecond}, 5 * time.Millisecond, 15 * time.Millisecond},
		{&config.Dist{Kind: "normal", Mean: 100 * time.Millisecond, StdDev: 20 * time.Millisecond}, 30 * time.Millisecond, 170 * time.Millisecond},
		{&config.Dist{Kind: "lognormal", Median: 50 * time.Millisecond, P99: 500 * time.Millisecond, MaxValue: 2 * time.Second}, 0, 2 * time.Second},
		{&config.Dist{Kind: "exponential", Mean: 20 * time.Millisecond}, 0, 10 * time.Second},
		{&config.Dist{Kind: "pareto", Scale: 10 * time.Millisecond, Shape: 3}, 10 * time.Millisecond, 10 * time.Second},
	}
	for _, c := range cases {
		for i := 0; i < 500; i++ {
			v := Sample(c.d, r)
			if v < c.lo || v > c.hi {
				t.Fatalf("%s: %v out of [%v,%v]", c.d.Kind, v, c.lo, c.hi)
			}
		}
	}
}

func TestLognormalMedian(t *testing.T) {
	r := generate.NewRNG(3, "ln")
	d := &config.Dist{Kind: "lognormal", Median: 100 * time.Millisecond, P99: 1000 * time.Millisecond}
	var vals []float64
	for i := 0; i < 4000; i++ {
		vals = append(vals, float64(Sample(d, r)))
	}
	// median should be near 100ms
	sum := 0.0
	n := 0
	for _, v := range vals {
		if v < float64(100*time.Millisecond) {
			n++
		}
		sum += v
	}
	frac := float64(n) / float64(len(vals))
	if math.Abs(frac-0.5) > 0.05 {
		t.Fatalf("median fraction %v", frac)
	}
}

func TestLoadFactorAndTotal(t *testing.T) {
	p := &config.LatencyProfile{TTFT: &config.Dist{Kind: "fixed", Value: 100 * time.Millisecond}, ITL: &config.Dist{Kind: "fixed", Value: 10 * time.Millisecond}, LoadFactor: 3, Concurrency: 10}
	r := generate.NewRNG(1, "lf")
	plan := Build(p, r, 100, 10, 0)
	if plan.TTFT != 300*time.Millisecond {
		t.Fatalf("load factor not applied: %v", plan.TTFT)
	}
	plan0 := Build(p, r, 100, 0, 0)
	if plan0.TTFT != 100*time.Millisecond {
		t.Fatalf("no load should be 100ms: %v", plan0.TTFT)
	}
	total := plan0.Total(0, 10, 0, 0)
	if total != 200*time.Millisecond {
		t.Fatalf("total: %v", total)
	}
}

func TestLogicalClock(t *testing.T) {
	p := &config.LatencyProfile{Clock: "logical", TTFT: &config.Dist{Kind: "fixed", Value: 5 * time.Second}}
	plan := Build(p, generate.NewRNG(1, "lc"), 1, 0, 0)
	start := time.Now()
	done := make(chan struct{})
	if !plan.Wait(plan.TTFT, done) {
		t.Fatal("wait cancelled")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("logical clock slept")
	}
	if plan.Elapsed != 5*time.Second {
		t.Fatalf("elapsed %v", plan.Elapsed)
	}
}

// TestCacheHitReducesTTFTProportionally: spec 008, ADR 0046 — with a
// sampled TTFT (constant, or per_token without prefill fields) a hit of c
// of n prompt tokens multiplies the TTFT by 1 - cache_ttft_reduction × c/n;
// the cached count is the caller's, never simulated.
func TestCacheHitReducesTTFTProportionally(t *testing.T) {
	ttft := &config.Dist{Kind: "fixed", Value: 100 * time.Millisecond}
	r := generate.NewRNG(1)
	for _, model := range []string{"", "constant", "per_token"} {
		prof := &config.LatencyProfile{Model: model, TTFT: ttft, CacheTTFTReduction: 0.5}
		p := BuildCached(prof, r, 100, 40, 0, 1)
		if p.TTFT != 80*time.Millisecond || p.CachedTokens != 40 || !p.CacheHit() {
			t.Fatalf("model %q: ttft %v cached %d hit %v, want 80ms/40/true", model, p.TTFT, p.CachedTokens, p.CacheHit())
		}
	}
	// Default reduction 0.7; a reduction above 1 is capped.
	if p := BuildCached(&config.LatencyProfile{TTFT: ttft}, r, 100, 100, 0, 1); p.TTFT != 30*time.Millisecond {
		t.Fatalf("default reduction: ttft %v, want 30ms", p.TTFT)
	}
	if p := BuildCached(&config.LatencyProfile{TTFT: ttft, CacheTTFTReduction: 3}, r, 100, 100, 0, 1); p.TTFT != 0 {
		t.Fatalf("capped reduction: ttft %v, want 0", p.TTFT)
	}
	// Without a hit nothing is cached and the TTFT is whole.
	if p := BuildCached(&config.LatencyProfile{TTFT: ttft}, r, 101, 0, 0, 1); p.CachedTokens != 0 || p.CacheHit() || p.TTFT != 100*time.Millisecond {
		t.Fatalf("no hit: cached %d hit %v ttft %v", p.CachedTokens, p.CacheHit(), p.TTFT)
	}
}
