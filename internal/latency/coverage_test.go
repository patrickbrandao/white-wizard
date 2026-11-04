package latency

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestSampleEdgeCases(t *testing.T) {
	r := generate.NewRNG(3, "edge")
	ms := time.Millisecond
	if got := Sample(nil, r); got != 0 {
		t.Fatalf("nil dist = %v", got)
	}
	// uniform with max < min collapses to min.
	if got := Sample(&config.Dist{Kind: "uniform", Min: 9 * ms, Max: 2 * ms}, r); got != 9*ms {
		t.Fatalf("uniform inverted = %v", got)
	}
	// lognormal without median falls back to the mean; sigma defaults when p99 is absent.
	if got := Sample(&config.Dist{Kind: "lognormal", Mean: 40 * ms}, r); got <= 0 {
		t.Fatalf("lognormal mean fallback = %v", got)
	}
	// p99 below median gives a non-positive sigma, clamped to a tiny spread.
	got := Sample(&config.Dist{Kind: "lognormal", Median: 50 * ms, P99: 10 * ms}, r)
	if got < 49*ms || got > 51*ms {
		t.Fatalf("lognormal clamped sigma = %v", got)
	}
	// pareto defaults: shape 2, scale from the mean.
	if got := Sample(&config.Dist{Kind: "pareto", Mean: 10 * ms}, r); got < 10*ms {
		t.Fatalf("pareto default = %v", got)
	}
	// unknown kind behaves as a fixed value.
	if got := Sample(&config.Dist{Kind: "bogus", Value: 7 * ms}, r); got != 7*ms {
		t.Fatalf("unknown kind = %v", got)
	}
	// negative values clamp to zero; max_value caps.
	if got := Sample(&config.Dist{Kind: "fixed", Value: -5 * ms}, r); got != 0 {
		t.Fatalf("negative = %v", got)
	}
	if got := Sample(&config.Dist{Kind: "fixed", Value: 50 * ms, MaxValue: 20 * ms}, r); got != 20*ms {
		t.Fatalf("max_value = %v", got)
	}
}

func TestBuildNilProfileAndLoadFactorSaturates(t *testing.T) {
	r := generate.NewRNG(1, "nil")
	plan := BuildCached(nil, r, 10, 0, 0, 1)
	if plan.ChunkTokens != 1 || plan.ChunkBytes != 24 || plan.Factor != 1 {
		t.Fatalf("defaults = %+v", plan)
	}
	p := &config.LatencyProfile{LoadFactor: 3, Concurrency: 10, TTFT: &config.Dist{Value: 100 * time.Millisecond}}
	plan = Build(p, r, 10, 1000, 1)
	if plan.Factor != 3 || plan.TTFT != 300*time.Millisecond {
		t.Fatalf("saturated factor = %v ttft=%v", plan.Factor, plan.TTFT)
	}
}

func TestWaitNonPositiveAndTotalBranches(t *testing.T) {
	p := &Plan{}
	if !p.Wait(0, nil) || !p.Wait(-time.Second, nil) {
		t.Fatal("non-positive wait must return true immediately")
	}
	ms := time.Millisecond
	p = &Plan{Queue: ms, TTFT: ms, FinishDelay: ms, TotalDuration: 10 * ms}
	if got := p.Total(5, 5, 5, 1); got != 13*ms {
		t.Fatalf("total with duration = %v", got)
	}
	p = &Plan{
		TTFT: ms, ITL: 2 * ms, ReasoningITL: 3 * ms, ToolCallDelay: 4 * ms, ToolArgsITL: 5 * ms,
		ChunkTokens: 2, ChunkBytes: 10,
	}
	// ttft 1 + reasoning 3*2 + content 2*3 + tools 4*2 + args 5*2
	if got := p.Total(3, 5, 15, 2); got != (1+6+6+8+10)*ms {
		t.Fatalf("total = %v", got)
	}
}
