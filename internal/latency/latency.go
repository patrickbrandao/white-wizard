// Package latency turns a LatencyProfile into concrete per-stage delays for
// one request, including the load factor and the logical clock mode.
package latency

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Sample draws one value from a distribution.
func Sample(d *config.Dist, r *generate.RNG) time.Duration {
	if d == nil {
		return 0
	}
	var v float64
	switch d.Kind {
	case "", "fixed":
		v = float64(d.Value)
	case "uniform":
		lo, hi := float64(d.Min), float64(d.Max)
		if hi < lo {
			hi = lo
		}
		v = lo + r.Float64()*(hi-lo)
	case "normal":
		sd := float64(d.StdDev)
		if sd > 0.3*float64(d.Mean) && d.Mean > 0 {
			sd = 0.3 * float64(d.Mean) // clamp like llm-d-inference-sim
		}
		v = r.NormFloat64()*sd + float64(d.Mean)
		if d.Mean > 0 {
			lo, hi := 0.3*float64(d.Mean), 1.7*float64(d.Mean)
			v = math.Max(lo, math.Min(hi, v))
		}
	case "lognormal":
		// Parametrised by median and p99: sigma = ln(p99/median)/2.326.
		med := float64(d.Median)
		if med <= 0 {
			med = float64(d.Mean)
		}
		sigma := 0.5
		if d.P99 > 0 && med > 0 {
			sigma = math.Log(float64(d.P99)/med) / 2.326
		}
		if sigma <= 0 {
			sigma = 0.01
		}
		v = med * math.Exp(r.NormFloat64()*sigma)
	case "exponential":
		v = r.ExpFloat64() * float64(d.Mean)
	case "pareto":
		shape := d.Shape
		if shape <= 0 {
			shape = 2
		}
		scale := float64(d.Scale)
		if scale <= 0 {
			scale = float64(d.Mean)
		}
		v = scale / math.Pow(1-r.Float64(), 1/shape)
	default:
		v = float64(d.Value)
	}
	if v < 0 {
		v = 0
	}
	if d.MaxValue > 0 && v > float64(d.MaxValue) {
		v = float64(d.MaxValue)
	}
	return time.Duration(v)
}

// Plan is the concrete schedule for one response.
type Plan struct {
	Queue         time.Duration
	TTFT          time.Duration
	ITL           time.Duration // per content chunk
	ReasoningITL  time.Duration
	ToolCallDelay time.Duration
	ToolArgsITL   time.Duration
	FinishDelay   time.Duration
	ChunkTokens   int
	ChunkBytes    int
	Factor        float64 // load factor applied
	Logical       bool    // do not sleep; account only
	// Explicit pacing overrides (nil when unused).
	InterChunkDelays []time.Duration
	TotalDuration    time.Duration
	ChunkRunes       int
	DribbleChunks    int  // content regrouped into exactly this many chunks (0 = off)
	Subword          bool // split words into sub-token pieces (subword_streaming)
	// CachedTokens is how many prompt tokens were read from the prompt
	// cache (given by the caller, spec 007); usage reports the same.
	CachedTokens int
	Elapsed      time.Duration // accumulated in logical mode
	cacheHit     bool
	// timer is reused by every Wait of the plan (spec 014: one timer per
	// stream, Reset per chunk). A plan waits from one goroutine at a time.
	timer *time.Timer
	// bound: BindCancel armed the timer to fire early when the request is
	// cancelled, so Wait parks on the timer alone (no select).
	bound bool
	// mark is the planned end of the previous Wait (zero before the first):
	// the next Wait ends at mark + d, not at now + d (task 378). The
	// scheduling delay of one chunk is not carried into the next, so the
	// streams of an instance do not drift into phase with each other.
	mark time.Time
}

// CacheHit reports whether the plan simulated a prompt cache hit.
func (p *Plan) CacheHit() bool { return p.cacheHit }

// Tracker counts in-flight requests per model for the load factor.
type Tracker struct {
	active atomic.Int64
}

func (t *Tracker) Enter() int64  { return t.active.Add(1) }
func (t *Tracker) Leave()        { t.active.Add(-1) }
func (t *Tracker) Active() int64 { return t.active.Load() }

