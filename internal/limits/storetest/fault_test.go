package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// This file checks the suite itself (ADR 0013): every subtest runs against
// limits.Memory wrapped in a faultStore that breaks one call (or a suffix of
// calls) at a time, with a fakeT that records failures. Each fault must make
// the subtest fail, which also exercises every assertion of the suite.

// fakeT records failures; Fatal* and Skip end the goroutine like *testing.T.
type fakeT struct {
	mu      sync.Mutex
	msgs    []string
	skipped bool
}

func (f *fakeT) Helper() {}

func (f *fakeT) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}

func (f *fakeT) Fatal(args ...any) {
	f.Errorf("%s", fmt.Sprint(args...))
	runtime.Goexit()
}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

func (f *fakeT) Skip(...any) {
	f.mu.Lock()
	f.skipped = true
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeT) failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs) > 0
}

// runCase runs fn in its own goroutine (so Goexit ends only the subtest) and
// reports a panic as a failure.
func runCase(fn func(T, limits.Store), s limits.Store) *fakeT {
	ft := &fakeT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				ft.Errorf("panic: %v", r)
			}
		}()
		fn(ft, s)
	}()
	<-done
	return ft
}

type faultMode int

const (
	modeNone    faultMode = iota
	modeErr               // return an error without calling the inner store
	modeSkip              // return the zero result and nil, without calling the inner store
	modePerturb           // call the inner store and perturb one leaf of the result
)

func (m faultMode) String() string {
	return [...]string{"none", "err", "skip", "perturb"}[m]
}

var errFault = errors.New("injected fault")

// faultStore wraps limits.Memory (which implements every optional
// interface) and applies mode to call number at (or to every call from at
// on, when suffix). In modeNone it records how many perturbations the result
// of each call admits.
type faultStore struct {
	inner   *limits.Memory
	mode    faultMode
	at      int
	suffix  bool
	variant int

	mu       sync.Mutex
	calls    int
	variants []int
}

// hide strips optional interfaces from the store seen by the subtest.
type tierNoPolicies struct {
	limits.Store
	limits.TierSetter
	limits.EntityResetter
}

func (f *faultStore) take() faultMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.calls
	f.calls++
	if f.mode == modeNone {
		f.variants = append(f.variants, 0)
		return modeNone
	}
	if n == f.at || (f.suffix && n > f.at) {
		return f.mode
	}
	return modeNone
}

func (f *faultStore) record(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == modeNone && len(f.variants) > 0 {
		f.variants[len(f.variants)-1] = n
	}
}

// do runs one call of the inner store under the fault plan.
func do[R any](f *faultStore, call func() (R, error)) (R, error) {
	var zero R
	switch f.take() {
	case modeErr:
		return zero, errFault
	case modeSkip:
		return zero, nil
	case modePerturb:
		r, err := call()
		if err != nil {
			return r, err
		}
		return perturb(r, f.variant), nil
	}
	r, err := call()
	if err == nil {
		f.record(countVariants(r))
	}
	return r, err
}

func do0(f *faultStore, call func() error) error {
	_, err := do(f, func() (struct{}, error) { return struct{}{}, call() })
	return err
}

func (f *faultStore) Check(ctx context.Context, req limits.CheckRequest) (limits.Decision, error) {
	return do(f, func() (limits.Decision, error) { return f.inner.Check(ctx, req) })
}

func (f *faultStore) Finish(ctx context.Context, req limits.FinishRequest) error {
	return do0(f, func() error { return f.inner.Finish(ctx, req) })
}

func (f *faultStore) Block(ctx context.Context, e limits.Entity, reason string, d time.Duration, by string) error {
	return do0(f, func() error { return f.inner.Block(ctx, e, reason, d, by) })
}

func (f *faultStore) Unblock(ctx context.Context, e limits.Entity) error {
	return do0(f, func() error { return f.inner.Unblock(ctx, e) })
}

func (f *faultStore) Ban(ctx context.Context, e limits.Entity, reason, by string) error {
	return do0(f, func() error { return f.inner.Ban(ctx, e, reason, by) })
}

func (f *faultStore) Unban(ctx context.Context, e limits.Entity) error {
	return do0(f, func() error { return f.inner.Unban(ctx, e) })
}

func (f *faultStore) Strike(ctx context.Context, e limits.Entity, reason string, ttl time.Duration) (int, error) {
	return do(f, func() (int, error) { return f.inner.Strike(ctx, e, reason, ttl) })
}

func (f *faultStore) Get(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	return do(f, func() (limits.EntityState, error) { return f.inner.Get(ctx, e) })
}

