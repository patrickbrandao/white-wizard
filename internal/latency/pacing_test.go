package latency

import (
	"context"
	"testing"
	"time"
)

// TestWaitPacesFromMark: a wait ends at the planned end of the previous one
// plus d, not at now + d (task 378), so the lateness of one chunk is not
// carried into the next and the streams do not drift into phase.
func TestWaitPacesFromMark(t *testing.T) {
	open := make(chan struct{})
	p := &Plan{}

	// First wait: no mark yet, sleeps d from now.
	start := time.Now()
	if !p.Wait(2*time.Millisecond, open) {
		t.Fatal("first wait")
	}
	if p.mark.Before(start.Add(2 * time.Millisecond)) {
		t.Fatalf("first wait plans from now: mark %v, start %v", p.mark, start)
	}

	// Started 15 ms late (scheduling delay, slow write): sleeps only the rest.
	mark := time.Now().Add(-15 * time.Millisecond)
	p.mark = mark
	start = time.Now()
	if !p.Wait(20*time.Millisecond, open) {
		t.Fatal("late wait")
	}
	if want := mark.Add(20 * time.Millisecond); !p.mark.Equal(want) {
		t.Fatalf("mark %v, want the previous mark + d %v", p.mark, want)
	}
	if el := time.Since(start); el < 4*time.Millisecond {
		t.Fatalf("woke before the planned end: %v", el)
	}

	// Overdue by more than d: returns at once and restarts from now.
	p.mark = time.Now().Add(-time.Hour)
	before := time.Now()
	if !p.Wait(time.Hour, open) {
		t.Fatal("overdue wait")
	}
	if p.mark.Before(before) || p.mark.After(time.Now()) {
		t.Fatalf("overdue wait restarts the schedule from now: mark %v", p.mark)
	}

	// A cancelled wait forgets the schedule: the next one plans from now.
	closed := make(chan struct{})
	close(closed)
	if p.Wait(time.Hour, closed) || !p.mark.IsZero() {
		t.Fatalf("cancelled wait: mark %v", p.mark)
	}
	start = time.Now()
	if !p.Wait(5*time.Millisecond, open) || time.Since(start) < 5*time.Millisecond {
		t.Fatal("wait after a cancelled one sleeps d")
	}
}

// TestWaitPacesFromMarkBound: the same schedule on a plan bound to its
// context, and a cancellation clears it.
func TestWaitPacesFromMarkBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Plan{}
	stop := p.BindCancel(ctx)
	defer stop()
	mark := time.Now().Add(-3 * time.Millisecond)
	p.mark = mark
	if !p.Wait(5*time.Millisecond, ctx.Done()) || !p.mark.Equal(mark.Add(5*time.Millisecond)) {
		t.Fatalf("bound wait: mark %v", p.mark)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	if p.Wait(time.Hour, ctx.Done()) || !p.mark.IsZero() {
		t.Fatalf("cancelled bound wait: mark %v", p.mark)
	}
	if p.Wait(time.Hour, ctx.Done()) || !p.mark.IsZero() {
		t.Fatal("wait after the cancellation: false, no schedule")
	}
}
