package metrics

import (
	"math"
	"testing"
)

// TestDefaultBuckets pins the ADR 0050 series: 1-1.5-2-3-5-7.5 per decade
// from 1 ms to 10 s, then 15, 20, 30 and 60 s.
func TestDefaultBuckets(t *testing.T) {
	var want []float64
	for _, decade := range []float64{0.001, 0.01, 0.1, 1} {
		for _, m := range []float64{1, 1.5, 2, 3, 5, 7.5} {
			want = append(want, m*decade)
		}
	}
	want = append(want, 10, 15, 20, 30, 60)
	if len(DefaultBuckets) != 29 || len(want) != 29 {
		t.Fatalf("len = %d, want 29", len(DefaultBuckets))
	}
	for i, b := range DefaultBuckets {
		if math.Abs(b-want[i]) > 1e-12*want[i] {
			t.Fatalf("bucket %d = %v, want %v", i, b, want[i])
		}
		if i == 0 {
			continue
		}
		prev := DefaultBuckets[i-1]
		if b <= prev {
			t.Fatalf("not increasing at %d: %v <= %v", i, b, prev)
		}
		// Up to 10 s each bucket is at most 67 % wider than its lower bound.
		if b <= 10 && b/prev > 1.67 {
			t.Fatalf("bucket (%v, %v] wider than 67 %%", prev, b)
		}
	}
}

// TestDefaultBucketsUsedWhenNil checks that a histogram without explicit
// buckets uses the defaults and keeps a 1.412 s maximum inside (1, 1.5].
func TestDefaultBucketsUsedWhenNil(t *testing.T) {
	r := New(10)
	h := r.NewHistogram("wwz_e2e_seconds", "E2E", nil, "model")
	h.Observe(1.412, "m")
	if got := len(h.buckets); got != 29 {
		t.Fatalf("nil buckets must default to DefaultBuckets: %d", got)
	}
	if p := h.Quantile(1, "m"); p <= 1 || p > 1.5 {
		t.Fatalf("max 1.412 s must fall in (1, 1.5]: %v", p)
	}
}
