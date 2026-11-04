package latency

import (
	"testing"
	"time"
)

// TestWaitReusesTimer: one timer per plan, reset by every Wait (spec 014);
// a Wait cut by done leaves no stale expiry for the next one, and a wait
// costs no allocation once the timer exists (task 158).
func TestWaitReusesTimer(t *testing.T) {
	p := &Plan{}
	open := make(chan struct{})
	if !p.Wait(time.Millisecond, open) || p.timer == nil {
		t.Fatal("first wait: sleeps on a new timer")
	}
	first := p.timer
	if !p.Wait(time.Millisecond, open) || p.timer != first {
		t.Fatal("second wait: same timer")
	}

	closed := make(chan struct{})
	close(closed)
	if p.Wait(time.Hour, closed) {
		t.Fatal("done before the timer: false")
	}
	start := time.Now()
	if !p.Wait(20*time.Millisecond, open) {
		t.Fatal("wait after a cancelled one")
	}
	if el := time.Since(start); el < 20*time.Millisecond {
		t.Fatalf("a stale expiry ended the wait after %v", el)
	}
	if n := testing.AllocsPerRun(20, func() { p.Wait(time.Microsecond, open) }); n != 0 {
		t.Fatalf("%v allocs per wait", n)
	}
}
