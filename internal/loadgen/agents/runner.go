package agents

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// ConversationHeader identifies the session on the wwz (I11).
const ConversationHeader = "x-wwz-conversation-id"

// Result is the outcome of Run.
type Result struct {
	Started, Finished time.Time
	// Interrupted: the context ended before every agent finished.
	Interrupted bool
	RunID       string
	// Agents in index order; Total is their merge.
	Agents []*Stats
	Total  *Stats
}

// Elapsed is the wall time of the run.
func (r *Result) Elapsed() time.Duration { return r.Finished.Sub(r.Started) }

type runner struct {
	cfg    Config
	client *loadgen.Client
	ctx    context.Context // Done = interrupt (SIGINT/SIGTERM)
	rctx   context.Context // requests and tool calls: never canceled
	start  time.Time
	runID  string

	intr                           atomic.Bool
	started, ended, failed         atomic.Int64
	requests, errors, retriesCount atomic.Int64
}

// RunID returns the <run> of the conversation ids: 8 hex digits derived
// from seed when it is not 0 (the same seed gives the same ids, so the wwz
// and the tools answer the same), else random.
func RunID(seed int64) string {
	if seed != 0 {
		return fmt.Sprintf("%08x", loadgen.Draw(seed, 0x72756e, 1<<32))
	}
	return fmt.Sprintf("%08x", rand.Uint32()) //nolint:gosec // G404: an id, not a secret
}

// Run plays the agents until each one ends its sessions (or Duration
// elapses). ctx ending is the interruption: no new turn, retry or session
// starts, the requests in flight end on their own (bounded by the client
// timeout) and the open sessions count as interrupted.
func Run(ctx context.Context, c *loadgen.Client, cfg Config) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &runner{cfg: cfg, client: c, ctx: ctx, rctx: context.WithoutCancel(ctx), runID: cfg.RunID}
	if r.runID == "" {
		r.runID = RunID(cfg.Seed)
	}
	r.start = time.Now()
	res := &Result{Started: r.start, RunID: r.runID, Agents: make([]*Stats, cfg.Agents)}
	stopProgress := r.progress()
	var wg sync.WaitGroup
	for a := range cfg.Agents {
		st := NewStats(a, cfg.Profile.For(a))
		res.Agents[a] = st
		wg.Go(func() { r.agent(a, st) })
	}
	wg.Wait()
	stopProgress()
	res.Finished = time.Now()
	res.Interrupted = r.intr.Load()
	res.Total = NewStats(-1, cfg.Profile)
	for _, st := range res.Agents {
		res.Total.Merge(st)
	}
	return res, nil
}

