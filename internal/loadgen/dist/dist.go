// Package dist is the controller/worker protocol of wwz-load --instances
// (spec 015, "Instâncias distribuídas"): HTTP/JSON between binaries of the
// same version, not a public contract. The controller waits for N workers,
// hands each one its slice of the load and a common start instant, collects
// their stats with the serialized histograms (ADR 0024) and merges them into
// one report: the merge is a sum of counters, so the aggregated percentiles
// are identical to those of a single histogram with the same samples.
package dist

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// Routes of the controller.
const (
	PathRegister = "/v1/register"
	PathResult   = "/v1/result"
	PathStatus   = "/v1/status"
)

// States of GET /v1/status.
const (
	StateWaiting    = "waiting"
	StateRunning    = "running"
	StateStopping   = "stopping"
	StateCollecting = "collecting"
	StateDone       = "done"
)

// Load is the wire form of loadgen.RunConfig (without the callbacks).
// Durations are in nanoseconds.
type Load struct {
	Mode              loadgen.Mode  `json:"mode"`
	RPS               float64       `json:"rps,omitempty"`
	Concurrency       int           `json:"concurrency,omitempty"`
	RampFrom          float64       `json:"ramp_from,omitempty"`
	RampTo            float64       `json:"ramp_to,omitempty"`
	RampDuration      time.Duration `json:"ramp_duration_ns,omitempty"`
	SweepRates        []float64     `json:"sweep_rates,omitempty"`
	StepDuration      time.Duration `json:"step_duration_ns,omitempty"`
	Duration          time.Duration `json:"duration_ns,omitempty"`
	Requests          int64         `json:"requests,omitempty"`
	Warmup            time.Duration `json:"warmup_ns,omitempty"`
	MaxInflight       int           `json:"max_inflight,omitempty"`
	SweepStopErrorPct float64       `json:"sweep_stop_error_pct,omitempty"`
}

// LoadOf is the wire form of c.
func LoadOf(c loadgen.RunConfig) Load {
	return Load{
		Mode: c.Mode, RPS: c.RPS, Concurrency: c.Concurrency, RampFrom: c.RampFrom, RampTo: c.RampTo,
		RampDuration: c.RampDuration, SweepRates: slices.Clone(c.SweepRates), StepDuration: c.StepDuration,
		Duration: c.Duration, Requests: c.Requests, Warmup: c.Warmup, MaxInflight: c.MaxInflight,
		SweepStopErrorPct: c.SweepStopErrorPct,
	}
}

// RunConfig is the run plan of l.
func (l Load) RunConfig() loadgen.RunConfig {
	return loadgen.RunConfig{
		Mode: l.Mode, RPS: l.RPS, Concurrency: l.Concurrency, RampFrom: l.RampFrom, RampTo: l.RampTo,
		RampDuration: l.RampDuration, SweepRates: slices.Clone(l.SweepRates), StepDuration: l.StepDuration,
		Duration: l.Duration, Requests: l.Requests, Warmup: l.Warmup, MaxInflight: l.MaxInflight,
		SweepStopErrorPct: l.SweepStopErrorPct,
	}
}

// Expected is the planned length of the run, warmup included; 0 when the
// plan has no time limit (--duration 0).
func (l Load) Expected() time.Duration {
	var d time.Duration
	switch l.Mode {
	case loadgen.ModeRamp:
		d = l.RampDuration
	case loadgen.ModeSweep:
		d = l.StepDuration * time.Duration(len(l.SweepRates))
	default:
		d = l.Duration
	}
	if d <= 0 {
		return 0
	}
	return l.Warmup + d
}

// Plan is what a worker runs: the load, the seeds, the request timeout and
// the request side, opaque to this package (the run flags of wwz-load).
type Plan struct {
	Load Load `json:"load"`
	// Seed ≠ 0 is the base of x-wwz-seed and SeedStep its stride between
	// local units (0 counts as 1): the worker k of N sends Seed + k + N·i
	// (ADR 0033). DrawSeed seeds the draws (prompt, tool results).
	Seed     int64           `json:"seed"`
	SeedStep int64           `json:"seed_step,omitempty"`
	DrawSeed int64           `json:"draw_seed"`
	Timeout  time.Duration   `json:"timeout_ns"`
	Request  json.RawMessage `json:"request,omitempty"`
}

// ErrBadPlan wraps the errors of Plan.Check.
var ErrBadPlan = errors.New("plan cannot be split")

// Check reports whether p splits into n non-empty slices: integer loads
// (concurrency, requests) must be at least n.
func (p Plan) Check(n int) error {
	switch {
	case n < 1:
		return fmt.Errorf("%w: instances must be ≥ 1, got %d", ErrBadPlan, n)
	case p.Load.Mode == loadgen.ModeConcurrency && p.Load.Concurrency < n:
		return fmt.Errorf("%w: concurrency %d below %d instances", ErrBadPlan, p.Load.Concurrency, n)
	case p.Load.Requests > 0 && p.Load.Requests < int64(n):
		return fmt.Errorf("%w: requests %d below %d instances", ErrBadPlan, p.Load.Requests, n)
	}
	return nil
}