// Build samples every stage of the profile for one request.
func Build(p *config.LatencyProfile, r *generate.RNG, promptTokens int, active int64, speed float64) *Plan {
	return BuildCached(p, r, promptTokens, 0, active, speed)
}

// polyFactor is the polynomial model's growth per active request, applied to
// both prefill (TTFT) and decode (ITL): cost grows with the active KV.
const polyFactor = 0.05

// BuildCached is Build with cachedTokens prompt tokens already in the prompt
// cache: under per_token and polynomial they are not prefilled (spec 008).
func BuildCached(p *config.LatencyProfile, r *generate.RNG, promptTokens, cachedTokens int, active int64, speed float64) *Plan {
	if p == nil {
		p = &config.LatencyProfile{}
	}
	plan := &Plan{ChunkTokens: p.ChunkTokens, ChunkBytes: p.ChunkBytes, Factor: 1, Logical: p.Clock == "logical"}
	if plan.ChunkTokens <= 0 {
		plan.ChunkTokens = 1
	}
	if plan.ChunkBytes <= 0 {
		plan.ChunkBytes = 24
	}
	cachedTokens = max(0, min(cachedTokens, promptTokens))
	plan.Queue = Sample(p.Queue, r)
	fromPrefill := false
	var overhead, perTok time.Duration
	switch p.Model {
	case "per_token", "polynomial":
		overhead, perTok = Sample(p.PrefillOverhead, r), Sample(p.PrefillPerToken, r)
		plan.TTFT = prefill(p, overhead, perTok, promptTokens-cachedTokens, active)
		fromPrefill = plan.TTFT != 0 || overhead != 0 || perTok != 0
		if !fromPrefill {
			plan.TTFT = Sample(p.TTFT, r)
		}
	default:
		plan.TTFT = Sample(p.TTFT, r)
	}
	plan.CachedTokens = cachedTokens
	plan.ITL = Sample(p.ITL, r)
	if p.ReasoningITL != nil {
		plan.ReasoningITL = Sample(p.ReasoningITL, r)
	} else {
		plan.ReasoningITL = plan.ITL
	}
	plan.ToolCallDelay = Sample(p.ToolCallDelay, r)
	if p.ToolArgsITL != nil {
		plan.ToolArgsITL = Sample(p.ToolArgsITL, r)
	} else {
		plan.ToolArgsITL = plan.ITL
	}
	plan.FinishDelay = Sample(p.FinishDelay, r)
	if p.Model == "polynomial" && active > 0 {
		// Decode slows down with the active KV, like the prefill.
		g := func(d time.Duration) time.Duration {
			return time.Duration(float64(d) * (1 + polyFactor*float64(active)))
		}
		plan.ITL, plan.ReasoningITL, plan.ToolArgsITL = g(plan.ITL), g(plan.ReasoningITL), g(plan.ToolArgsITL)
	}
	if cachedTokens > 0 {
		// A prompt-cache hit (spec 008, ADR 0046): the per-token prefill
		// above already skipped the cached tokens; a sampled TTFT shrinks
		// by cache_ttft_reduction in proportion to the cached share.
		plan.cacheHit = true
		if !fromPrefill { // cachedTokens <= promptTokens, so promptTokens > 0
			red := p.CacheTTFTReduction
			if red <= 0 {
				red = 0.7
			}
			red = min(red, 1)
			plan.TTFT = time.Duration(float64(plan.TTFT) * (1 - red*float64(cachedTokens)/float64(promptTokens)))
		}
	}
	// Load factor: 1 + (x-1) * active/concurrency.
	if p.LoadFactor > 1 && p.Concurrency > 0 {
		ratio := float64(active) / float64(p.Concurrency)
		if ratio > 1 {
			ratio = 1
		}
		plan.Factor = 1 + (p.LoadFactor-1)*ratio
	}
	if p.SpeedupRatio > 0 {
		plan.Factor /= p.SpeedupRatio
	}
	if speed > 0 {
		plan.Factor /= speed
	}
	if p.Pacing != nil {
		pc := p.Pacing
		if pc.TokensPerSecond > 0 {
			base := time.Duration(float64(time.Second) / pc.TokensPerSecond)
			j := 1.0
			if pc.Jitter > 0 {
				j = 1 + (r.Float64()*2-1)*pc.Jitter
			}
			plan.ITL = time.Duration(float64(base) * j)
			plan.ReasoningITL = plan.ITL
			plan.ToolArgsITL = plan.ITL
		}
		if len(pc.InterChunkDelays) > 0 {
			plan.InterChunkDelays = pc.InterChunkDelays
		}
		if pc.TotalDuration > 0 {
			plan.TotalDuration = pc.TotalDuration
		}
		if pc.ChunkRunes > 0 {
			plan.ChunkRunes = pc.ChunkRunes
		}
		plan.Subword = pc.SubwordStreaming
		if pc.DribbleChunks > 0 && pc.DribbleTotal > 0 {
			plan.DribbleChunks = pc.DribbleChunks
			plan.TotalDuration = pc.DribbleTotal
			plan.InterChunkDelays = make([]time.Duration, pc.DribbleChunks)
			for i := range plan.InterChunkDelays {
				plan.InterChunkDelays[i] = pc.DribbleTotal / time.Duration(pc.DribbleChunks)
			}
		}
	}
	plan.applyFactor()
	return plan
}

