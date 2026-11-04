package tools

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// CallRequest is one tool call.
type CallRequest struct {
	// Session id. Unknown → created with profile all; "" → a throwaway
	// session that is not kept.
	Session string
	Name    string
	// Arguments is a JSON object, or a JSON string holding the object (as
	// OpenAI sends it). Empty = {}.
	Arguments  json.RawMessage
	ToolCallID string
	// ForceOutcome forces success (normal execution, no injection), error,
	// timeout or invalid_payload. "" = draw by the configured rates.
	ForceOutcome Outcome
	// Seed of a session created by this call (x-wwz-seed); 0 = Config.Seed.
	Seed int64
}

// Result of a call. Latency is the simulated time already waited.
type Result struct {
	ToolCallID string
	Content    string
	IsError    bool
	Outcome    Outcome
	Latency    time.Duration
}

// SessionOptions create a session. ID "" = generated; Profile "" = all;
// Seed 0 = Config.Seed (and, if that is 0 too, random).
type SessionOptions struct {
	ID      string
	Profile Profile
	Seed    int64
}

// SessionInfo describes a session. Seed is the effective seed (drawn when
// the requested one was 0); Files are the paths of the virtual files,
// relative to the root, in lexicographic order.
type SessionInfo struct {
	ID      string
	Profile Profile
	Seed    int64
	Files   []string
}

// Option customises an Executor.
type Option func(*Executor)

// WithClock injects the clock used for session_ttl.
func WithClock(now func() time.Time) Option { return func(e *Executor) { e.now = now } }

// WithSleep injects the wait used for latency and timeouts. It must return
// ctx.Err() when the context ends first.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(e *Executor) { e.sleep = sleep }
}

type session struct {
	id      string
	profile Profile
	seed    int64
	mu      sync.Mutex // guards rng and fs
	rng     *rand.Rand
	fs      *vfs
	last    time.Time     // guarded by Executor.mu
	elem    *list.Element // guarded by Executor.mu
}

// Executor runs tool calls against per-session sandboxes. Safe for
// concurrent use; calls on the same session are serialised.
type Executor struct {
	cfg   Config
	eff   map[string]toolCfg
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu       sync.Mutex
	sessions map[string]*session
	lru      *list.List // front = most recently used
	ids      *rand.Rand
}

// New validates cfg and builds an Executor.
func New(cfg Config, opts ...Option) (*Executor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	e := &Executor{
		cfg:      cfg,
		eff:      map[string]toolCfg{},
		now:      time.Now,
		sleep:    sleepCtx,
		sessions: map[string]*session{},
		lru:      list.New(),
	}
	for _, t := range catalog {
		e.eff[t.Name] = cfg.effective(t.Name)
	}
	idSeed := cfg.Seed
	if idSeed == 0 {
		idSeed = randomSeed()
	}
	e.ids = newRNG(idSeed, "session-ids")
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Executor) newSession(id string, profile Profile, seed int64) *session {
	if seed == 0 {
		seed = e.cfg.Seed
	}
	if seed == 0 {
		seed = randomSeed()
	}
	return &session{
		id:      id,
		profile: profile,
		seed:    seed,
		rng:     newRNG(seed, "calls", id),
		fs:      seedTree(seed, profile),
	}
}

// lookup returns a live session, dropping it when expired. e.mu held.
func (e *Executor) lookup(id string, now time.Time) *session {
	s, ok := e.sessions[id]
	if !ok {
		return nil
	}
	if now.Sub(s.last) > e.cfg.SessionTTL {
		e.drop(s)
		return nil
	}
	return s
}

func (e *Executor) drop(s *session) {
	e.lru.Remove(s.elem)
	delete(e.sessions, s.id)
}

// store registers s, evicting expired and least recently used sessions.
// e.mu held.
func (e *Executor) store(s *session, now time.Time) {
	for back := e.lru.Back(); back != nil; back = e.lru.Back() {
		old := back.Value.(*session)
		if now.Sub(old.last) <= e.cfg.SessionTTL && e.lru.Len() < e.cfg.MaxSessions {
			break
		}
		e.drop(old)
	}
	s.last = now
	s.elem = e.lru.PushFront(s)
	e.sessions[s.id] = s
}

