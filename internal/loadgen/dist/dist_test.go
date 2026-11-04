package dist

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

func TestLoadRoundTripAndExpected(t *testing.T) {
	c := loadgen.RunConfig{
		Mode: loadgen.ModeSweep, SweepRates: []float64{10, 20}, StepDuration: time.Second, Warmup: time.Second,
		Requests: 9, MaxInflight: 5, SweepStopErrorPct: 50, Progress: func(loadgen.Progress) {},
	}
	l := LoadOf(c)
	b, _ := json.Marshal(l)
	var back Load
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, l) {
		t.Fatalf("%v %+v", err, back)
	}
	got := back.RunConfig()
	c.Progress = nil
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("%+v", got)
	}
	cases := []struct {
		l    Load
		want time.Duration
	}{
		{l, 3 * time.Second},
		{Load{Mode: loadgen.ModeRamp, RampDuration: time.Minute}, time.Minute},
		{Load{Mode: loadgen.ModeRPS, Duration: time.Second, Warmup: time.Second}, 2 * time.Second},
		{Load{Mode: loadgen.ModeConcurrency, Warmup: time.Second}, 0},
	}
	for _, tc := range cases {
		if e := tc.l.Expected(); e != tc.want {
			t.Fatalf("%+v: %v", tc.l, e)
		}
	}
}

func TestSplitAndCheck(t *testing.T) {
	p := Plan{
		Load: Load{
			Mode: loadgen.ModeConcurrency, Concurrency: 10, Requests: 11, RPS: 30, RampFrom: 3, RampTo: 9,
			SweepRates: []float64{3, 6}, Duration: time.Second, Warmup: time.Second,
		},
		Seed: 100, DrawSeed: 7, Timeout: time.Second, Request: json.RawMessage(`{"x":1}`),
	}
	var conc int
	var reqs int64
	for i := range 3 {
		s := p.Split(3, i)
		conc += s.Load.Concurrency
		reqs += s.Load.Requests
		if s.Load.RPS != 10 || s.Load.RampFrom != 1 || s.Load.RampTo != 3 || !reflect.DeepEqual(s.Load.SweepRates, []float64{1, 2}) ||
			s.Seed != 100+int64(i) || s.SeedStep != 3 || s.DrawSeed != 7+int64(i) || s.Load.Duration != time.Second || s.Timeout != time.Second ||
			string(s.Request) != `{"x":1}` {
			t.Fatalf("%d: %+v", i, s)
		}
	}
	if conc != 10 || reqs != 11 || p.Split(3, 0).Load.Concurrency != 4 || p.Split(3, 2).Load.Requests != 3 {
		t.Fatalf("shares %d %d", conc, reqs)
	}
	if p.Load.SweepRates[0] != 3 {
		t.Fatal("split changed the plan")
	}
	p.Seed = 0
	if s := p.Split(3, 2); s.Seed != 0 || s.SeedStep != 0 {
		t.Fatal(s.Seed, s.SeedStep)
	}
	if err := p.Check(3); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 11} {
		if err := p.Check(n); !errors.Is(err, ErrBadPlan) {
			t.Fatalf("%d: %v", n, err)
		}
	}
	p.Load.Mode = loadgen.ModeRPS
	if err := p.Check(11); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(12); !errors.Is(err, ErrBadPlan) {
		t.Fatal(err)
	}
}

// TestSplitSeedsInterleave (ADR 0033): the x-wwz-seed of every worker's
// local unit i are disjoint and their union is that of one machine.
func TestSplitSeedsInterleave(t *testing.T) {
	p := Plan{Load: Load{Mode: loadgen.ModeConcurrency, Concurrency: 3, Requests: 31}, Seed: 100}
	seen := map[int64]bool{}
	for k := range 3 {
		s := p.Split(3, k)
		for i := range s.Load.Requests {
			v := loadgen.SeedOf(s.Seed, s.SeedStep, i)
			if seen[v] {
				t.Fatalf("seed %d twice", v)
			}
			seen[v] = true
		}
	}
	for j := int64(100); j < 131; j++ {
		if !seen[j] {
			t.Fatalf("seed %d missing: %v", j, seen)
		}
	}
	if len(seen) != 31 {
		t.Fatal(len(seen))
	}
}

// sample is a synthetic request with the latencies drawn from r.
func sample(r *rand.Rand, ok bool) *loadgen.Result {
	if !ok {
		return &loadgen.Result{Err: "http_5xx"}
	}
	d := func() time.Duration { return time.Duration(r.ExpFloat64() * float64(20*time.Millisecond)) }
	return &loadgen.Result{
		Status: 200, TTFT: d(), E2E: d() + 50*time.Millisecond, ITL: []time.Duration{d(), d()},
		InputTokens: 3, OutputTokens: 5,
	}
}

