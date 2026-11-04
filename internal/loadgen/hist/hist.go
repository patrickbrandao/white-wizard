// Package hist is the mergeable log-linear latency histogram of wwz-load
// (ADR 0024). Values are kept in microseconds from 1 µs to 1 h; every power
// of two is split into 128 linear sub-buckets, so a quantile is within 1/256
// of the true sample (relative error < 1 %). Count, sum, minimum and maximum
// are exact. Merging is a sum of counters: the quantiles of merged histograms
// are identical to those of one histogram that recorded every sample.
//
// A Hist is not safe for concurrent use: keep one per worker goroutine and
// Merge them at the end.
package hist

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"
)

const (
	subBits = 7
	subN    = 1 << subBits // linear sub-buckets per power of two
	// MaxValue is the largest value kept apart from the exact maximum;
	// longer samples are counted in the last bucket.
	MaxValue = time.Hour
	maxMicro = uint64(MaxValue / time.Microsecond)
	// Version is the serialization version.
	Version = 1
)

// nBuckets covers every index produced by index(maxMicro).
var nBuckets = index(maxMicro) + 1

// index maps a value in microseconds to its bucket.
func index(v uint64) int {
	if v < 2*subN {
		return int(v)
	}
	e := bits.Len64(v) - 1
	return (e-subBits+1)<<subBits + int((v>>(e-subBits))&(subN-1))
}

// bucketRange returns the lowest value and the width of bucket i, in µs.
func bucketRange(i int) (low, width uint64) {
	if i < 2*subN {
		return uint64(i), 1 //nolint:gosec // G115: 0 ≤ i < 256
	}
	e := i>>subBits + subBits - 1
	shift := e - subBits
	return uint64(subN+i&(subN-1)) << shift, 1 << shift
}

// Hist is a log-linear histogram of durations.
type Hist struct {
	counts []uint64
	n      uint64
	sum    time.Duration
	min    time.Duration
	max    time.Duration
}

// New returns an empty histogram.
func New() *Hist { return &Hist{} }

// Record adds one sample. Negative durations count as zero.
func (h *Hist) Record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	if h.counts == nil {
		h.counts = make([]uint64, nBuckets)
	}
	v := uint64(d / time.Microsecond)
	if v > maxMicro {
		v = maxMicro
	}
	h.counts[index(v)]++
	if h.n == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.n++
	h.sum += d
}

// Merge adds every sample of o to h. o is not changed.
func (h *Hist) Merge(o *Hist) {
	if o == nil || o.n == 0 {
		return
	}
	if h.counts == nil {
		h.counts = make([]uint64, nBuckets)
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	if h.n == 0 || o.min < h.min {
		h.min = o.min
	}
	if o.max > h.max {
		h.max = o.max
	}
	h.n += o.n
	h.sum += o.sum
}

// Reset empties the histogram, keeping its memory.
func (h *Hist) Reset() {
	clear(h.counts)
	h.n, h.sum, h.min, h.max = 0, 0, 0, 0
}

// Count is the number of samples.
func (h *Hist) Count() uint64 { return h.n }

// Sum is the exact sum of the samples.
func (h *Hist) Sum() time.Duration { return h.sum }

// Min is the exact smallest sample (0 when empty).
func (h *Hist) Min() time.Duration { return h.min }

// Max is the exact largest sample (0 when empty).
func (h *Hist) Max() time.Duration { return h.max }

// Mean is the exact mean (0 when empty).
func (h *Hist) Mean() time.Duration {
	if h.n == 0 {
		return 0
	}
	return h.sum / time.Duration(h.n) //nolint:gosec // G115: a count never reaches 2^63
}

// Quantile returns the nearest-rank q-quantile (0 ≤ q ≤ 1), within 1 % of
// the true sample and clamped to [Min, Max]. q ≤ 0 is Min, q ≥ 1 is Max.
func (h *Hist) Quantile(q float64) time.Duration {
	switch {
	case h.n == 0:
		return 0
	case q <= 0 || math.IsNaN(q):
		return h.min
	case q >= 1:
		return h.max
	}
	rank := uint64(math.Ceil(q * float64(h.n))) // ≥ 1 because q > 0 and n ≥ 1
	var seen uint64
	for i := 0; ; i++ { // terminates: the counts sum to n ≥ rank
		seen += h.counts[i]
		if seen >= rank {
			low, width := bucketRange(i)
			d := time.Duration(low+(width-1)/2) * time.Microsecond //nolint:gosec // G115: ≤ 2^32 µs
			return min(max(d, h.min), h.max)
		}
	}
}

// Summary is a distribution in milliseconds with 3 decimals, as in the
// wwz-load report (spec 015).
type Summary struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

// Ms converts d to milliseconds rounded to 3 decimals.
func Ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

// Summary returns count, mean, p50, p90, p99 and max in milliseconds.
func (h *Hist) Summary() Summary {
	return Summary{
		Count: h.n,
		Mean:  Ms(h.Mean()),
		P50:   Ms(h.Quantile(0.50)),
		P90:   Ms(h.Quantile(0.90)),
		P99:   Ms(h.Quantile(0.99)),
		Max:   Ms(h.max),
	}
}

// wire is the sparse JSON form: only non-empty buckets as [index, count].
type wire struct {
	Version int         `json:"version"`
	Count   uint64      `json:"count"`
	SumNs   int64       `json:"sum_ns"`
	MinNs   int64       `json:"min_ns"`
	MaxNs   int64       `json:"max_ns"`
	Buckets [][2]uint64 `json:"buckets"`
}

// MarshalJSON encodes the histogram sparsely (ADR 0024).
func (h *Hist) MarshalJSON() ([]byte, error) {
	w := wire{Version: Version, Count: h.n, SumNs: int64(h.sum), MinNs: int64(h.min), MaxNs: int64(h.max), Buckets: [][2]uint64{}}
	for i, c := range h.counts {
		if c != 0 {
			w.Buckets = append(w.Buckets, [2]uint64{uint64(i), c})
		}
	}
	return json.Marshal(w)
}

// ErrBadHist reports a serialized histogram that cannot be decoded.
var ErrBadHist = errors.New("hist: invalid serialized histogram")

// UnmarshalJSON decodes MarshalJSON output, replacing the contents of h.
func (h *Hist) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("%w: %w", ErrBadHist, err)
	}
	if w.Version != Version {
		return fmt.Errorf("%w: version %d", ErrBadHist, w.Version)
	}
	var n uint64
	counts := make([]uint64, nBuckets)
	for _, b := range w.Buckets {
		if b[0] >= uint64(nBuckets) { //nolint:gosec // G115: nBuckets is a small positive constant
			return fmt.Errorf("%w: bucket %d", ErrBadHist, b[0])
		}
		counts[b[0]] += b[1]
		n += b[1]
	}
	if n != w.Count {
		return fmt.Errorf("%w: count %d, buckets sum %d", ErrBadHist, w.Count, n)
	}
	*h = Hist{counts: counts, n: n, sum: time.Duration(w.SumNs), min: time.Duration(w.MinNs), max: time.Duration(w.MaxNs)}
	return nil
}