func (s *session) info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := make([]string, 0, len(s.fs.files))
	for p := range s.fs.files {
		files = append(files, p)
	}
	sort.Strings(files)
	return SessionInfo{ID: s.id, Profile: s.profile, Seed: s.seed, Files: files}
}

// NewSession creates a session (POST /v1/sessions).
func (e *Executor) NewSession(o SessionOptions) (SessionInfo, error) {
	p, err := ParseProfile(string(o.Profile))
	if err != nil {
		return SessionInfo{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	id := o.ID
	if id == "" {
		id = e.newID()
	} else if e.lookup(id, now) != nil {
		return SessionInfo{}, fmt.Errorf("%w: %q", ErrSessionExists, id)
	}
	s := e.newSession(id, p, o.Seed)
	e.store(s, now)
	return s.info(), nil
}

// newID returns an unused session id. e.mu held.
func (e *Executor) newID() string {
	for {
		id := fmt.Sprintf("ses_%016x", e.ids.Uint64())
		if _, taken := e.sessions[id]; !taken {
			return id
		}
	}
}

// Session describes a live session.
func (e *Executor) Session(id string) (SessionInfo, error) {
	e.mu.Lock()
	s := e.lookup(id, e.now())
	e.mu.Unlock()
	if s == nil {
		return SessionInfo{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	return s.info(), nil
}

// DeleteSession removes a session (DELETE /v1/sessions/{id}).
func (e *Executor) DeleteSession(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.lookup(id, e.now())
	if s == nil {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	e.drop(s)
	return nil
}

// Sessions is the number of kept sessions (wwz_tools_sessions), including
// expired ones not yet evicted.
func (e *Executor) Sessions() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lru.Len()
}

// callSession returns the session of a call, creating it with profile all.
func (e *Executor) callSession(id string, seed int64) *session {
	if id == "" {
		return e.newSession("", ProfileAll, seed)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	s := e.lookup(id, now)
	if s == nil {
		s = e.newSession(id, ProfileAll, seed)
		e.store(s, now)
		return s
	}
	s.last = now
	e.lru.MoveToFront(s.elem)
	return s
}

var injectedErrors = []string{
	"permission denied",
	"EIO: i/o error",
	"resource temporarily unavailable",
	"internal tool error",
}

// Call executes one tool call. Errors: ErrUnknownTool (name not in the
// catalogue or not in the session profile), ErrInvalidOutcome and the
// context error when ctx ends during the simulated wait. Every other
// failure is a Result with IsError.
func (e *Executor) Call(ctx context.Context, req CallRequest) (Result, error) {
	i := toolIndex(req.Name)
	if i < 0 {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownTool, req.Name)
	}
	def := &catalog[i]
	if _, err := ParseOutcome(string(req.ForceOutcome)); err != nil {
		return Result{}, err
	}
	s := e.callSession(req.Session, req.Seed)
	if !def.inProfile(s.profile) {
		return Result{}, fmt.Errorf("%w: %q is not in profile %s", ErrUnknownTool, req.Name, s.profile)
	}
	tc := e.eff[req.Name]
	c := &call{s: s, tc: tc, args: req.Arguments}

	s.mu.Lock()
	outcome := drawOutcome(s.rng, tc.rates)
	if req.ForceOutcome != "" {
		outcome = req.ForceOutcome
	}
	res := Result{ToolCallID: req.ToolCallID, Outcome: outcome, Latency: drawLatency(s.rng, tc.latency)}
	switch outcome {
	case OutcomeTimeout:
		res.Latency = e.cfg.Timeout
		res.IsError = true
		res.Content = "tool timed out after " + e.cfg.Timeout.String()
	case OutcomeError:
		res.IsError = true
		res.Content = injectedErrors[s.rng.IntN(len(injectedErrors))]
	case OutcomeInvalidPayload:
		res.Content = c.invalidPayload(def.Name)
	default:
		content, natural := c.run(def.Name)
		res.Content = content
		res.Outcome = OutcomeSuccess
		if natural {
			res.IsError = true
			res.Outcome = OutcomeNatural
		}
	}
	s.mu.Unlock()

	if err := e.sleep(ctx, res.Latency); err != nil {
		return Result{}, err
	}
	return res, nil
}