func (f *faultStore) List(ctx context.Context, status limits.Status, limit int) ([]limits.EntityState, error) {
	return do(f, func() ([]limits.EntityState, error) { return f.inner.List(ctx, status, limit) })
}

func (f *faultStore) Attempts(ctx context.Context, conversation, step string, ttl time.Duration) (int, error) {
	return do(f, func() (int, error) { return f.inner.Attempts(ctx, conversation, step, ttl) })
}

func (f *faultStore) SetCredits(ctx context.Context, e limits.Entity, usd float64) error {
	return do0(f, func() error { return f.inner.SetCredits(ctx, e, usd) })
}

func (f *faultStore) Reset(ctx context.Context) error {
	return do0(f, func() error { return f.inner.Reset(ctx) })
}

func (f *faultStore) Events(ctx context.Context, limit int) ([]limits.Event, error) {
	return do(f, func() ([]limits.Event, error) { return f.inner.Events(ctx, limit) })
}

func (f *faultStore) Kind() string {
	k, _ := do(f, func() (string, error) { return f.inner.Kind(), nil })
	return k
}

func (f *faultStore) Close() error { return f.inner.Close() }

func (f *faultStore) ResetCounters(ctx context.Context) error {
	return do0(f, func() error { return f.inner.ResetCounters(ctx) })
}

func (f *faultStore) ResetConversations(ctx context.Context, conversation string) (int, error) {
	return do(f, func() (int, error) { return f.inner.ResetConversations(ctx, conversation) })
}

func (f *faultStore) SetTier(ctx context.Context, e limits.Entity, tier string) error {
	return do0(f, func() error { return f.inner.SetTier(ctx, e, tier) })
}

func (f *faultStore) ResetEntity(ctx context.Context, e limits.Entity, counters, strikes bool) error {
	return do0(f, func() error { return f.inner.ResetEntity(ctx, e, counters, strikes) })
}

func (f *faultStore) Policies(ctx context.Context) (limits.Policies, error) {
	return do(f, func() (limits.Policies, error) { return f.inner.Policies(ctx) })
}

func (f *faultStore) WatchPolicies(fn func(limits.Policies, error)) {
	_ = do0(f, func() error {
		f.inner.WatchPolicies(fn)
		return nil
	})
}

func (f *faultStore) AddCredits(ctx context.Context, e limits.Entity, usd float64) (limits.EntityState, error) {
	return do(f, func() (limits.EntityState, error) { return f.inner.AddCredits(ctx, e, usd) })
}

func (f *faultStore) SwapTier(ctx context.Context, e limits.Entity, expected, tier string) (bool, error) {
	return do(f, func() (bool, error) { return f.inner.SwapTier(ctx, e, expected, tier) })
}

func (f *faultStore) PromptCacheLookup(ctx context.Context, a limits.Entity, hashes []string, now time.Time) (int, error) {
	return do(f, func() (int, error) { return f.inner.PromptCacheLookup(ctx, a, hashes, now) })
}

func (f *faultStore) PromptCacheCommit(ctx context.Context, a limits.Entity, entries []limits.PromptCacheEntry, now time.Time) (int, error) {
	return do(f, func() (int, error) { return f.inner.PromptCacheCommit(ctx, a, entries, now) })
}

func (f *faultStore) ResponsePut(ctx context.Context, id string, rec limits.ResponseRecord) error {
	return do0(f, func() error { return f.inner.ResponsePut(ctx, id, rec) })
}

func (f *faultStore) ResponseGet(ctx context.Context, id, account string, now time.Time) (*limits.ResponseRecord, error) {
	return do(f, func() (*limits.ResponseRecord, error) { return f.inner.ResponseGet(ctx, id, account, now) })
}

func (f *faultStore) Escalate(ctx context.Context, e limits.Entity, escalated bool, ttl time.Duration) (limits.Escalation, error) {
	return do(f, func() (limits.Escalation, error) { return f.inner.Escalate(ctx, e, escalated, ttl) })
}

var (
	_ limits.Escalator            = (*faultStore)(nil)
	_ limits.ResponseRegistry     = (*faultStore)(nil)
	_ limits.PromptCacher         = (*faultStore)(nil)
	_ limits.CounterResetter      = (*faultStore)(nil)
	_ limits.ConversationResetter = (*faultStore)(nil)
	_ limits.TierSetter           = (*faultStore)(nil)
	_ limits.EntityResetter       = (*faultStore)(nil)
	_ limits.PolicySource         = (*faultStore)(nil)
	_ limits.CreditAdder          = (*faultStore)(nil)
	_ limits.TierSwapper          = (*faultStore)(nil)
)

// countVariants returns how many perturbations perturb admits for v.
func countVariants[R any](v R) int {
	w := &walker{target: -1}
	c := deepCopy(reflect.ValueOf(&v).Elem())
	w.walk(c)
	return w.seen
}

