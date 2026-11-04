package dist

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// DefaultStartDelay separates the last registration from start_at.
const DefaultStartDelay = 2 * time.Second

// maxBody caps the request bodies of the controller (a result carries the
// sparse histograms of every step).
const maxBody = 64 << 20

// Config configures a Coordinator.
type Config struct {
	// Instances is the number of workers to wait for (≥ 1).
	Instances int
	// Token ≠ "": every call must carry "Authorization: Bearer Token".
	Token string
	// Version of the controller; a worker of another version gets 409.
	Version string
	// Plan is the whole load, split per worker.
	Plan Plan
	// StartDelay after the last registration (default DefaultStartDelay).
	StartDelay time.Duration
}

// Coordinator is the controller side: it registers the workers, hands out
// the plans and collects the results. Serve its Handler with net/http.
type Coordinator struct {
	cfg     Config
	mu      sync.Mutex
	regs    []*Registration
	ready   chan struct{}
	done    chan struct{}
	startAt time.Time
	results map[int]*Result
	closed  bool
	// stopping: the controller was interrupted; GET /v1/status says so and
	// the workers stop and send their partial results (ADR 0033).
	stopping bool
}

// NewCoordinator returns a coordinator waiting for cfg.Instances workers.
func NewCoordinator(cfg Config) *Coordinator {
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = DefaultStartDelay
	}
	return &Coordinator{cfg: cfg, ready: make(chan struct{}), done: make(chan struct{}), results: map[int]*Result{}}
}

// Ready is closed when the N workers are registered.
func (c *Coordinator) Ready() <-chan struct{} { return c.ready }

// Done is closed when the N results arrived.
func (c *Coordinator) Done() <-chan struct{} { return c.done }

// StartAt is the common start instant (zero before Ready).
func (c *Coordinator) StartAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startAt
}

// Stop puts the coordinator in the stopping state: the workers that poll
// GET /v1/status stop their run and send a partial result.
func (c *Coordinator) Stop() {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
}

// Handler serves the routes of the controller.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathRegister, c.register)
	mux.HandleFunc("POST "+PathResult, c.result)
	mux.HandleFunc("GET "+PathStatus, c.status)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.cfg.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		mux.ServeHTTP(w, r)
	})
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (c *Coordinator) register(w http.ResponseWriter, r *http.Request) {
	var reg Registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || reg.ID == "" {
		http.Error(w, "want {\"id\", \"hostname\", \"version\"} with an id", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	switch {
	case reg.Version != c.cfg.Version:
		c.mu.Unlock()
		http.Error(w, "version "+reg.Version+" differs from the controller's "+c.cfg.Version, http.StatusConflict)
		return
	case isClosed(c.ready) || c.closed:
		c.mu.Unlock()
		http.Error(w, "all instances already registered", http.StatusConflict)
		return
	case slices.ContainsFunc(c.regs, func(o *Registration) bool { return o.ID == reg.ID }):
		c.mu.Unlock()
		http.Error(w, "id "+reg.ID+" already registered", http.StatusConflict)
		return
	}
	p := &reg
	c.regs = append(c.regs, p)
	if len(c.regs) == c.cfg.Instances {
		c.startAt = time.Now().Add(c.cfg.StartDelay)
		close(c.ready)
	}
	c.mu.Unlock()
	select {
	case <-c.ready:
	case <-r.Context().Done():
		c.mu.Lock()
		gone := !isClosed(c.ready)
		if gone {
			// The worker left before the start: free its slot.
			c.regs = slices.DeleteFunc(c.regs, func(o *Registration) bool { return o == p })
		}
		c.mu.Unlock()
		if gone {
			return
		}
	}
	c.mu.Lock()
	idx := slices.Index(c.regs, p)
	a := Assignment{Index: idx, Instances: c.cfg.Instances, StartAt: c.startAt, Plan: c.cfg.Plan.Split(c.cfg.Instances, idx)}
	c.mu.Unlock()
	writeJSON(w, a)
}

func (c *Coordinator) result(w http.ResponseWriter, r *http.Request) {
	var res Result
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil || !res.valid() {
		http.Error(w, "invalid result", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !isClosed(c.ready) || res.Index < 0 || res.Index >= c.cfg.Instances {
		http.Error(w, "unknown index", http.StatusConflict)
		return
	}
	if _, dup := c.results[res.Index]; dup || c.closed {
		http.Error(w, "result already received or collection closed", http.StatusConflict)
		return
	}
	c.results[res.Index] = &res
	if len(c.results) == c.cfg.Instances {
		close(c.done)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Coordinator) status(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	st := Status{State: StateWaiting, Registered: len(c.regs), Results: len(c.results)}
	switch {
	case isClosed(c.done) || c.closed:
		st.State = StateDone
	case c.stopping:
		st.State = StateStopping
	case !isClosed(c.ready):
	case time.Now().Before(c.startAt.Add(c.cfg.Plan.Load.Expected())) || c.cfg.Plan.Load.Expected() == 0:
		st.State = StateRunning
	default:
		st.State = StateCollecting
	}
	c.mu.Unlock()
	writeJSON(w, st)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Collect closes the collection (later results get 409) and aggregates
// what arrived: the merged run, the merged conversation stats (nil without
// any) and the instances object. A worker without result, or whose result
// carries an error, is missing; errs lists "id: error" of the latter.
func (c *Coordinator) Collect() (run *loadgen.RunResult, conv *loadgen.ConvStats, inst *Instances, errs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	inst = &Instances{Expected: c.cfg.Instances, Missing: []string{}, Workers: []WorkerReport{}}
	var runs []*loadgen.RunResult
	for i, reg := range c.regs {
		res := c.results[i]
		switch {
		case res == nil:
			inst.Missing = append(inst.Missing, reg.ID)
			continue
		case res.Error != "":
			inst.Missing = append(inst.Missing, reg.ID)
			errs = append(errs, reg.ID+": "+res.Error)
			continue
		}
		h := res.Histograms
		runs = append(runs, h.Run)
		if h.Conversations != nil {
			if conv == nil {
				conv = loadgen.NewConvStats()
			}
			conv.Merge(h.Conversations)
		}
		inst.Workers = append(inst.Workers, WorkerReport{
			ID: reg.ID, Hostname: reg.Hostname, Requests: h.Run.Total.Stats.Requests, LateStartMs: res.LateStartMs,
		})
	}
	inst.Reported = len(inst.Workers)
	return Merge(runs), conv, inst, errs
}
