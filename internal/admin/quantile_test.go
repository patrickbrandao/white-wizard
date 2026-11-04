package admin

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/metrics"
)

// hist builds cumulative buckets (and the _sum) for the observations.
func hist(bounds, obs []float64) ([]bucketPoint, float64) {
	pts := make([]bucketPoint, 0, len(bounds)+1)
	sum := 0.0
	for _, o := range obs {
		sum += o
	}
	for _, b := range append(append([]float64{}, bounds...), math.Inf(1)) {
		n := 0.0
		for _, o := range obs {
			if o <= b {
				n++
			}
		}
		pts = append(pts, bucketPoint{b, n})
	}
	return pts, sum
}

var testBounds = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func TestQuantileKnownDistribution(t *testing.T) {
	// 1000 observations uniform in (0, 1.4]: p50 = 0.7 s, p99 = 1.386 s,
	// maximum 1.4 s; p99 lies in the (1, 2.5] bucket.
	obs := make([]float64, 1000)
	for i := range obs {
		obs[i] = 1.4 * float64(i+1) / 1000
	}
	pts, sum := hist(testBounds, obs)
	for _, s := range []float64{math.NaN(), sum} {
		p99 := quantileMS(pts, s, 0.99)
		if p99 <= 1000 || p99 > 2500 {
			t.Fatalf("p99 (sum %v) outside bucket (1,2.5]: %v", s, p99)
		}
	}
	p50 := quantileMS(pts, sum, 0.5)
	if math.Abs(p50-700) > 1 {
		t.Fatalf("p50 interpolation: %v", p50)
	}

	// Few observations above 1 s: the _sum bound pulls p99 toward the real
	// maximum (1.4 s) and keeps it inside its bucket.
	few := []float64{0.26, 0.26, 0.26, 0.26, 0.26, 0.26, 0.26, 0.26, 0.26, 1.4}
	pts, sum = hist(testBounds, few)
	raw := quantileMS(pts, math.NaN(), 0.99)
	if raw <= 1000 || raw > 2500 {
		t.Fatalf("few, no sum: %v", raw)
	}
	mx, ok := maxBound(pts, sum)
	if !ok || mx < 1.4 || mx > 1.4+9*0.01+1e-9 {
		t.Fatalf("max bound: %v %v", mx, ok)
	}
	q := quantileMS(pts, sum, 0.99)
	if q <= 1000 || q > mx*1000+1e-6 || q >= raw {
		t.Fatalf("few p99 must stay in (1,2.5] and <= bound %v (raw %v): %v", mx, raw, q)
	}
}

func TestQuantileEdges(t *testing.T) {
	// Zero count.
	if q := quantileMS([]bucketPoint{{1, 0}, {math.Inf(1), 0}}, 0, 0.99); q != 0 {
		t.Fatalf("zero count: %v", q)
	}
	// Only the +Inf bucket: no finite bound to report.
	if q := quantileMS([]bucketPoint{{math.Inf(1), 5}}, 10, 0.99); q != 0 {
		t.Fatalf("only +Inf: %v", q)
	}
	// Rank in +Inf: largest finite bound, even with a larger sum.
	if q := quantileMS([]bucketPoint{{1, 1}, {math.Inf(1), 10}}, 50, 0.99); q != 1000 {
		t.Fatalf("+Inf rank: %v", q)
	}
	// A single finite bucket interpolates from 0.
	if q := quantileMS([]bucketPoint{{2, 4}}, math.NaN(), 0.5); q != 1000 {
		t.Fatalf("single bucket: %v", q)
	}
	// Single bucket with a known sum caps at the maximum bound.
	if q := quantileMS([]bucketPoint{{2, 4}}, 0.4, 0.99); math.Abs(q-400) > 1e-9 {
		t.Fatalf("single bucket capped: %v", q)
	}
	// Empty bucket holding the rank returns its bound.
	if q := quantileMS([]bucketPoint{{1, 0}, {2, 5}}, math.NaN(), 0); q != 1000 {
		t.Fatalf("empty rank bucket: %v", q)
	}
	// Inconsistent sum (below the lower bounds) is ignored.
	if q := quantileMS([]bucketPoint{{1, 0}, {2, 10}}, 0.5, 0.99); math.Abs(q-1990) > 1e-6 {
		t.Fatalf("inconsistent sum: %v", q)
	}
	if _, ok := maxBound(nil, -1); ok {
		t.Fatal("negative sum must be unknown")
	}
}