// perturb returns a deep copy of v with perturbation number variant applied.
func perturb[R any](v R, variant int) R {
	c := deepCopy(reflect.ValueOf(&v).Elem())
	(&walker{target: variant}).walk(c)
	return c.Interface().(R)
}

var timeType = reflect.TypeFor[time.Time]()

// walker enumerates perturbations of a value in a fixed order and applies
// the one numbered target (target < 0 only counts them).
type walker struct {
	target, seen int
	done         bool
}

func (w *walker) hit() bool {
	n := w.seen
	w.seen++
	if !w.done && n == w.target {
		w.done = true
		return true
	}
	return false
}

// walk visits the settable value v.
func (w *walker) walk(v reflect.Value) {
	if w.done {
		return // the shape may have changed; stop after the perturbation
	}
	if v.Type() == timeType {
		t := v.Interface().(time.Time)
		if w.hit() {
			v.Set(reflect.ValueOf(t.Add(time.Hour)))
		}
		if w.hit() {
			v.Set(reflect.ValueOf(time.Time{}))
		}
		if w.hit() {
			v.Set(reflect.ValueOf(time.Now().Add(-time.Hour)))
		}
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		if w.hit() {
			v.SetBool(!v.Bool())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		x := v.Int()
		for _, y := range []int64{x + 1, x - 1, x + 1e12, -x - 1e12} {
			if w.hit() {
				v.SetInt(y)
			}
		}
	case reflect.Float32, reflect.Float64:
		x := v.Float()
		for _, y := range []float64{x + 1, x - 1, x + 1e9, -x - 1e9} {
			if w.hit() {
				v.SetFloat(y)
			}
		}
	case reflect.String:
		if w.hit() {
			v.SetString(v.String() + "x")
		}
		if w.hit() {
			v.SetString("")
		}
	case reflect.Pointer:
		if v.IsNil() {
			if w.hit() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			return
		}
		if w.hit() {
			v.Set(reflect.Zero(v.Type()))
			return
		}
		w.walk(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				w.walk(f)
			}
		}
	case reflect.Slice:
		n := v.Len()
		if w.hit() {
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		}
		if n == 0 {
			return
		}
		if w.hit() {
			v.Set(v.Slice(0, n-1))
		}
		if w.hit() {
			v.Set(v.Slice(1, n))
		}
		if w.hit() {
			v.Set(reflect.Append(v, v.Index(0)))
		}
		for i := 0; i < n && !w.done; i++ {
			w.walk(v.Index(i))
		}
	case reflect.Map:
		w.walkMap(v)
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		c := reflect.New(v.Elem().Type()).Elem()
		c.Set(v.Elem())
		w.walk(c)
		v.Set(c)
	}
}

func (w *walker) walkMap(v reflect.Value) {
	if v.IsNil() {
		if w.hit() {
			v.Set(reflect.MakeMap(v.Type()))
			v.SetMapIndex(reflect.Zero(v.Type().Key()), reflect.Zero(v.Type().Elem()))
		}
		return
	}
	// An extra entry; for map[string]any it cannot be marshalled to JSON.
	if w.hit() {
		val := reflect.Zero(v.Type().Elem())
		if v.Type().Elem().Kind() == reflect.Interface {
			val = reflect.ValueOf(make(chan int))
		}
		v.SetMapIndex(reflect.Zero(v.Type().Key()), val)
	}
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		if w.hit() {
			v.SetMapIndex(k, reflect.Value{})
			continue
		}
		c := reflect.New(v.Type().Elem()).Elem()
		c.Set(v.MapIndex(k))
		w.walk(c)
		v.SetMapIndex(k, c)
	}
}

// deepCopy returns a settable deep copy of v (maps, slices and pointers are
// duplicated, so perturbing the copy never touches the inner store).
func deepCopy(v reflect.Value) reflect.Value {
	c := reflect.New(v.Type()).Elem()
	c.Set(v)
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			p := reflect.New(v.Type().Elem())
			p.Elem().Set(deepCopy(v.Elem()))
			c.Set(p)
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return c
		}
		for i := range v.NumField() {
			if f := c.Field(i); f.CanSet() {
				f.Set(deepCopy(v.Field(i)))
			}
		}
	case reflect.Slice:
		if !v.IsNil() {
			s := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := range v.Len() {
				s.Index(i).Set(deepCopy(v.Index(i)))
			}
			c.Set(s)
		}
	case reflect.Map:
		if !v.IsNil() {
			m := reflect.MakeMapWithSize(v.Type(), v.Len())
			for _, k := range v.MapKeys() {
				m.SetMapIndex(k, deepCopy(v.MapIndex(k)))
			}
			c.Set(m)
		}
	case reflect.Interface:
		if !v.IsNil() {
			c.Set(deepCopy(v.Elem()))
		}
	}
	return c
}