// prefill is overhead + tokens × perTok, grown by the polynomial load term.
func prefill(p *config.LatencyProfile, overhead, perTok time.Duration, tokens int, active int64) time.Duration {
	d := overhead + time.Duration(float64(perTok)*float64(tokens))
	if p.Model == "polynomial" && active > 0 {
		d += time.Duration(float64(d) * polyFactor * float64(active))
	}
	return d
}

func (p *Plan) applyFactor() {
	if p.Factor == 1 {
		return
	}
	f := func(d time.Duration) time.Duration { return time.Duration(float64(d) * p.Factor) }
	p.Queue, p.TTFT, p.ITL, p.ReasoningITL = f(p.Queue), f(p.TTFT), f(p.ITL), f(p.ReasoningITL)
	p.ToolCallDelay, p.ToolArgsITL, p.FinishDelay = f(p.ToolCallDelay), f(p.ToolArgsITL), f(p.FinishDelay)
}

// Wait sleeps d (or accounts it in logical mode). It returns false if ctx is
// done before the wait completes.
func (p *Plan) Wait(d time.Duration, done <-chan struct{}) bool {
	if d <= 0 {
		return true
	}
	if p.Logical {
		p.Elapsed += d
		return true
	}
	// The wait ends at mark + d (task 378): a wait that started late (the
	// goroutine was scheduled late, or the write took long) is shortened by
	// the same amount, so the lateness of one chunk does not shift the next.
	// Resetting the period after each emission (now + d) couples thousands of
	// periodic streams through the shared CPU and makes them synchronize in
	// bursts (Floyd and Jacobson, "The Synchronization of Periodic Routing
	// Messages", 1993): measured with 3 000 streams at 60 % CPU, the wwz sent
	// for ~14 ms and stayed silent for ~6 ms of every 20 ms, and the ITL p99
	// was 20 ms + ~9 ms. A wait already overdue (behind by more than d)
	// returns at once and restarts the schedule from now.
	now := time.Now()
	wait := d
	if !p.mark.IsZero() {
		wait = p.mark.Add(d).Sub(now)
		if wait <= 0 {
			p.mark = now
			return true
		}
	}
	p.mark = now.Add(wait)
	// Since Go 1.23 Reset and Stop discard a pending expiry: no drain.
	if p.timer == nil {
		p.timer = time.NewTimer(wait)
	} else {
		p.timer.Reset(wait)
	}
	if p.bound {
		// The cancellation fires the timer (BindCancel): park on the timer
		// alone, then tell an early expiry from the planned one by done.
		// A cancellation before the Reset above is caught by the first
		// check; one after it fires the timer.
		if isDone(done) {
			p.timer.Stop()
			p.mark = time.Time{}
			return false
		}
		<-p.timer.C
		if isDone(done) {
			p.mark = time.Time{}
			return false
		}
		return true
	}
	select {
	case <-p.timer.C:
		return true
	case <-done:
		p.timer.Stop()
		p.mark = time.Time{}
		return false
	}
}

func isDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// BindCancel makes every later Wait on ctx.Done() cheaper: the plan's timer
// is fired as soon as ctx is cancelled, so a wait parks on the timer alone
// instead of selecting on the timer and done (task 341: the select and the
// timer's channel send were ~9 % of the CPU of a stream). It must be called
// from the goroutine that waits, before the first Wait, and only for waits
// whose done is ctx.Done(); call stop when the waits are over. In logical
// mode it does nothing.
func (p *Plan) BindCancel(ctx context.Context) (stop func() bool) {
	if p.Logical || ctx.Done() == nil {
		return func() bool { return false }
	}
	if p.timer == nil {
		p.timer = time.NewTimer(time.Hour)
	}
	p.timer.Stop()
	p.bound = true
	t := p.timer
	stopFn := context.AfterFunc(ctx, func() { t.Reset(0) })
	return func() bool {
		p.bound = false
		return stopFn()
	}
}

// ChunkDelay returns the delay before chunk i (0-based) of a content stream,
// honouring explicit inter-chunk delays or a total duration spread over n chunks.
func (p *Plan) ChunkDelay(i, n int, base time.Duration) time.Duration {
	if len(p.InterChunkDelays) > 0 {
		if i < len(p.InterChunkDelays) {
			return p.InterChunkDelays[i]
		}
		return p.InterChunkDelays[len(p.InterChunkDelays)-1]
	}
	if p.TotalDuration > 0 && n > 0 {
		return p.TotalDuration / time.Duration(n)
	}
	return base
}

// Total estimates the whole non-streaming latency for a response of the
// given reasoning/content/tool-arg sizes (used to hold non-streaming replies).
func (p *Plan) Total(reasoningTokens, contentTokens, toolArgBytes int, toolCalls int) time.Duration {
	d := p.Queue + p.TTFT + p.FinishDelay
	if p.TotalDuration > 0 {
		return d + p.TotalDuration
	}
	if reasoningTokens > 0 {
		d += p.ReasoningITL * time.Duration((reasoningTokens+p.ChunkTokens-1)/p.ChunkTokens)
	}
	if contentTokens > 0 {
		d += p.ITL * time.Duration((contentTokens+p.ChunkTokens-1)/p.ChunkTokens)
	}
	if toolCalls > 0 {
		d += p.ToolCallDelay * time.Duration(toolCalls)
		d += p.ToolArgsITL * time.Duration((toolArgBytes+p.ChunkBytes-1)/p.ChunkBytes)
	}
	return d
}

// subwordRunes is the longest letter run of a subword_streaming piece.
const subwordRunes = 3

// Shape applies the content pacing that moves chunk boundaries:
// subword_streaming (when subword is true and chunk_runes is off) splits each
// chunk into pieces of at most 3 letters, with spaces and punctuation
// attached to the following piece; dribble then regroups the result into
// exactly DribbleChunks chunks. The concatenation is preserved.
func (p *Plan) Shape(chunks []string, subword bool) []string {
	if subword && p.Subword && p.ChunkRunes <= 0 {
		chunks = splitSubword(chunks)
	}
	n := p.DribbleChunks
	if n <= 0 || len(chunks) <= n {
		return chunks
	}
	out := make([]string, n)
	for i := range out {
		lo, hi := i*len(chunks)/n, (i+1)*len(chunks)/n
		out[i] = strings.Join(chunks[lo:hi], "")
	}
	return out
}

func splitSubword(chunks []string) []string {
	out := make([]string, 0, len(chunks)*2)
	for _, c := range chunks {
		start, letters := 0, 0
		for i, r := range c {
			word := unicode.IsLetter(r) || unicode.IsDigit(r)
			if i > start && ((word && letters == subwordRunes) || (!word && letters > 0)) {
				out = append(out, c[start:i])
				start, letters = i, 0
			}
			if word {
				letters++
			}
		}
		if start < len(c) {
			out = append(out, c[start:])
		}
	}
	return out
}
