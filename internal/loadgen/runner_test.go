package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// okUnit records a synthetic success after sleeping d.
func okUnit(d time.Duration) Unit {
	return func(_ context.Context, _ int64, rec *Recorder) {
		if d > 0 {
			time.Sleep(d)
		}
		rec.Record(&Result{TTFT: time.Millisecond, E2E: 2 * time.Millisecond, ITL: []time.Duration{time.Millisecond}, InputTokens: 3, OutputTokens: 5})
	}
}

func mustRun(t *testing.T, ctx context.Context, cfg RunConfig, u Unit) *RunResult {
	t.Helper()
	res, err := Run(ctx, cfg, u)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func within(got, want, tol float64) bool { return math.Abs(got-want) <= tol*want }

// TestRunRPS: the open loop starts exactly rate·duration units, and the
// latency of the unit does not reduce the rate (RNF-02).
func TestRunRPS(t *testing.T) {
	for _, sleep := range []time.Duration{0, 60 * time.Millisecond} {
		res := mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 500, Duration: 400 * time.Millisecond}, okUnit(sleep))
		s := res.Total.Stats
		if s.Requests != 200 || s.OK != 200 || s.ScheduleLag.Count() != 200 || s.Dropped != 0 {
			t.Fatalf("sleep %v: %d requests, %d ok, %d lags", sleep, s.Requests, s.OK, s.ScheduleLag.Count())
		}
		if res.Steps != nil || res.Total.TargetRPS != 500 || res.Interrupted {
			t.Fatalf("steps %v total %+v", res.Steps, res.Total)
		}
		if rps := float64(s.Requests) / res.Total.Duration.Seconds(); !within(rps, 500, 0.05) {
			t.Errorf("sleep %v: rps %v", sleep, rps)
		}
		if s.InputTokens != 600 || s.OutputTokens != 1000 || s.ITL.Count() != 200 {
			t.Errorf("tokens %d/%d itl %d", s.InputTokens, s.OutputTokens, s.ITL.Count())
		}
	}
}

func TestRunDropped(t *testing.T) {
	res := mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 200, Duration: 100 * time.Millisecond, MaxInflight: 2}, okUnit(300*time.Millisecond))
	s := res.Total.Stats
	if s.Requests != 2 || s.Dropped != 18 {
		t.Fatalf("%d requests, %d dropped", s.Requests, s.Dropped)
	}
}

func TestRunConcurrency(t *testing.T) {
	var cur, peak atomic.Int64
	u := func(ctx context.Context, i int64, rec *Recorder) {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		okUnit(10*time.Millisecond)(ctx, i, rec)
		cur.Add(-1)
	}
	res := mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 4, Duration: 200 * time.Millisecond}, u)
	s := res.Total.Stats
	if peak.Load() != 4 || res.Total.Concurrency != 4 || res.Steps != nil {
		t.Fatalf("peak %d total %+v", peak.Load(), res.Total)
	}
	if s.Requests < 40 || s.Requests > 84 || s.ScheduleLag.Count() != 0 {
		t.Fatalf("%d requests, %d lags", s.Requests, s.ScheduleLag.Count())
	}
	if !within(res.Total.Duration.Seconds(), 0.2, 0.05) {
		t.Errorf("duration %v", res.Total.Duration)
	}
}

// TestRunRamp: one step per interval, target = mean rate of the interval and
// the count of each step follows the area under the rate.
func TestRunRamp(t *testing.T) {
	cfg := RunConfig{Mode: ModeRamp, RampFrom: 0, RampTo: 400, RampDuration: time.Second, StepDuration: 250 * time.Millisecond}
	res := mustRun(t, bg, cfg, okUnit(0))
	if len(res.Steps) != 4 {
		t.Fatalf("%d steps", len(res.Steps))
	}
	var total int64
	for i, st := range res.Steps {
		want := []float64{50, 150, 250, 350}[i]
		if st.Index != i+1 || st.TargetRPS != want || st.Start != time.Duration(i)*cfg.StepDuration || st.Duration != cfg.StepDuration {
			t.Fatalf("step %d: %+v", i, st)
		}
		if n := float64(st.Stats.Requests); math.Abs(n-want/4) > 1 {
			t.Errorf("step %d: %v requests, want ~%v", i+1, n, want/4)
		}
		total += st.Stats.Requests
	}
	if total != 200 || res.Total.Stats.Requests != 200 {
		t.Fatalf("total %d / %d", total, res.Total.Stats.Requests)
	}
	// Last step shorter.
	cfg.RampDuration = 600 * time.Millisecond
	cfg.StepDuration = 400 * time.Millisecond
	res = mustRun(t, bg, cfg, okUnit(0))
	if len(res.Steps) != 2 || res.Steps[1].Duration != 200*time.Millisecond {
		t.Fatalf("steps %+v", res.Steps)
	}
}