// TestMergeExact is the acceptance of spec 015: three workers' histograms,
// serialized and merged, give the same counters and percentiles as one
// histogram with the same samples, step by step and in the total.
func TestMergeExact(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: test data
	single := &loadgen.RunResult{Total: loadgen.StepResult{Stats: loadgen.NewStats()}}
	for s := range 2 {
		single.Steps = append(single.Steps, loadgen.StepResult{Index: s + 1, Stats: loadgen.NewStats()})
	}
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	var runs []*loadgen.RunResult
	for w := range 3 {
		run := &loadgen.RunResult{
			Started: base.Add(time.Duration(w) * time.Millisecond), Finished: base.Add(time.Duration(10-w) * time.Second),
			Interrupted: w == 1, Total: loadgen.StepResult{TargetRPS: 10, Stats: loadgen.NewStats(), Duration: time.Duration(5+w) * time.Second},
		}
		for s := range 2 {
			if w == 2 && s == 1 {
				break // this worker's sweep stopped earlier
			}
			st := loadgen.StepResult{Index: s + 1, TargetRPS: 10, Concurrency: 1, Start: time.Duration(s) * time.Second, Duration: time.Second, Stats: loadgen.NewStats()}
			for i := range 500 + 100*w {
				res := sample(r, i%17 != 0)
				st.Stats.Add(res)
				single.Steps[s].Stats.Add(res)
			}
			st.Stats.Dropped = 2
			single.Steps[s].Stats.Dropped += 2
			run.Total.Stats.Merge(st.Stats)
			run.Steps = append(run.Steps, st)
		}
		// Through the wire, as a worker sends it.
		b, err := json.Marshal(&Result{Index: w, Histograms: &Histograms{Run: run}})
		if err != nil {
			t.Fatal(err)
		}
		var back Result
		if err := json.Unmarshal(b, &back); err != nil || !back.valid() {
			t.Fatalf("%v %s", err, b)
		}
		runs = append(runs, back.Histograms.Run)
	}
	for _, st := range single.Steps {
		single.Total.Stats.Merge(st.Stats)
	}
	m := Merge(runs)
	if !m.Started.Equal(base) || !m.Finished.Equal(base.Add(10*time.Second)) || !m.Interrupted || m.Total.Duration != 7*time.Second || m.Total.TargetRPS != 30 {
		t.Fatalf("meta %+v", m)
	}
	if len(m.Steps) != 2 || m.Steps[0].TargetRPS != 30 || m.Steps[1].TargetRPS != 20 || m.Steps[1].Concurrency != 2 || m.Steps[1].Start != time.Second {
		t.Fatalf("steps %+v", m.Steps)
	}
	same := func(name string, got, want *loadgen.Stats) {
		t.Helper()
		g, w := loadgen.Summarize(loadgen.StepResult{Stats: got}, 0), loadgen.Summarize(loadgen.StepResult{Stats: want}, 0)
		if !reflect.DeepEqual(g, w) || got.Requests == 0 {
			t.Fatalf("%s:\n got %+v\nwant %+v", name, g, w)
		}
	}
	same("total", m.Total.Stats, single.Total.Stats)
	same("step 1", m.Steps[0].Stats, single.Steps[0].Stats)
	same("step 2", m.Steps[1].Stats, single.Steps[1].Stats)

	empty := Merge(nil)
	if empty.Total.Stats.Requests != 0 || empty.Started.IsZero() || empty.Steps != nil {
		t.Fatalf("%+v", empty)
	}
}

func TestResultValid(t *testing.T) {
	good := func() *Result {
		return &Result{Histograms: &Histograms{Run: &loadgen.RunResult{
			Total: loadgen.StepResult{Stats: loadgen.NewStats()},
			Steps: []loadgen.StepResult{{Stats: loadgen.NewStats()}},
		}, Conversations: loadgen.NewConvStats()}}
	}
	if !good().valid() || !(&Result{Error: "x"}).valid() {
		t.Fatal("valid rejected")
	}
	bad := []func(*Result){
		func(r *Result) { r.Histograms = nil },
		func(r *Result) { r.Histograms.Run = nil },
		func(r *Result) { r.Histograms.Run.Total.Stats = nil },
		func(r *Result) { r.Histograms.Run.Total.Stats.TTFT = nil },
		func(r *Result) { r.Histograms.Run.Total.Stats.ErrorsByType = nil },
		func(r *Result) { r.Histograms.Run.Steps[0].Stats.ITL = nil },
		func(r *Result) { r.Histograms.Conversations.E2E = nil },
	}
	for i, f := range bad {
		r := good()
		f(r)
		if r.valid() {
			t.Fatalf("%d accepted", i)
		}
	}
}