func (r *runner) progress() func() {
	if r.cfg.Progress == nil || r.cfg.ProgressEvery <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(r.cfg.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.cfg.Progress(Progress{
					Elapsed: time.Since(r.start), SessionsStarted: r.started.Load(), SessionsEnded: r.ended.Load(),
					SessionsFailed: r.failed.Load(), Requests: r.requests.Load(), Errors: r.errors.Load(),
					Retries: r.retriesCount.Load(),
				})
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// interrupted reports (and remembers) the interruption.
func (r *runner) interrupted() bool {
	if r.ctx.Err() != nil {
		r.intr.Store(true)
		return true
	}
	return false
}

// wait sleeps d; false when interrupted first.
func (r *runner) wait(d time.Duration) bool {
	if r.interrupted() {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.ctx.Done():
		r.intr.Store(true)
		return false
	case <-t.C:
		return true
	}
}

// agent runs the sessions of agent a.
func (r *runner) agent(a int, st *Stats) {
	if !r.wait(time.Until(r.start.Add(time.Duration(a) * r.cfg.SpawnInterval))) {
		return
	}
	rng := rand.New(rand.NewPCG(uint64(r.cfg.DrawSeed), uint64(a))) //nolint:gosec // G115/G404: jitter, bit pattern
	for s := 0; ; s++ {
		if r.cfg.Duration > 0 {
			if time.Since(r.start) >= r.cfg.Duration {
				return
			}
		} else if s >= r.cfg.Sessions {
			return
		}
		if r.interrupted() {
			return
		}
		r.session(a, s, st, rng)
	}
}

// session plays session s (local index) of agent a.
func (r *runner) session(a, s int, st *Stats, rng *rand.Rand) {
	g := int64(a) + int64(r.cfg.Agents)*int64(s)
	id := fmt.Sprintf("%s-a%d-s%d", r.runID, a, s)
	var seed int64
	if r.cfg.Seed != 0 {
		seed = r.cfg.Seed + g
	}
	sc := scripts[st.Profile]
	req := r.cfg.Template
	req.Headers = append(slices.Clip(req.Headers), loadgen.Header{Key: ConversationHeader, Value: id})
	req.Tools, req.Seed, req.Capture, req.Body = declared[st.Profile], seed, true, nil
	sess := &sessionRun{
		r: r, st: st, rng: rng, req: req, id: id, seed: seed, sc: sc,
		off:  loadgen.Draw(r.cfg.DrawSeed, g, len(sc.next)),
		msgs: []loadgen.Message{{Role: "system", Content: sc.system}, {Role: "user", Content: sc.first}},
	}
	st.SessionsStarted++
	r.started.Add(1)
	start := time.Now()
	status, reason := sess.run()
	st.end(status, reason, time.Since(start))
	r.ended.Add(1)
	if status == statusFailed {
		r.failed.Add(1)
	}
}

// sessionRun is the state of one session.
type sessionRun struct {
	r    *runner
	st   *Stats
	rng  *rand.Rand
	req  loadgen.Request
	id   string
	seed int64
	sc   script
	off  int
	msgs []loadgen.Message
	res  loadgen.Result
}

// finalFinish are the finish reasons that end a turn.
var finalFinish = map[string]bool{"stop": true, "end_turn": true, "length": true, "max_tokens": true}

func (s *sessionRun) run() (status, reason string) {
	cfg := &s.r.cfg
	for t := range cfg.Turns {
		if t > 0 {
			if !s.r.wait(cfg.ThinkTime) {
				return statusInterrupted, ""
			}
			s.msgs = append(s.msgs, loadgen.Message{Role: "user", Content: s.sc.next[(s.off+t-1)%len(s.sc.next)]})
		}
		start := time.Now()
		if status, reason := s.turn(); status != "" {
			return status, reason
		}
		s.st.Turns++
		s.st.TurnE2E.Record(time.Since(start))
	}
	return statusCompleted, ""
}

// turn sends requests until a final answer; status "" = the turn ended.
func (s *sessionRun) turn() (status, reason string) {
	for round := 1; ; round++ {
		if s.r.interrupted() {
			return statusInterrupted, ""
		}
		s.req.Messages = s.msgs
		res := &s.res
		if s.r.do(s.st, &s.req, res, s.rng) {
			return statusInterrupted, ""
		}
		switch {
		case !res.OK():
			return statusFailed, res.Err
		case len(res.ToolCalls) == 0 && finalFinish[res.FinishReason]:
			s.msgs = append(s.msgs, *res.Message)
			return "", ""
		case len(res.ToolCalls) == 0 && (res.FinishReason == "tool_calls" || res.FinishReason == "tool_use"):
			return statusFailed, loadgen.ReasonNoToolCalls
		case len(res.ToolCalls) == 0:
			return statusFailed, loadgen.ReasonFinishPrefix + res.FinishReason
		case round >= s.r.cfg.MaxToolRounds:
			return statusFailed, ReasonMaxToolRounds
		}
		s.msgs = append(s.msgs, *res.Message)
		for _, tc := range res.ToolCalls {
			tr, err := s.r.cfg.Tools.Call(s.r.rctx, ToolRequest{Session: s.id, Seed: s.seed, Call: tc})
			if err != nil {
				return statusFailed, ReasonTools
			}
			s.st.tool(tc.Name, tr)
			s.msgs = append(s.msgs, loadgen.Message{Role: "tool", ToolCallID: tc.ID, Content: tr.Content, IsError: tr.IsError})
		}
	}
}

// do sends req with the retries of the run; it returns true when the
// interruption cut a backoff (the last attempt then counts as final).
func (r *runner) do(st *Stats, req *loadgen.Request, res *loadgen.Result, rng *rand.Rand) bool {
	for attempt := 0; ; attempt++ {
		r.client.DoInto(r.rctx, req, res)
		if res.OK() || attempt >= r.cfg.Retries || !Retryable(res.Err) {
			r.final(st, res)
			return false
		}
		wait := Backoff(rng, r.cfg.Backoff, r.cfg.BackoffMax, attempt)
		if ra, ok := RetryAfter(res.RetryAfterMS, res.RetryAfter, time.Now()); ok {
			if ra > r.cfg.RetryAfterMax {
				r.final(st, res)
				return false
			}
			wait = max(wait, ra)
		}
		if !r.wait(wait) {
			r.final(st, res)
			return true
		}
		st.retry(res.Err, wait)
		r.requests.Add(1)
		r.retriesCount.Add(1)
	}
}

// final counts the last attempt of a request.
func (r *runner) final(st *Stats, res *loadgen.Result) {
	st.Requests.Add(res)
	r.requests.Add(1)
	if !res.OK() {
		r.errors.Add(1)
	}
}