func TestRunSweep(t *testing.T) {
	cfg := RunConfig{Mode: ModeSweep, SweepRates: []float64{100, 300}, StepDuration: 200 * time.Millisecond, Warmup: 50 * time.Millisecond}
	res := mustRun(t, bg, cfg, okUnit(0))
	if len(res.Steps) != 2 || res.Steps[0].Stats.Requests != 20 || res.Steps[1].Stats.Requests != 60 {
		t.Fatalf("steps %+v", res.Steps)
	}
	if res.Total.TargetRPS != 0 || res.Total.Stats.Requests != 80 || !within(res.Total.Duration.Seconds(), 0.4, 0.05) {
		t.Fatalf("total %+v", res.Total)
	}
}

func TestSweepStopOnErrors(t *testing.T) {
	fail := func(_ context.Context, _ int64, rec *Recorder) { rec.Record(&Result{Err: ErrTimeout}) }
	cfg := RunConfig{Mode: ModeSweep, SweepRates: []float64{100, 100, 100}, StepDuration: 100 * time.Millisecond, SweepStopErrorPct: 50}
	res := mustRun(t, bg, cfg, fail)
	if len(res.Steps) != 1 || res.Interrupted || res.Total.Stats.ErrorsByType[ErrTimeout] != 10 {
		t.Fatalf("steps %d, errors %v", len(res.Steps), res.Total.Stats.ErrorsByType)
	}
	// Without errors the sweep goes on.
	res = mustRun(t, bg, cfg, okUnit(0))
	if len(res.Steps) != 3 {
		t.Fatalf("steps %d", len(res.Steps))
	}
}

func TestRunRequestsLimit(t *testing.T) {
	res := mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 3, Requests: 10}, okUnit(time.Millisecond))
	if res.Total.Stats.Requests != 10 {
		t.Fatalf("concurrency: %d", res.Total.Stats.Requests)
	}
	res = mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 1000, Duration: time.Hour, Requests: 7}, okUnit(0))
	if res.Total.Stats.Requests != 7 || res.Interrupted {
		t.Fatalf("rps: %d", res.Total.Stats.Requests)
	}
	// No duration: the request limit alone ends the run.
	res = mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 1000, Requests: 5}, okUnit(0))
	if res.Total.Stats.Requests != 5 || res.Total.Duration <= 0 {
		t.Fatalf("unbounded: %+v", res.Total)
	}
}

func TestRunWarmup(t *testing.T) {
	var calls, warm atomic.Int64
	u := func(ctx context.Context, i int64, rec *Recorder) {
		calls.Add(1)
		if rec.Warmup() {
			warm.Add(1)
		}
		okUnit(0)(ctx, i, rec)
	}
	res := mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 100, Warmup: 200 * time.Millisecond, Duration: 200 * time.Millisecond}, u)
	if res.Total.Stats.Requests != 20 || calls.Load() != 40 || warm.Load() != 20 {
		t.Fatalf("rps: measured %d calls %d warm %d", res.Total.Stats.Requests, calls.Load(), warm.Load())
	}
	calls.Store(0)
	warm.Store(0)
	res = mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 1, Warmup: 50 * time.Millisecond, Duration: 50 * time.Millisecond}, func(ctx context.Context, i int64, rec *Recorder) {
		u(ctx, i, rec)
		time.Sleep(5 * time.Millisecond)
	})
	if warm.Load() == 0 || res.Total.Stats.Requests == 0 || res.Total.Stats.Requests+warm.Load() != calls.Load() {
		t.Fatalf("concurrency: measured %d calls %d warm %d", res.Total.Stats.Requests, calls.Load(), warm.Load())
	}
}

