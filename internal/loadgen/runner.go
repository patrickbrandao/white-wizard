package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
)

// Mode is a load mode of spec 015 ("Modos").
type Mode string

// Load modes: exactly one per run.
const (
	// ModeRPS is a fixed rate in open loop.
	ModeRPS Mode = "rps"
	// ModeConcurrency is a fixed number of workers in closed loop.
	ModeConcurrency Mode = "concurrency"
	// ModeRamp is a linearly growing rate in open loop, reported per step.
	ModeRamp Mode = "ramp"
	// ModeSweep is a list of rates in open loop, one step per rate.
	ModeSweep Mode = "sweep"
)

// DefaultMaxInflight is the default --max-inflight.
const DefaultMaxInflight = 10000

// RunConfig is the load plan of one run.
type RunConfig struct {
	Mode Mode
	// RPS: rate of ModeRPS (> 0, fractions allowed).
	RPS float64
	// Concurrency: workers of ModeConcurrency (≥ 1).
	Concurrency int
	// RampFrom, RampTo, RampDuration: ModeRamp goes from RampFrom to RampTo
	// req/s over RampDuration.
	RampFrom, RampTo float64
	RampDuration     time.Duration
	// SweepRates: one step per rate (ModeSweep).
	SweepRates []float64
	// StepDuration: length of a ramp or sweep step (the last ramp step may
	// be shorter).
	StepDuration time.Duration
	// Duration of the measurement in ModeRPS/ModeConcurrency, without the
	// warmup. ≤ 0: no limit (Requests or the context end the run).
	Duration time.Duration
	// Requests > 0 ends the run once that many measured units started.
	Requests int64
	// Warmup precedes the first step with its load; its units are neither
	// measured nor counted.
	Warmup time.Duration
	// MaxInflight caps the units in flight in the open-loop modes; a start
	// above it is dropped and counted in Stats.Dropped. Default 10000.
	MaxInflight int
	// SweepStopErrorPct > 0 stops a sweep after a step whose error share
	// (among the requests completed when the step ends) is above it.
	SweepStopErrorPct float64
	// Progress, when set, is called every ProgressEvery (default 5s) from
	// its own goroutine.
	Progress      func(Progress)
	ProgressEvery time.Duration
}

// Progress is a live snapshot of a run, for the progress line.
type Progress struct {
	Elapsed   time.Duration
	Warmup    bool // still in the warmup
	Started   int64
	Completed int64 // measured requests recorded
	Errors    int64
	Dropped   int64
	Inflight  int64
}

// ErrBadRunConfig wraps every RunConfig validation error.
var ErrBadRunConfig = errors.New("invalid run config")

func badRun(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRunConfig, fmt.Sprintf(format, a...))
}

// Validate checks the plan of c's mode.
func (c *RunConfig) Validate() error {
	switch c.Mode {
	case ModeRPS:
		if !(c.RPS > 0) || math.IsInf(c.RPS, 0) {
			return badRun("rps must be > 0, got %v", c.RPS)
		}
	case ModeConcurrency:
		if c.Concurrency < 1 {
			return badRun("concurrency must be ≥ 1, got %d", c.Concurrency)
		}
	case ModeRamp:
		if !validRate(c.RampFrom) || !validRate(c.RampTo) || c.RampFrom+c.RampTo == 0 {
			return badRun("ramp rates must be ≥ 0 and not both 0, got %v:%v", c.RampFrom, c.RampTo)
		}
		if c.RampDuration <= 0 {
			return badRun("ramp duration must be > 0")
		}
		if c.StepDuration <= 0 {
			return badRun("step duration must be > 0")
		}
	case ModeSweep:
		if len(c.SweepRates) == 0 {
			return badRun("sweep needs at least one rate")
		}
		for _, r := range c.SweepRates {
			if !(r > 0) || math.IsInf(r, 0) {
				return badRun("sweep rate must be > 0, got %v", r)
			}
		}
		if c.StepDuration <= 0 {
			return badRun("step duration must be > 0")
		}
	default:
		return badRun("unknown mode %q", c.Mode)
	}
	switch {
	case c.Requests < 0:
		return badRun("requests must be ≥ 0")
	case c.Warmup < 0:
		return badRun("warmup must be ≥ 0")
	case c.MaxInflight < 0:
		return badRun("max inflight must be ≥ 1")
	case c.SweepStopErrorPct < 0:
		return badRun("sweep stop error pct must be ≥ 0")
	}
	return nil
}

