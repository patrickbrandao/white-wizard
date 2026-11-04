package limits

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Fallback is the circuit around the Valkey store (README 6, spec 012
// on_down). The data plane never fails because of Valkey: after 3
// consecutive failures (or timeouts) the circuit opens and Check/Finish
// follow store.on_down until a PING succeeds again:
//
//   - allow: Check admits without rate-limit state, Finish does nothing;
//   - deny:  Check denies with limit store_down (overloaded, retry 1 s);
//   - local: Check/Finish go to an internal Memory, dropped when it closes.
//
// Attempts and Strike use the internal Memory in every mode; admin
// operations return ErrStoreDown while the circuit is open.
type Fallback struct {
	mode     string
	log      *slog.Logger
	connect  func(context.Context) (*Valkey, error)
	onChange func(open bool)
	probe    time.Duration

	mu sync.RWMutex
	v  *Valkey

	open  atomic.Bool
	fails atomic.Int32

	memMu sync.Mutex
	mem   *Memory

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// FallbackOptions customizes NewFallback.
type FallbackOptions struct {
	Logger *slog.Logger
	// OnChange is called on every transition (true = circuit open), e.g.
	// to set the wwz_store_circuit_open gauge.
	OnChange func(open bool)
	// ProbeInterval between PINGs (or reconnects) while open; 0 = 1 s.
	ProbeInterval time.Duration
}

// failThreshold is the number of consecutive failures that opens the circuit.
const failThreshold = 3

// NewFallback wraps the store returned by connect. If the first connection
// fails the circuit starts open and connect is retried every probe
// interval, so the process boots with Valkey down.
func NewFallback(ctx context.Context, cfg config.StoreConfig, connect func(context.Context) (*Valkey, error), o FallbackOptions) *Fallback {
	f := &Fallback{mode: cfg.OnDown, log: o.Logger, connect: connect, onChange: o.OnChange, probe: o.ProbeInterval, stop: make(chan struct{})}
	if f.mode == "" {
		f.mode = "local"
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if f.probe <= 0 {
		f.probe = time.Second
	}
	v, err := connect(ctx)
	if err != nil {
		f.log.Warn("limits: valkey unreachable at boot; circuit open", "on_down", f.mode, "err", err)
		f.open.Store(true)
		if f.onChange != nil {
			f.onChange(true)
		}
	} else {
		f.v = v
		if f.onChange != nil {
			f.onChange(false)
		}
	}
	f.wg.Go(f.prober)
	return f
}

// CircuitOpen reports whether the store is currently bypassed.
func (f *Fallback) CircuitOpen() bool { return f.open.Load() }

// Valkey returns the wrapped store (nil until the first connection).
func (f *Fallback) Valkey() *Valkey {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.v
}

// live returns the store when the circuit is closed.
func (f *Fallback) live() *Valkey {
	if f.open.Load() {
		return nil
	}
	return f.Valkey()
}

func (f *Fallback) success() { f.fails.Store(0) }

// failure counts a data plane failure and opens the circuit at the threshold.
func (f *Fallback) failure(err error) {
	f.log.Debug("limits: valkey failure", "err", err)
	if f.fails.Add(1) < failThreshold {
		return
	}
	if f.open.CompareAndSwap(false, true) {
		f.log.Warn("limits: valkey circuit open", "on_down", f.mode, "err", err)
		if f.onChange != nil {
			f.onChange(true)
		}
	}
}

func (f *Fallback) close() {
	f.memMu.Lock()
	f.mem = nil
	f.memMu.Unlock()
	f.fails.Store(0)
	if f.open.CompareAndSwap(true, false) {
		f.log.Info("limits: valkey circuit closed")
		if f.onChange != nil {
			f.onChange(false)
		}
	}
}

// memory is the internal store used while the circuit is open.
func (f *Fallback) memory() *Memory {
	f.memMu.Lock()
	defer f.memMu.Unlock()
	if f.mem == nil {
		f.mem = NewMemory()
	}
	return f.mem
}

func (f *Fallback) prober() {
	t := time.NewTicker(f.probe)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
		}
		if !f.open.Load() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), max(f.probe, 2*time.Second))
		v := f.Valkey()
		var err error
		if v == nil {
			if v, err = f.connect(ctx); err == nil {
				f.mu.Lock()
				f.v = v
				f.mu.Unlock()
			}
		} else {
			err = v.Ping(ctx)
		}
		cancel()
		if err == nil {
			f.close()
		}
	}
}

// callerGone reports whether err comes from the caller's own context (the
// client went away), which says nothing about Valkey.
func callerGone(ctx context.Context) bool { return ctx.Err() != nil }

// Check implements Store.
func (f *Fallback) Check(ctx context.Context, req CheckRequest) (Decision, error) {
	if v := f.live(); v != nil {
		d, err := v.Check(ctx, req)
		if err == nil {
			f.success()
			return d, nil
		}
		if callerGone(ctx) {
			return Decision{}, err
		}
		f.failure(err)
	}
	switch f.mode {
	case "allow":
		return Decision{Allowed: true}, nil
	case "deny":
		return Decision{Allowed: false, Kind: core.ErrOverloaded, LimitName: "store_down", RetryAfter: time.Second, Message: "control store unavailable"}, nil
	default:
		return f.memory().Check(ctx, req)
	}
}

