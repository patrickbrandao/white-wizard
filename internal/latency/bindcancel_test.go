package latency

import (
	"context"
	"testing"
	"time"
)

// TestBindCancel: once bound, a wait parks on the plan's timer alone and
// the cancellation fires it: the wait ends at once with false, and so does
// every later one; until then waits sleep as planned, with no allocation.
func TestBindCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Plan{}
	stop := p.BindCancel(ctx)
	if p.timer == nil || !p.bound {
		t.Fatal("bind arms the plan's timer")
	}
	start := time.Now()
	if !p.Wait(5*time.Millisecond, ctx.Done()) || time.Since(start) < 5*time.Millisecond {
		t.Fatal("a bound wait sleeps as planned")
	}
	if n := testing.AllocsPerRun(10, func() { p.Wait(time.Microsecond, ctx.Done()) }); n != 0 {
		t.Fatalf("%v allocs per bound wait", n)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start = time.Now()
	if p.Wait(time.Hour, ctx.Done()) || time.Since(start) > 10*time.Second {
		t.Fatal("the cancellation ends a bound wait")
	}
	if p.Wait(time.Hour, ctx.Done()) {
		t.Fatal("a wait after the cancellation is false")
	}
	if stop() {
		t.Fatal("stop after the cancellation fired: false")
	}
	if p.bound {
		t.Fatal("stop unbinds")
	}

	// Stopped before the cancellation: later waits select again.
	ctx2, cancel2 := context.WithCancel(context.Background())
	p2 := &Plan{}
	p2.Wait(time.Microsecond, nil) // the timer exists before the bind
	stop2 := p2.BindCancel(ctx2)
	if !stop2() {
		t.Fatal("stop before the cancellation: true")
	}
	cancel2()
	if p2.Wait(time.Hour, ctx2.Done()) {
		t.Fatal("unbound wait sees done")
	}
}

// TestBindCancelNoop: logical plans and contexts that never end bind
// nothing.
func TestBindCancelNoop(t *testing.T) {
	p := &Plan{Logical: true}
	if p.BindCancel(context.Background())() || p.bound || p.timer != nil {
		t.Fatal("logical plan: nothing to bind")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if p.BindCancel(ctx)() || p.bound {
		t.Fatal("logical plan with a context: nothing to bind")
	}
	p = &Plan{}
	if p.BindCancel(context.Background())() || p.bound {
		t.Fatal("a context with no Done: nothing to bind")
	}
}