func TestBuildStatsUsesSum(t *testing.T) {
	var b strings.Builder
	lbl := `driver="openai",model="m"`
	b.WriteString("# TYPE wwz_ttft_seconds histogram\n")
	for _, kv := range []struct {
		le string
		n  int
	}{{"1", 0}, {"2.5", 10}, {"+Inf", 10}} {
		fmt.Fprintf(&b, "wwz_ttft_seconds_bucket{%s,le=%q} %d\n", lbl, kv.le, kv.n)
	}
	// Ten observations in (1, 2.5] summing 10.4 s: maximum <= 1.4 s.
	fmt.Fprintf(&b, "wwz_ttft_seconds_sum{%s} 10.4\nwwz_ttft_seconds_count{%s} 10\n", lbl, lbl)
	// E2E without _sum keeps the plain interpolation.
	b.WriteString("# TYPE wwz_e2e_seconds histogram\n")
	fmt.Fprintf(&b, "wwz_e2e_seconds_bucket{%s,le=\"1\"} 0\nwwz_e2e_seconds_bucket{%s,le=\"2.5\"} 10\n", lbl, lbl)
	_, models := buildStats(parseExposition([]byte(b.String())))
	if len(models) != 1 {
		t.Fatalf("models: %+v", models)
	}
	if p := models[0].TTFTP99MS; p <= 1000 || p > 1400+1e-6 {
		t.Fatalf("ttft p99 capped by sum: %v", p)
	}
	if p := models[0].E2EP99MS; math.Abs(p-2485) > 1e-6 {
		t.Fatalf("e2e p99 without sum: %v", p)
	}
}

// TestStatsP99DefaultBuckets is the ADR 0050 case: E2E observations whose
// maximum is 1 412 ms give, with metrics.DefaultBuckets, p99 <= 1 500 ms in
// /admin/stats (the old (1, 2.5] s bucket gave ~2.4 s), never above the _sum
// bound.
func TestStatsP99DefaultBuckets(t *testing.T) {
	// 970 fast answers in (0.2, 0.8] s and a 30-request tail up to 1.412 s:
	// the p99 rank lands in the tail.
	obs := make([]float64, 0, 1000)
	for i := range 970 {
		obs = append(obs, 0.2+0.6*float64(i+1)/970)
	}
	for i := range 30 {
		obs = append(obs, 1.0+0.412*float64(i+1)/30)
	}
	stats := func(buckets []float64) (ModelStats, float64) {
		reg := metrics.New(10)
		h := reg.NewHistogram("wwz_e2e_seconds", "E2E", buckets, "driver", "model")
		for _, o := range obs {
			h.Observe(o, "openai", "m")
		}
		var buf bytes.Buffer
		reg.Write(&buf)
		_, models := buildStats(parseExposition(buf.Bytes()))
		if len(models) != 1 {
			t.Fatalf("models: %+v", models)
		}
		sum, _ := h.Sum("openai", "m")
		return models[0], sum
	}
	m, sum := stats(metrics.DefaultBuckets)
	if m.E2EP99MS <= 1000 || m.E2EP99MS > 1500 {
		t.Fatalf("p99 with default buckets = %v ms, want in (1000, 1500]", m.E2EP99MS)
	}
	pts, _ := hist(metrics.DefaultBuckets, obs)
	if mx, ok := maxBound(pts, sum); ok && m.E2EP99MS > mx*1000+1e-6 {
		t.Fatalf("p99 %v ms above the _sum bound %v s", m.E2EP99MS, mx)
	}
	if m.E2EP50MS <= 200 || m.E2EP50MS > 800 {
		t.Fatalf("p50 = %v ms", m.E2EP50MS)
	}
	// The pre-ADR 0050 buckets put p99 past 1.5 s: the test tells them apart.
	old, _ := stats([]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60})
	if old.E2EP99MS <= 1500 {
		t.Fatalf("old buckets p99 = %v ms, expected > 1500", old.E2EP99MS)
	}
}
