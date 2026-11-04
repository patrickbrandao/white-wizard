package agents

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// Retryable reports whether an error class is transient (spec 015, "Novas
// tentativas"): http_408, http_429/*, http_5xx/* (500–599), connect, reset,
// timeout, stream_truncated and stream_error/*. Other 4xx and parse fail at
// once.
func Retryable(class string) bool {
	switch class {
	case loadgen.ErrConnect, loadgen.ErrReset, loadgen.ErrTimeout, loadgen.ErrStreamTruncated:
		return true
	}
	if strings.HasPrefix(class, "stream_error/") {
		return true
	}
	code, ok := strings.CutPrefix(class, "http_")
	if !ok {
		return false
	}
	code, _, _ = strings.Cut(code, "/")
	n, err := strconv.Atoi(code)
	return err == nil && (n == 408 || n == 429 || (n >= 500 && n <= 599))
}

// RetryAfter reads the wait the server asked for: retry-after-ms
// (milliseconds) first, then retry-after (seconds or an HTTP date, relative
// to now). ok is false when neither is present and valid.
func RetryAfter(ms, ra string, now time.Time) (time.Duration, bool) {
	if d, ok := duration(ms, time.Millisecond); ok {
		return d, true
	}
	if d, ok := duration(ra, time.Second); ok {
		return d, true
	}
	ra = strings.TrimSpace(ra)
	if t, err := http.ParseTime(ra); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// duration parses a non-negative number of units, saturating at the
// largest Duration.
func duration(s string, unit time.Duration) (time.Duration, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || !(v >= 0) {
		return 0, false
	}
	if f := v * float64(unit); f < math.MaxInt64 {
		return time.Duration(f), true
	}
	return math.MaxInt64, true
}

// Backoff is the full-jitter wait before retry attempt (0-based):
// rand(0, min(ceiling, base · 2^attempt)).
func Backoff(r *rand.Rand, base, ceiling time.Duration, attempt int) time.Duration {
	limit := ceiling
	if attempt < 62 && base <= ceiling>>attempt {
		limit = base << attempt
	}
	if limit <= 0 {
		return 0
	}
	return time.Duration(r.Int64N(int64(limit) + 1))
}