// Finish implements Store.
func (f *Fallback) Finish(ctx context.Context, req FinishRequest) error {
	if v := f.live(); v != nil {
		err := v.Finish(ctx, req)
		if err == nil {
			f.success()
			return nil
		}
		if callerGone(ctx) {
			return err
		}
		f.failure(err)
	}
	if f.mode == "local" {
		// The interim memory store has no account history: no promotion.
		req.OnUsage = nil
		return f.memory().Finish(ctx, req)
	}
	return nil
}

// Attempts implements Store.
func (f *Fallback) Attempts(ctx context.Context, conversation, step string, ttl time.Duration) (int, error) {
	if v := f.live(); v != nil {
		n, err := v.Attempts(ctx, conversation, step, ttl)
		if err == nil {
			f.success()
			return n, nil
		}
		if callerGone(ctx) {
			return 0, err
		}
		f.failure(err)
	}
	return f.memory().Attempts(ctx, conversation, step, ttl)
}

// Strike implements Store.
func (f *Fallback) Strike(ctx context.Context, e Entity, reason string, ttl time.Duration) (int, error) {
	if v := f.live(); v != nil {
		n, err := v.Strike(ctx, e, reason, ttl)
		if err == nil {
			f.success()
			return n, nil
		}
		if callerGone(ctx) {
			return 0, err
		}
		f.failure(err)
	}
	return f.memory().Strike(ctx, e, reason, ttl)
}

// Escalate implements Escalator: in Valkey while the circuit is closed, in
// the local memory store otherwise (like Strike).
func (f *Fallback) Escalate(ctx context.Context, e Entity, escalated bool, ttl time.Duration) (Escalation, error) {
	if v := f.live(); v != nil {
		es, err := v.Escalate(ctx, e, escalated, ttl)
		if err == nil {
			f.success()
			return es, nil
		}
		if callerGone(ctx) {
			return Escalation{}, err
		}
		f.failure(err)
	}
	return f.memory().Escalate(ctx, e, escalated, ttl)
}

// admin returns the store for admin operations or ErrStoreDown.
func (f *Fallback) admin() (*Valkey, error) {
	if v := f.live(); v != nil {
		return v, nil
	}
	return nil, ErrStoreDown
}

// Block implements Store.
func (f *Fallback) Block(ctx context.Context, e Entity, reason string, d time.Duration, by string) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.Block(ctx, e, reason, d, by)
}

// Unblock implements Store.
func (f *Fallback) Unblock(ctx context.Context, e Entity) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.Unblock(ctx, e)
}

// Ban implements Store.
func (f *Fallback) Ban(ctx context.Context, e Entity, reason, by string) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.Ban(ctx, e, reason, by)
}

// Unban implements Store.
func (f *Fallback) Unban(ctx context.Context, e Entity) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.Unban(ctx, e)
}

// Get implements Store.
func (f *Fallback) Get(ctx context.Context, e Entity) (EntityState, error) {
	v, err := f.admin()
	if err != nil {
		return EntityState{}, err
	}
	return v.Get(ctx, e)
}

// List implements Store.
func (f *Fallback) List(ctx context.Context, status Status, limit int) ([]EntityState, error) {
	v, err := f.admin()
	if err != nil {
		return nil, err
	}
	return v.List(ctx, status, limit)
}

// SetCredits implements Store.
func (f *Fallback) SetCredits(ctx context.Context, e Entity, usd float64) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.SetCredits(ctx, e, usd)
}

// Reset implements Store.
func (f *Fallback) Reset(ctx context.Context) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.Reset(ctx)
}

// ResetCounters implements CounterResetter.
func (f *Fallback) ResetCounters(ctx context.Context) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.ResetCounters(ctx)
}

// ResetConversations implements ConversationResetter.
func (f *Fallback) ResetConversations(ctx context.Context, conversation string) (int, error) {
	v, err := f.admin()
	if err != nil {
		return 0, err
	}
	return v.ResetConversations(ctx, conversation)
}

// Events implements Store.
func (f *Fallback) Events(ctx context.Context, limit int) ([]Event, error) {
	v, err := f.admin()
	if err != nil {
		return nil, err
	}
	return v.Events(ctx, limit)
}

// Kind implements Store.
func (f *Fallback) Kind() string { return "valkey" }

// Close stops the prober and closes the wrapped store.
func (f *Fallback) Close() error {
	var err error
	f.once.Do(func() {
		close(f.stop)
		f.wg.Wait()
		if v := f.Valkey(); v != nil {
			err = v.Close()
		}
	})
	return err
}

// OpenValkey builds the production store of store.kind: valkey: NewValkey
// wrapped in the on_down circuit.
func OpenValkey(ctx context.Context, cfg config.StoreConfig, vo ValkeyOptions, fo FallbackOptions) *Fallback {
	if fo.Logger == nil {
		fo.Logger = vo.Logger
	}
	return NewFallback(ctx, cfg, func(ctx context.Context) (*Valkey, error) {
		return NewValkey(ctx, cfg, vo)
	}, fo)
}
