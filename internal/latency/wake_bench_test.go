package latency

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// benchWakes runs 5 000 goroutines that each wait 2 ms in a loop until b.N
// wakes happened, and reports the process CPU time per wake.
func benchWakes(b *testing.B, wait func(p *Plan, done <-chan struct{})) {
	const g = 5000
	var n atomic.Int64
	var wg sync.WaitGroup
	done := make(chan struct{})
	cpu0 := cpuNow()
	b.ResetTimer()
	for range g {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &Plan{}
			for n.Add(1) <= int64(b.N) {
				wait(p, done)
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(cpuNow()-cpu0)/float64(b.N), "cpu-ns/wake")
}

func cpuNow() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func BenchmarkWakeWait(b *testing.B) {
	benchWakes(b, func(p *Plan, done <-chan struct{}) { p.Wait(2*time.Millisecond, done) })
}

func BenchmarkWakeSleep(b *testing.B) {
	benchWakes(b, func(_ *Plan, _ <-chan struct{}) { time.Sleep(2 * time.Millisecond) })
}

func BenchmarkWakeTimerRecv(b *testing.B) {
	benchWakes(b, func(p *Plan, _ <-chan struct{}) {
		if p.timer == nil {
			p.timer = time.NewTimer(2 * time.Millisecond)
		} else {
			p.timer.Reset(2 * time.Millisecond)
		}
		<-p.timer.C
	})
}

// TestTimerJitter is a manual probe (task 341): WWZ_JITTER_GOROUTINES
// goroutines wait 20 ms in a loop for 10 s, bound to a context as the
// engine's streams are, and it prints how late the waits end, with no
// network involved.
func TestTimerJitter(t *testing.T) {
	g, _ := strconv.Atoi(os.Getenv("WWZ_JITTER_GOROUTINES"))
	if g <= 0 {
		t.Skip("manual probe: set WWZ_JITTER_GOROUTINES")
	}
	var hist [100001]atomic.Int64 // 10 µs buckets up to 1 s
	var stop atomic.Bool
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range g {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &Plan{}
			defer p.BindCancel(ctx)()
			for !stop.Load() {
				t0 := time.Now()
				p.Wait(20*time.Millisecond, ctx.Done())
				late := time.Since(t0) - 20*time.Millisecond
				hist[min(int(late/(10*time.Microsecond)), len(hist)-1)].Add(1)
			}
		}()
	}
	time.Sleep(10 * time.Second)
	stop.Store(true)
	wg.Wait()
	var n int64
	for i := range hist {
		n += hist[i].Load()
	}
	q := func(f float64) float64 {
		want, acc := int64(f*float64(n)), int64(0)
		for i := range hist {
			if acc += hist[i].Load(); acc > want {
				return float64(i) / 100
			}
		}
		return 0
	}
	fmt.Printf("jitter: %d goroutines, late p50 %.2f p90 %.2f p99 %.2f p999 %.2f ms\n", g, q(0.5), q(0.9), q(0.99), q(0.999))
}
