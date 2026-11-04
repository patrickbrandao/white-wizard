package server

import (
	"sync/atomic"
	"time"
)

// gcra is a lock-free global rate limiter (generic cell rate algorithm)
// for server.max_rps: rate per second with a burst of one second's worth of
// requests. The rate is read per call, so a reload takes effect at once.
type gcra struct {
	tat atomic.Int64 // theoretical arrival time, unix nanoseconds
}

// allow reports whether one more request fits under rps at now. When it
// does not, retry is how long until the next request would fit.
func (g *gcra) allow(rps int, now time.Time) (ok bool, retry time.Duration) {
	if rps <= 0 {
		return true, 0
	}
	interval := int64(time.Second) / int64(rps)
	if interval <= 0 {
		interval = 1
	}
	burst := int64(time.Second) // tolerance: rps requests at once
	n := now.UnixNano()
	for {
		tat := g.tat.Load()
		base := max(tat, n)
		next := base + interval
		if next-n > burst {
			return false, time.Duration(next - n - burst)
		}
		if g.tat.CompareAndSwap(tat, next) {
			return true, 0
		}
	}
}
