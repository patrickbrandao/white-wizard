package hist

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func exactQuantile(sorted []time.Duration, q float64) time.Duration {
	rank := int(math.Ceil(q * float64(len(sorted))))
	rank = max(rank, 1)
	return sorted[rank-1]
}

func TestIndexAndRangeAgree(t *testing.T) {
	prev := -1
	for _, v := range []uint64{0, 1, 127, 128, 255, 256, 257, 511, 512, 1000, 1 << 20, 123456789, maxMicro} {
		i := index(v)
		if i < prev {
			t.Fatalf("index not monotonic at %d", v)
		}
		prev = i
		low, width := bucketRange(i)
		if v < low || v >= low+width {
			t.Fatalf("v=%d in bucket %d [%d,%d)", v, i, low, low+width)
		}
		if v >= 256 && float64(width)/float64(low) > 1.0/128+1e-12 {
			t.Fatalf("bucket %d too wide: %d/%d", i, width, low)
		}
	}
	if index(maxMicro) != nBuckets-1 {
		t.Fatal("last bucket")
	}
}

// TestQuantileError: every quantile within 1 % of the exact nearest-rank sample.
func TestQuantileError(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, dist := range []string{"uniform", "lognormal", "spread"} {
		h := New()
		var all []time.Duration
		for range 20000 {
			var d time.Duration
			switch dist {
			case "uniform":
				d = time.Duration(100+rng.IntN(10_000_000)) * time.Microsecond
			case "lognormal":
				d = time.Duration(math.Exp(rng.NormFloat64()*1.5+10)) * time.Microsecond
			default:
				d = time.Duration(math.Pow(10, 2+rng.Float64()*7)) * time.Microsecond
			}
			d = max(d, 100*time.Microsecond)
			all = append(all, d)
			h.Record(d)
		}
		slices.Sort(all)
		for _, q := range []float64{0.001, 0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999} {
			want := exactQuantile(all, q)
			got := h.Quantile(q)
			if rel := math.Abs(float64(got-want)) / float64(want); rel > 0.01 {
				t.Errorf("%s q=%v: got %v want %v (rel %.4f)", dist, q, got, want, rel)
			}
		}
		if h.Quantile(0) != all[0] || h.Quantile(1) != all[len(all)-1] || h.Min() != all[0] || h.Max() != all[len(all)-1] {
			t.Errorf("%s: min/max not exact", dist)
		}
		var sum time.Duration
		for _, d := range all {
			sum += d
		}
		if h.Sum() != sum || h.Mean() != sum/time.Duration(len(all)) || h.Count() != uint64(len(all)) {
			t.Errorf("%s: sum/mean/count not exact", dist)
		}
	}
}

// TestMergeEqualsSingle is the property of ADR 0024: merging shards is the
// same as recording everything in one histogram.
func TestMergeEqualsSingle(t *testing.T) {
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 7))
		single := New()
		shards := make([]*Hist, 1+rng.IntN(6))
		for i := range shards {
			shards[i] = New()
		}
		for range 1 + rng.IntN(5000) {
			d := time.Duration(rng.Int64N(int64(2 * time.Hour)))
			single.Record(d)
			shards[rng.IntN(len(shards))].Record(d)
		}
		merged := New()
		for _, s := range shards {
			merged.Merge(s)
		}
		// Merge through the JSON form too, as the controller does.
		viaJSON := New()
		for _, s := range shards {
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var back Hist
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			viaJSON.Merge(&back)
		}
		for _, m := range []*Hist{merged, viaJSON} {
			if m.Count() != single.Count() || m.Sum() != single.Sum() || m.Min() != single.Min() || m.Max() != single.Max() {
				t.Fatalf("seed %d: totals differ", seed)
			}
			for q := 0.0; q <= 1; q += 0.01 {
				if m.Quantile(q) != single.Quantile(q) {
					t.Fatalf("seed %d q=%v: %v != %v", seed, q, m.Quantile(q), single.Quantile(q))
				}
			}
			if m.Summary() != single.Summary() {
				t.Fatalf("seed %d: summary differs", seed)
			}
		}
	}
}

func TestEdges(t *testing.T) {
	h := New()
	if h.Quantile(0.5) != 0 || h.Mean() != 0 || h.Summary() != (Summary{}) {
		t.Fatal("empty histogram must be zero")
	}
	h.Merge(nil)
	h.Merge(New())
	if h.Count() != 0 {
		t.Fatal("merge of empty")
	}
	h.Record(-time.Second) // counts as zero
	h.Record(3 * time.Hour)
	if h.Min() != 0 || h.Max() != 3*time.Hour || h.Quantile(math.NaN()) != 0 {
		t.Fatalf("min %v max %v", h.Min(), h.Max())
	}
	if got := h.Quantile(0.99); got < 59*time.Minute || got > 3*time.Hour {
		t.Fatalf("clamped sample quantile %v", got)
	}
	// Merge into an empty histogram sets min from the other side.
	e := New()
	e.Merge(h)
	if e.Min() != 0 || e.Count() != 2 {
		t.Fatal("merge into empty")
	}
	o := New()
	o.Record(time.Millisecond)
	o.Merge(h)
	if o.Min() != 0 {
		t.Fatal("merge must lower min")
	}
	h.Reset()
	if h.Count() != 0 || h.Max() != 0 || h.Quantile(0.5) != 0 {
		t.Fatal("reset")
	}
	h.Record(1500 * time.Microsecond)
	s := h.Summary()
	if s.Count != 1 || s.P50 != 1.5 || s.Max != 1.5 || s.Mean != 1.5 {
		t.Fatalf("summary %+v", s)
	}
	if Ms(1234567*time.Nanosecond) != 1.235 {
		t.Fatal("Ms rounding")
	}
}

func TestJSON(t *testing.T) {
	h := New()
	b, err := json.Marshal(h)
	if err != nil || string(b) != `{"version":1,"count":0,"sum_ns":0,"min_ns":0,"max_ns":0,"buckets":[]}` {
		t.Fatalf("empty json %s %v", b, err)
	}
	h.Record(3 * time.Microsecond)
	h.Record(3 * time.Microsecond)
	b, _ = json.Marshal(h)
	if string(b) != `{"version":1,"count":2,"sum_ns":6000,"min_ns":3000,"max_ns":3000,"buckets":[[3,2]]}` {
		t.Fatalf("json %s", b)
	}
	for _, bad := range []string{
		`{`,
		`{"version":2}`,
		`{"version":1,"count":1,"buckets":[[999999,1]]}`,
		`{"version":1,"count":5,"buckets":[[1,1]]}`,
	} {
		var x Hist
		if err := x.UnmarshalJSON([]byte(bad)); !errors.Is(err, ErrBadHist) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func BenchmarkRecord(b *testing.B) {
	h := New()
	for i := range b.N {
		h.Record(time.Duration(i%1_000_000) * time.Microsecond)
	}
}