// TestRunInterrupt: canceling the context stops the starts, waits for the
// units in flight (with an uncanceled context) and returns a partial result.
func TestRunInterrupt(t *testing.T) {
	for _, cfg := range []RunConfig{
		{Mode: ModeRPS, RPS: 100, Duration: 10 * time.Second},
		{Mode: ModeConcurrency, Concurrency: 2, Duration: 10 * time.Second},
		{Mode: ModeSweep, SweepRates: []float64{100, 100}, StepDuration: 10 * time.Second},
	} {
		ctx, cancel := context.WithTimeout(bg, 150*time.Millisecond)
		var unitCtxErr atomic.Bool
		var sawDone atomic.Bool
		// The first units finish at once (something is measured); the next
		// one waits for the interruption, so it always observes Done()
		// regardless of the load on the machine.
		u := func(uctx context.Context, i int64, rec *Recorder) {
			if i >= 2 {
				select {
				case <-rec.Done():
					sawDone.Store(true)
				case <-time.After(10 * time.Second):
				}
			}
			if uctx.Err() != nil {
				unitCtxErr.Store(true)
			}
			okUnit(0)(uctx, i, rec)
		}
		res := mustRun(t, ctx, cfg, u)
		cancel()
		if !res.Interrupted || res.Total.Stats.Requests == 0 || unitCtxErr.Load() || !sawDone.Load() {
			t.Fatalf("%s: interrupted %v requests %d ctxErr %v done %v", cfg.Mode, res.Interrupted, res.Total.Stats.Requests, unitCtxErr.Load(), sawDone.Load())
		}
		if d := res.Total.Duration; d < 140*time.Millisecond || d > 5*time.Second {
			t.Errorf("%s: duration %v", cfg.Mode, d)
		}
		if cfg.Mode == ModeSweep && len(res.Steps) != 1 {
			t.Errorf("sweep: %d steps", len(res.Steps))
		}
	}
	// Interrupted during the warmup: nothing measured.
	ctx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	res := mustRun(t, ctx, RunConfig{Mode: ModeRPS, RPS: 100, Warmup: time.Second, Duration: time.Second}, okUnit(0))
	if !res.Interrupted || res.Total.Duration != 0 || res.Total.Stats.Requests != 0 {
		t.Fatalf("warmup interrupt: %+v", res.Total)
	}
}

func TestRunProgressAndUpdate(t *testing.T) {
	var mu sync.Mutex
	var snaps []Progress
	cfg := RunConfig{
		Mode: ModeRPS, RPS: 200, Duration: 200 * time.Millisecond, Warmup: 30 * time.Millisecond, ProgressEvery: 20 * time.Millisecond,
		Progress: func(p Progress) { mu.Lock(); snaps = append(snaps, p); mu.Unlock() },
	}
	var steps []int
	u := func(_ context.Context, _ int64, rec *Recorder) {
		rec.Update(func(s *Stats) { s.ErrorsByType["custom"]++ })
		mu.Lock()
		steps = append(steps, rec.Step())
		mu.Unlock()
	}
	res := mustRun(t, bg, cfg, u)
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) < 3 || !snaps[0].Warmup || snaps[len(snaps)-1].Started == 0 {
		t.Fatalf("progress %+v", snaps)
	}
	// Update counted only outside the warmup; lag flushed by Update.
	s := res.Total.Stats
	if s.ErrorsByType["custom"] != 40 || s.ScheduleLag.Count() != 40 || s.Requests != 0 || slices.Max(steps) != 0 {
		t.Fatalf("custom %d lags %d", s.ErrorsByType["custom"], s.ScheduleLag.Count())
	}
	// A unit that records nothing still has its schedule lag.
	res = mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 100, Duration: 100 * time.Millisecond}, func(context.Context, int64, *Recorder) {})
	if res.Total.Stats.ScheduleLag.Count() != 10 {
		t.Fatalf("lags %d", res.Total.Stats.ScheduleLag.Count())
	}
}

func TestRunConfigValidate(t *testing.T) {
	bad := []RunConfig{
		{},
		{Mode: "x"},
		{Mode: ModeRPS},
		{Mode: ModeRPS, RPS: math.Inf(1)},
		{Mode: ModeConcurrency},
		{Mode: ModeRamp, RampDuration: time.Second, StepDuration: time.Second},
		{Mode: ModeRamp, RampFrom: -1, RampTo: 3, RampDuration: time.Second, StepDuration: time.Second},
		{Mode: ModeRamp, RampTo: 3, StepDuration: time.Second},
		{Mode: ModeRamp, RampTo: 3, RampDuration: time.Second},
		{Mode: ModeSweep, StepDuration: time.Second},
		{Mode: ModeSweep, SweepRates: []float64{0}, StepDuration: time.Second},
		{Mode: ModeSweep, SweepRates: []float64{1}},
		{Mode: ModeRPS, RPS: 1, Requests: -1},
		{Mode: ModeRPS, RPS: 1, Warmup: -1},
		{Mode: ModeRPS, RPS: 1, MaxInflight: -1},
		{Mode: ModeRPS, RPS: 1, SweepStopErrorPct: -1},
	}
	for i, c := range bad {
		if _, err := Run(bg, c, okUnit(0)); !errors.Is(err, ErrBadRunConfig) {
			t.Errorf("%d: %v", i, err)
		}
	}
}

func TestGeometricRatesAndDraw(t *testing.T) {
	if g := GeometricRates(10, 1000, 3); !slices.Equal(g, []float64{10, 100, 1000}) {
		t.Fatal(g)
	}
	if g := GeometricRates(10, 1000, 8); len(g) != 8 || g[0] != 10 || g[7] != 1000 || g[1] != 19.307 {
		t.Fatal(g)
	}
	if g := GeometricRates(7, 9, 1); !slices.Equal(g, []float64{7}) {
		t.Fatal(g)
	}
	seen := map[int]bool{}
	for i := range int64(200) {
		k := Draw(42, i, 5)
		if k < 0 || k >= 5 || k != Draw(42, i, 5) {
			t.Fatal(k)
		}
		seen[k] = true
	}
	if len(seen) != 5 || Draw(1, 2, 1) != 0 {
		t.Fatalf("draws %v", seen)
	}
}