// plan is one fault applied to a subtest.
type plan struct {
	mode    faultMode
	at      int
	suffix  bool
	variant int
}

func (p plan) String() string {
	return fmt.Sprintf("%s at=%d suffix=%v variant=%d", p.mode, p.at, p.suffix, p.variant)
}

// plans lists the faults for a subtest whose clean run made the calls with
// the given variant counts. Long (parallel) subtests get faults only at the
// first and last calls, the deterministic part of their sequence.
func plans(variants []int) []plan {
	n := len(variants)
	var at []int
	if n <= 200 {
		for k := range n {
			at = append(at, k)
		}
	} else {
		for k := range 10 {
			at = append(at, k)
		}
		for k := n - 20; k < n; k++ {
			at = append(at, k)
		}
	}
	var ps []plan
	for _, k := range at {
		ps = append(ps, plan{mode: modeErr, at: k}, plan{mode: modeSkip, at: k},
			plan{mode: modeErr, at: k, suffix: true}, plan{mode: modeSkip, at: k, suffix: true})
		nv := variants[k]
		if n > 200 {
			nv = min(nv, 1)
		}
		for v := range nv {
			ps = append(ps, plan{mode: modePerturb, at: k, variant: v})
		}
	}
	// Every call perturbed the same way (flipping Decision.Allowed, the
	// first leaf, lets every worker of the parallel subtests in).
	ps = append(ps, plan{mode: modePerturb, at: 0, suffix: true, variant: 0})
	return ps
}

// TestSuiteDetectsFaults runs every subtest against faulty stores and
// requires that at least one fault fails it and that injecting an error into
// every call always does.
func TestSuiteDetectsFaults(t *testing.T) {
	for _, g := range suite() {
		for _, c := range g.cases {
			t.Run(g.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				base := &faultStore{inner: limits.NewMemory()}
				if ft := runCase(c.fn, base); ft.failed() || ft.skipped {
					t.Fatalf("clean run: failed=%v skipped=%v %v", ft.failed(), ft.skipped, ft.msgs)
				}
				ps := plans(base.variants)
				caught := make([]bool, len(ps))
				var wg sync.WaitGroup
				sem := make(chan struct{}, 32)
				for i, p := range ps {
					wg.Go(func() {
						sem <- struct{}{}
						defer func() { <-sem }()
						fs := &faultStore{inner: limits.NewMemory(), mode: p.mode, at: p.at, suffix: p.suffix, variant: p.variant}
						caught[i] = runCase(c.fn, fs).failed()
					})
				}
				wg.Wait()
				detected := false
				for i, p := range ps {
					detected = detected || caught[i]
					if p.mode == modeErr && p.at == 0 && p.suffix && !caught[i] {
						t.Errorf("every call failing went unnoticed")
					}
				}
				if !detected {
					t.Errorf("no fault of %d was detected", len(ps))
				}
			})
		}
	}
}

// TestSkipWithoutPolicySource covers the subtests that need
// limits.PolicySource next to limits.TierSetter.
func TestSkipWithoutPolicySource(t *testing.T) {
	m := limits.NewMemory()
	s := tierNoPolicies{m, m, m}
	for _, fn := range []func(T, limits.Store){testSetTier, testSetTierRemove} {
		if ft := runCase(fn, s); !ft.skipped || ft.failed() {
			t.Errorf("skipped=%v msgs=%v", ft.skipped, ft.msgs)
		}
	}
}

// TestCheckBugsUnknown covers the report of a KnownBug with no subtest.
func TestCheckBugsUnknown(t *testing.T) {
	ft := runCase(func(t T, _ limits.Store) {
		checkBugs(t, suite(), map[string]string{"Nope/Nothing": "x", "Reset/KindNotEmpty": "y"})
	}, nil)
	if len(ft.msgs) != 1 || !strings.Contains(ft.msgs[0], "Nope/Nothing") {
		t.Fatalf("msgs = %v", ft.msgs)
	}
}

// TestPerturbDoesNotTouchInput guards the deep copy.
func TestPerturbDoesNotTouchInput(t *testing.T) {
	in := []limits.Event{{Type: "a", Fields: map[string]any{"k": []string{"v"}}}}
	n := countVariants(in)
	for v := range n {
		_ = perturb(in, v)
	}
	if in[0].Type != "a" || in[0].Fields["k"].([]string)[0] != "v" || len(in[0].Fields) != 1 {
		t.Fatalf("input changed: %+v", in)
	}
}