// Split returns the slice of worker index (0-based) out of n (spec 015,
// "Plano"): rates divided equally (fractions allowed), concurrency and
// requests integer with the remainder to the lowest indices, x-wwz-seed
// base + index with stride n (interleaved, ADR 0033) and draw seed +
// index; durations unchanged.
func (p Plan) Split(n, index int) Plan {
	l := p.Load
	f := float64(n)
	l.RPS /= f
	l.RampFrom /= f
	l.RampTo /= f
	l.SweepRates = make([]float64, len(p.Load.SweepRates))
	for i, r := range p.Load.SweepRates {
		l.SweepRates[i] = r / f
	}
	l.Concurrency = int(share(int64(l.Concurrency), n, index))
	l.Requests = share(l.Requests, n, index)
	out := p
	out.Load = l
	if p.Seed != 0 {
		out.Seed = p.Seed + int64(index)
		out.SeedStep = int64(n)
	}
	out.DrawSeed = p.DrawSeed + int64(index)
	return out
}

// share is the part of total of index out of n, remainder to the lowest
// indices.
func share(total int64, n, index int) int64 {
	s := total / int64(n)
	if int64(index) < total%int64(n) {
		s++
	}
	return s
}

// Registration is the body of POST /v1/register.
type Registration struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

// Assignment is the response of POST /v1/register once the N workers are
// registered.
type Assignment struct {
	Index     int       `json:"index"`
	Instances int       `json:"instances"`
	StartAt   time.Time `json:"start_at"`
	Plan      Plan      `json:"plan"`
}

// Histograms are the mergeable measures of a worker: every step and the
// total with their serialized histograms, and the conversation stats in
// conversation mode.
type Histograms struct {
	Run           *loadgen.RunResult `json:"run"`
	Conversations *loadgen.ConvStats `json:"conversations,omitempty"`
}

// Result is the body of POST /v1/result. Error ≠ "" reports a worker that
// could not run (its measures are absent).
type Result struct {
	Index       int             `json:"index"`
	Report      json.RawMessage `json:"report,omitempty"`
	Histograms  *Histograms     `json:"histograms,omitempty"`
	Error       string          `json:"error,omitempty"`
	LateStartMs float64         `json:"late_start_ms"`
}

// valid reports a result whose measures can be merged without nil checks.
func (r *Result) valid() bool {
	if r.Error != "" {
		return true
	}
	h := r.Histograms
	if h == nil || h.Run == nil || !validStats(h.Run.Total.Stats) {
		return false
	}
	for _, st := range h.Run.Steps {
		if !validStats(st.Stats) {
			return false
		}
	}
	c := h.Conversations
	return c == nil || (c.E2E != nil && c.FailedByReason != nil)
}

func validStats(s *loadgen.Stats) bool {
	return s != nil && s.TTFT != nil && s.ITL != nil && s.E2E != nil && s.ScheduleLag != nil && s.ErrorsByType != nil
}

// Status is the response of GET /v1/status.
type Status struct {
	State      string `json:"state"`
	Registered int    `json:"registered"`
	Results    int    `json:"results"`
}

// Instances is the "instances" object of the aggregated report.
type Instances struct {
	Expected int            `json:"expected"`
	Reported int            `json:"reported"`
	Missing  []string       `json:"missing"`
	Workers  []WorkerReport `json:"workers"`
}

// WorkerReport is a worker that reported.
type WorkerReport struct {
	ID          string  `json:"id"`
	Hostname    string  `json:"hostname"`
	Requests    int64   `json:"requests"`
	LateStartMs float64 `json:"late_start_ms"`
}

// Merge aggregates the runs of the workers: counters summed, histograms
// merged step by step (same index), target rates and concurrency summed,
// the earliest start and offset, the latest finish and the longest
// duration; interrupted if any was.
func Merge(runs []*loadgen.RunResult) *loadgen.RunResult {
	out := &loadgen.RunResult{Total: loadgen.StepResult{Stats: loadgen.NewStats()}}
	steps := map[int]*loadgen.StepResult{}
	for i, r := range runs {
		if i == 0 || r.Started.Before(out.Started) {
			out.Started = r.Started
		}
		if r.Finished.After(out.Finished) {
			out.Finished = r.Finished
		}
		out.Interrupted = out.Interrupted || r.Interrupted
		mergeStep(&out.Total, r.Total, i == 0)
		for _, st := range r.Steps {
			m, ok := steps[st.Index]
			if !ok {
				m = &loadgen.StepResult{Index: st.Index, Stats: loadgen.NewStats()}
				steps[st.Index] = m
			}
			mergeStep(m, st, !ok)
		}
	}
	if len(runs) == 0 {
		out.Started = time.Now()
		out.Finished = out.Started
	}
	for _, m := range steps {
		out.Steps = append(out.Steps, *m)
	}
	slices.SortFunc(out.Steps, func(a, b loadgen.StepResult) int { return a.Index - b.Index })
	return out
}

func mergeStep(dst *loadgen.StepResult, src loadgen.StepResult, first bool) {
	dst.TargetRPS += src.TargetRPS
	dst.Concurrency += src.Concurrency
	if first || src.Start < dst.Start {
		dst.Start = src.Start
	}
	dst.Duration = max(dst.Duration, src.Duration)
	dst.Stats.Merge(src.Stats)
}