func TestStatsAddMerge(t *testing.T) {
	a, b := NewStats(), NewStats()
	if a.Add(&Result{Err: ErrCanceled}) || a.Requests != 0 {
		t.Fatal("canceled counted")
	}
	a.Add(&Result{TTFT: time.Millisecond, E2E: time.Millisecond, TokensEstimated: true, ReasoningTokens: 2})
	b.Add(&Result{Err: "http_429/rate_limit_error"})
	b.Dropped = 3
	a.Merge(b)
	if a.Requests != 2 || a.OK != 1 || a.Errors != 1 || a.Dropped != 3 || !a.TokensEstimated || a.ReasoningTokens != 2 || a.ErrorsByType["http_429/rate_limit_error"] != 1 {
		t.Fatalf("%+v", a)
	}
}

// TestRequestUnitAgainstWWZ: --rps and --concurrency against a wwz in
// process with zero latency, both protocols, with seed and prompts.
func TestRequestUnitAgainstWWZ(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	cl := NewClient(Options{Timeout: 5 * time.Second})
	defer cl.CloseIdleConnections()
	for _, p := range []Protocol{OpenAI, Anthropic} {
		u, err := NewRequestUnit(cl, RequestPlan{Template: *baseReq(base, p, "m-zero", true), Prompts: []string{"a", "b"}, Seed: 7, DrawSeed: 7})
		if err != nil {
			t.Fatal(err)
		}
		res := mustRun(t, bg, RunConfig{Mode: ModeRPS, RPS: 200, Duration: 500 * time.Millisecond, Warmup: 50 * time.Millisecond}, u)
		s := res.Total.Stats
		if s.Requests != 100 || s.OK != 100 || s.TTFT.Count() != 100 || s.ITL.Count() != 700 || s.OutputTokens != 800 || s.InputTokens == 0 {
			t.Fatalf("%s rps: %+v %v", p, s, s.ErrorsByType)
		}
		u, err = NewRequestUnit(cl, RequestPlan{Template: *baseReq(base, p, "m-trunc", false)})
		if err != nil {
			t.Fatal(err)
		}
		res = mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 2, Requests: 6}, u)
		if s := res.Total.Stats; s.Requests != 6 {
			t.Fatalf("%s concurrency: %+v", p, s)
		}
	}
	// Truncated streams are classified.
	u, _ := NewRequestUnit(cl, RequestPlan{Template: *baseReq(base, OpenAI, "m-trunc", true)})
	res := mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 1, Requests: 3}, u)
	if res.Total.Stats.ErrorsByType[ErrStreamTruncated] != 3 || res.Total.Stats.TTFT.Count() != 0 {
		t.Fatalf("%v", res.Total.Stats.ErrorsByType)
	}
}

// seedServer records the x-wwz-seed of every request and answers 500.
func seedServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seeds []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seeds = append(seeds, r.Header.Get("x-wwz-seed"))
		mu.Unlock()
		http.Error(w, "{}", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(seeds)
		slices.Sort(out)
		return out
	}
}

// TestRequestUnitSeedStep (ADR 0033): x-wwz-seed is Seed + SeedStep·i; a
// step ≤ 0 counts as 1.
func TestRequestUnitSeedStep(t *testing.T) {
	url, seeds := seedServer(t)
	cl := NewClient(Options{Timeout: 5 * time.Second})
	defer cl.CloseIdleConnections()
	u, err := NewRequestUnit(cl, RequestPlan{Template: Request{Protocol: OpenAI, URL: url, Model: "m"}, Seed: 101, SeedStep: 3})
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 1, Requests: 3}, u)
	if got := seeds(); !slices.Equal(got, []string{"101", "104", "107"}) {
		t.Fatal(got)
	}
	if SeedOf(5, 0, 2) != 7 || SeedOf(5, -1, 2) != 7 || SeedOf(5, 4, 2) != 13 {
		t.Fatal("SeedOf")
	}
}

func TestNewRequestUnitBadBody(t *testing.T) {
	tmpl := Request{Protocol: OpenAI, Tools: []Tool{{Name: "x", Schema: json.RawMessage("{")}}}
	if _, err := NewRequestUnit(nil, RequestPlan{Template: tmpl}); err == nil {
		t.Fatal("template")
	}
	if _, err := NewRequestUnit(nil, RequestPlan{Template: tmpl, Prompts: []string{"p"}}); err == nil {
		t.Fatal("prompts")
	}
}