func validRate(r float64) bool { return r >= 0 && !math.IsInf(r, 0) }

// GeometricRates returns k rates from `from` to `to` (inclusive) in geometric
// progression, rounded to 3 decimals: from·(to/from)^(i/(k−1)).
func GeometricRates(from, to float64, k int) []float64 {
	if k <= 1 {
		return []float64{round3(from)}
	}
	out := make([]float64, k)
	for i := range k {
		out[i] = round3(from * math.Pow(to/from, float64(i)/float64(k-1)))
	}
	return out
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Stats are the counters and histograms of a step (or of the whole run).
// They are mergeable: the merge of the per-shard stats is the same as one
// Stats that saw every request (ADR 0024).
type Stats struct {
	// Requests completed (success + error).
	Requests int64
	OK       int64
	Errors   int64
	// Dropped starts (MaxInflight reached); not requests.
	Dropped int64
	// Tokens of the successes only.
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	// TokensEstimated: some success had no usage.
	TokensEstimated bool
	// Latency of the successes; ScheduleLag of every measured open-loop
	// start (scheduled instant to the send).
	TTFT, ITL, E2E, ScheduleLag *hist.Hist
	// ErrorsByType: error class → count.
	ErrorsByType map[string]int64
}

// NewStats returns empty Stats.
func NewStats() *Stats {
	return &Stats{TTFT: hist.New(), ITL: hist.New(), E2E: hist.New(), ScheduleLag: hist.New(), ErrorsByType: map[string]int64{}}
}

// Add counts one completed request. A canceled request (ErrCanceled) is not
// a measure and is ignored; it returns false then.
func (s *Stats) Add(res *Result) bool {
	if res.Err == ErrCanceled {
		return false
	}
	s.Requests++
	if !res.OK() {
		s.Errors++
		s.ErrorsByType[res.Err]++
		return true
	}
	s.OK++
	s.InputTokens += int64(res.InputTokens)
	s.OutputTokens += int64(res.OutputTokens)
	s.ReasoningTokens += int64(res.ReasoningTokens)
	s.TokensEstimated = s.TokensEstimated || res.TokensEstimated
	s.TTFT.Record(res.TTFT)
	s.E2E.Record(res.E2E)
	for _, d := range res.ITL {
		s.ITL.Record(d)
	}
	return true
}

// Merge adds o into s. o is not changed.
func (s *Stats) Merge(o *Stats) {
	s.Requests += o.Requests
	s.OK += o.OK
	s.Errors += o.Errors
	s.Dropped += o.Dropped
	s.InputTokens += o.InputTokens
	s.OutputTokens += o.OutputTokens
	s.ReasoningTokens += o.ReasoningTokens
	s.TokensEstimated = s.TokensEstimated || o.TokensEstimated
	s.TTFT.Merge(o.TTFT)
	s.ITL.Merge(o.ITL)
	s.E2E.Merge(o.E2E)
	s.ScheduleLag.Merge(o.ScheduleLag)
	for k, v := range o.ErrorsByType {
		s.ErrorsByType[k] += v
	}
}

// StepResult is one step of a run (or the total).
type StepResult struct {
	// Index is 1-based (0 for the total).
	Index       int
	TargetRPS   float64
	Concurrency int
	// Start is the offset from the start of the measurement (after the
	// warmup); Duration the time the step actually ran.
	Start    time.Duration
	Duration time.Duration
	Stats    *Stats
}

// RunResult is the outcome of Run.
type RunResult struct {
	Started, Finished time.Time
	// Interrupted: the context ended before the plan did.
	Interrupted bool
	// Steps of ModeRamp and ModeSweep (empty for ModeRPS and
	// ModeConcurrency).
	Steps []StepResult
	Total StepResult
}

// Unit is what the runner starts: one request (NewRequestUnit) or, with a
// scenario, one whole conversation. i is the global index of the unit from
// 0 (warmup included). ctx is not canceled by the interruption of the run,
// so in-flight requests finish (bounded by the client timeout); rec.Done
// tells the unit to stop starting new requests.
type Unit func(ctx context.Context, i int64, rec *Recorder)

// Recorder is where a unit records its requests, bound to the step in which
// the unit started. It is valid only during the Unit call.
type Recorder struct {
	r      *runner
	sh     *shard
	step   int
	warm   bool
	lag    time.Duration
	hasLag bool
}

// Step is the 0-based index of the step the unit belongs to.
func (rec *Recorder) Step() int { return rec.step }

// Warmup reports a unit started in the warmup (its records are discarded).
func (rec *Recorder) Warmup() bool { return rec.warm }

// Done is closed when the run stops starting units (end of plan, request
// limit or interruption).
func (rec *Recorder) Done() <-chan struct{} { return rec.r.done }

// Record counts one completed request. Warmup records are discarded.
func (rec *Recorder) Record(res *Result) {
	if rec.warm {
		return
	}
	rec.sh.mu.Lock()
	s := rec.sh.steps[rec.step]
	counted := s.Add(res)
	rec.flushLag(s)
	rec.sh.mu.Unlock()
	if counted {
		rec.r.completed.Add(1)
		if !res.OK() {
			rec.r.errors.Add(1)
		}
	}
}

// Update runs f on the Stats of the unit's step under the shard lock, for
// measures beyond a single request (conversations, task 044). Skipped in
// the warmup.
func (rec *Recorder) Update(f func(*Stats)) {
	if rec.warm {
		return
	}
	rec.sh.mu.Lock()
	s := rec.sh.steps[rec.step]
	f(s)
	rec.flushLag(s)
	rec.sh.mu.Unlock()
}

func (rec *Recorder) flushLag(s *Stats) {
	if rec.hasLag {
		s.ScheduleLag.Record(rec.lag)
		rec.hasLag = false
	}
}

// shard holds the stats of a group of executors. A run has a few shards
// (≤ GOMAXPROCS, ≤ 16) instead of one histogram per goroutine: with 10 000
// goroutines in flight, per-goroutine histograms would cost gigabytes, and
// the shard lock is held for a few hundred nanoseconds, so it is almost
// never contended. Merged at the end (ADR 0024).
type shard struct {
	mu    sync.Mutex
	steps []*Stats
}

// plannedStep is a step of the plan.
type plannedStep struct {
	target      float64
	concurrency int
	start       time.Duration // offset from the measurement start
	dur         time.Duration // ≤ 0: unbounded
}

// segment is a stretch of the open-loop schedule with a linear rate.
type segment struct {
	step   int // index in steps
	warm   bool
	off    time.Duration // offset from the run start
	r0, r1 float64
	dur    time.Duration // math.MaxInt64: unbounded
}

// total is the number of starts in the segment (the area under the rate).
func (s *segment) total() float64 {
	if s.dur == math.MaxInt64 {
		return math.Inf(1)
	}
	return (s.r0 + s.r1) / 2 * s.dur.Seconds()
}

// at is the offset of the n-th start (n starts already happened):
// r0·t + a·t²/2 = n, solved in a numerically stable form.
func (s *segment) at(n float64) time.Duration {
	if n == 0 {
		return 0
	}
	a := 0.0
	if s.dur != math.MaxInt64 {
		a = (s.r1 - s.r0) / s.dur.Seconds()
	}
	t := 2 * n / (s.r0 + math.Sqrt(s.r0*s.r0+2*a*n))
	return time.Duration(t * float64(time.Second))
}

type job struct {
	at   time.Time
	i    int64
	step int
	warm bool
}

type runner struct {
	cfg    RunConfig
	unit   Unit
	uctx   context.Context
	steps  []plannedStep
	shards []*shard

	start     time.Time // run start (warmup included)
	measStart time.Time

	done     chan struct{}
	stopOnce sync.Once
	stopMu   sync.Mutex
	stopAt   time.Time
	interrup bool

	measured  atomic.Int64 // measured units started
	next      atomic.Int64 // global unit index
	completed atomic.Int64
	errors    atomic.Int64
	dropped   atomic.Int64
	inflight  atomic.Int64

	// open loop
	jobs  chan job
	nexec int
	wg    sync.WaitGroup
}

// Run executes the plan, calling unit for every start, and returns the
// merged measures. When ctx ends, Run stops starting units, waits for the
// ones in flight and returns a partial result with Interrupted set.
func Run(ctx context.Context, cfg RunConfig, unit Unit) (*RunResult, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxInflight == 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	if cfg.ProgressEvery <= 0 {
		cfg.ProgressEvery = 5 * time.Second
	}
	r := &runner{cfg: cfg, unit: unit, done: make(chan struct{}), uctx: context.WithoutCancel(ctx)}
	r.plan()
	executors := cfg.MaxInflight
	if cfg.Mode == ModeConcurrency {
		executors = cfg.Concurrency
	}
	r.shards = make([]*shard, max(1, min(runtime.GOMAXPROCS(0), 16, executors)))
	for i := range r.shards {
		sh := &shard{steps: make([]*Stats, len(r.steps))}
		for j := range sh.steps {
			sh.steps[j] = NewStats()
		}
		r.shards[i] = sh
	}

	r.start = time.Now()
	r.measStart = r.start.Add(cfg.Warmup)
	stopWatch := context.AfterFunc(ctx, func() { r.stop(time.Now(), true) })
	defer stopWatch()
	stopProgress := r.progress()
	if cfg.Mode == ModeConcurrency {
		r.closedLoop()
	} else {
		r.openLoop()
	}
	stopProgress()
	return r.result(), nil
}

// plan builds the steps of the mode.
func (r *runner) plan() {
	c := &r.cfg
	switch c.Mode {
	case ModeRPS:
		r.steps = []plannedStep{{target: c.RPS, dur: c.Duration}}
	case ModeConcurrency:
		r.steps = []plannedStep{{concurrency: c.Concurrency, dur: c.Duration}}
	case ModeRamp:
		for a := time.Duration(0); a < c.RampDuration; a += c.StepDuration {
			b := min(a+c.StepDuration, c.RampDuration)
			r.steps = append(r.steps, plannedStep{target: round3((r.rampRate(a) + r.rampRate(b)) / 2), start: a, dur: b - a})
		}
	case ModeSweep:
		for k, rate := range c.SweepRates {
			r.steps = append(r.steps, plannedStep{target: rate, start: time.Duration(k) * c.StepDuration, dur: c.StepDuration})
		}
	}
}

func (r *runner) rampRate(t time.Duration) float64 {
	c := &r.cfg
	return c.RampFrom + (c.RampTo-c.RampFrom)*t.Seconds()/c.RampDuration.Seconds()
}

// segments of the open-loop schedule: the warmup (with the load of the first
// step) then one per step.
func (r *runner) segments() []segment {
	var segs []segment
	first := r.steps[0]
	r0 := first.target
	if r.cfg.Mode == ModeRamp {
		r0 = r.cfg.RampFrom
	}
	if r.cfg.Warmup > 0 {
		segs = append(segs, segment{warm: true, r0: r0, r1: r0, dur: r.cfg.Warmup})
	}
	for i, st := range r.steps {
		s := segment{step: i, off: r.cfg.Warmup + st.start, r0: st.target, r1: st.target, dur: st.dur}
		if r.cfg.Mode == ModeRamp {
			s.r0, s.r1 = r.rampRate(st.start), r.rampRate(st.start+st.dur)
		}
		if s.dur <= 0 {
			s.dur = math.MaxInt64
		}
		segs = append(segs, s)
	}
	return segs
}

// stop ends the starts once; at is the instant the plan stopped.
func (r *runner) stop(at time.Time, interrupted bool) {
	r.stopOnce.Do(func() {
		r.stopMu.Lock()
		r.stopAt, r.interrup = at, interrupted
		r.stopMu.Unlock()
		close(r.done)
	})
}

func (r *runner) stopped() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// claim reserves a measured start; false when Requests is reached (and
// stops the run).
func (r *runner) claim() bool {
	if r.cfg.Requests > 0 && r.measured.Add(1) > r.cfg.Requests {
		r.stop(time.Now(), false)
		return false
	}
	return true
}

func (r *runner) openLoop() {
	r.jobs = make(chan job)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer func() {
		close(r.jobs)
		r.wg.Wait()
	}()
	need := 0.0
	segs := r.segments()
	for si := range segs {
		seg := &segs[si]
		segStart := r.start.Add(seg.off)
		total := seg.total()
		for k := 0; ; k++ {
			n := need + float64(k)
			if n >= total-1e-9 {
				need = n - total
				break
			}
			at := segStart.Add(seg.at(n))
			if !r.wait(timer, at) {
				return
			}
			if !seg.warm && !r.claim() {
				return
			}
			r.dispatch(job{at: at, step: seg.step, warm: seg.warm})
		}
		if !seg.warm && r.sweepShouldStop(seg.step) {
			r.stop(segStart.Add(seg.dur), false)
			return
		}
	}
	end := r.measStart
	if n := len(r.steps); n > 0 {
		last := r.steps[n-1]
		end = r.measStart.Add(last.start + last.dur)
	}
	r.stop(end, false)
}

// wait sleeps until at; false if the run stopped meanwhile.
func (r *runner) wait(timer *time.Timer, at time.Time) bool {
	d := time.Until(at)
	if d <= 0 {
		return !r.stopped()
	}
	timer.Reset(d)
	select {
	case <-timer.C:
		return !r.stopped()
	case <-r.done:
		timer.Stop()
		return false
	}
}

func (r *runner) dispatch(j job) {
	j.i = r.next.Add(1) - 1
	select {
	case r.jobs <- j:
		return
	default:
	}
	if r.nexec < r.cfg.MaxInflight {
		id := r.nexec
		r.nexec++
		r.wg.Add(1)
		go r.executor(id, j)
		return
	}
	r.next.Add(-1)
	if !j.warm {
		r.dropped.Add(1)
		sh := r.shards[0]
		sh.mu.Lock()
		sh.steps[j.step].Dropped++
		sh.mu.Unlock()
	}
}

// executor runs jobs until the channel closes; executors are spawned on
// demand up to MaxInflight and reused.
func (r *runner) executor(id int, j job) {
	defer r.wg.Done()
	rec := &Recorder{r: r, sh: r.shards[id%len(r.shards)]}
	for {
		rec.step, rec.warm = j.step, j.warm
		rec.lag, rec.hasLag = time.Since(j.at), !j.warm
		r.inflight.Add(1)
		r.unit(r.uctx, j.i, rec)
		r.inflight.Add(-1)
		rec.flushPendingLag()
		var ok bool
		if j, ok = <-r.jobs; !ok {
			return
		}
	}
}

// flushPendingLag records a pending lag of a unit that recorded nothing.
func (rec *Recorder) flushPendingLag() {
	if !rec.hasLag {
		return
	}
	rec.sh.mu.Lock()
	rec.flushLag(rec.sh.steps[rec.step])
	rec.sh.mu.Unlock()
}

// sweepShouldStop: a sweep step above SweepStopErrorPct (requests completed
// so far).
func (r *runner) sweepShouldStop(step int) bool {
	if r.cfg.Mode != ModeSweep || r.cfg.SweepStopErrorPct <= 0 || step == len(r.steps)-1 {
		return false
	}
	var req, errs int64
	for _, sh := range r.shards {
		sh.mu.Lock()
		req += sh.steps[step].Requests
		errs += sh.steps[step].Errors
		sh.mu.Unlock()
	}
	return req > 0 && float64(errs)*100/float64(req) > r.cfg.SweepStopErrorPct
}

func (r *runner) closedLoop() {
	var end time.Time
	if r.cfg.Duration > 0 {
		end = r.measStart.Add(r.cfg.Duration)
	}
	var wg sync.WaitGroup
	for w := range r.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := &Recorder{r: r, sh: r.shards[w%len(r.shards)]}
			for !r.stopped() {
				now := time.Now()
				warm := now.Before(r.measStart)
				if !warm && !end.IsZero() && !now.Before(end) {
					r.stop(end, false)
					return
				}
				if !warm && !r.claim() {
					return
				}
				rec.warm = warm
				r.inflight.Add(1)
				r.unit(r.uctx, r.next.Add(1)-1, rec)
				r.inflight.Add(-1)
			}
		}()
	}
	wg.Wait()
}

