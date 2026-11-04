package auth

import (
	"sync"
	"time"
)

// limiter is a fixed-window counter per key, in memory (login attempts per
// IP, spec 016).
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

// maxBuckets triggers a sweep of expired windows.
const maxBuckets = 10000

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, hits: map[string]*bucket{}}
}

// allow counts one attempt of key at now. When the limit is exceeded it
// returns false and how long until the window resets.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) >= maxBuckets {
		for k, b := range l.hits {
			if now.Sub(b.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	b := l.hits[key]
	if b == nil || now.Sub(b.start) >= l.window {
		b = &bucket{start: now}
		l.hits[key] = b
	}
	if b.n >= l.limit {
		return false, b.start.Add(l.window).Sub(now)
	}
	b.n++
	return true, 0
}