// progress starts the progress goroutine; the returned func stops it and
// waits for it, so no callback runs after Run returns.
func (r *runner) progress() func() {
	if r.cfg.Progress == nil {
		return func() {}
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(r.cfg.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				r.cfg.Progress(Progress{
					Elapsed: now.Sub(r.start), Warmup: now.Before(r.measStart),
					Started: r.next.Load(), Completed: r.completed.Load(), Errors: r.errors.Load(),
					Dropped: r.dropped.Load(), Inflight: r.inflight.Load(),
				})
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func (r *runner) result() *RunResult {
	r.stopMu.Lock()
	stopAt, interrupted := r.stopAt, r.interrup
	r.stopMu.Unlock()
	measured := max(stopAt.Sub(r.measStart), 0)
	res := &RunResult{Started: r.start, Finished: time.Now(), Interrupted: interrupted}
	total := StepResult{Stats: NewStats(), Duration: measured}
	for i, st := range r.steps {
		if i > 0 && st.start >= measured {
			break // never started (interrupted or stopped sweep)
		}
		dur := measured - st.start
		if st.dur > 0 {
			dur = min(dur, st.dur)
		}
		sr := StepResult{Index: i + 1, TargetRPS: st.target, Concurrency: st.concurrency, Start: st.start, Duration: max(dur, 0), Stats: NewStats()}
		for _, sh := range r.shards {
			sr.Stats.Merge(sh.steps[i])
		}
		total.Stats.Merge(sr.Stats)
		res.Steps = append(res.Steps, sr)
	}
	if r.cfg.Mode == ModeRPS || r.cfg.Mode == ModeConcurrency {
		total.TargetRPS, total.Concurrency = r.steps[0].target, r.steps[0].concurrency
		res.Steps = nil
	}
	res.Total = total
	return res
}

// RequestPlan describes the single-request unit of NewRequestUnit.
type RequestPlan struct {
	// Template is copied for every request (URL, key, model, stream, max
	// tokens, headers, tools).
	Template Request
	// Prompts: each request carries one user message drawn from them
	// (deterministic by DrawSeed and the unit index). Empty: the template
	// messages as they are.
	Prompts []string
	// Seed ≠ 0 sends x-wwz-seed: Seed + SeedStep·i.
	Seed int64
	// SeedStep is the stride of x-wwz-seed between units (≤ 0 counts as
	// 1): N in a distributed run of N workers, so their seeds interleave
	// (ADR 0033).
	SeedStep int64
	// DrawSeed seeds the local draws (the prompt).
	DrawSeed int64
}

// NewRequestUnit returns a Unit that sends one request per call through c
// and records it. The bodies are encoded once.
func NewRequestUnit(c *Client, p RequestPlan) (Unit, error) {
	var bodies [][]byte
	if len(p.Prompts) == 0 {
		b, err := p.Template.BuildBody()
		if err != nil {
			return nil, err
		}
		bodies = [][]byte{b}
	}
	for _, prompt := range p.Prompts {
		r := p.Template
		r.Messages = []Message{{Role: "user", Content: prompt}}
		b, err := r.BuildBody()
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, b)
	}
	pool := sync.Pool{New: func() any { return new(Result) }}
	return func(ctx context.Context, i int64, rec *Recorder) {
		req := p.Template
		req.Body = bodies[Draw(p.DrawSeed, i, len(bodies))]
		if p.Seed != 0 {
			req.Seed = SeedOf(p.Seed, p.SeedStep, i)
		}
		res := pool.Get().(*Result)
		c.DoInto(ctx, &req, res)
		rec.Record(res)
		pool.Put(res)
	}, nil
}

// SeedOf is the x-wwz-seed of unit i: seed + step·i, step ≤ 0 counting
// as 1 (ADR 0033).
func SeedOf(seed, step, i int64) int64 {
	return seed + max(step, 1)*i
}

// Draw returns a deterministic index in [0, n) for the i-th draw of seed
// (splitmix64), so a run with a fixed seed draws the same sequence.
func Draw(seed, i int64, n int) int {
	if n <= 1 {
		return 0
	}
	z := uint64(seed) + uint64(i)*0x9e3779b97f4a7c15 //nolint:gosec // G115: bit mixing
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % uint64(n)) //nolint:gosec // G115: n > 1, result < n
}
