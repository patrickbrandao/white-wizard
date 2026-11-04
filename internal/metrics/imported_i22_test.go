package metrics

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// I22: teto de séries de métricas (metrics.max_series) — anti-DoS de
// memória: label combinations beyond the cap are dropped and counted.

func exposition(r *Registry) string {
	var b bytes.Buffer
	r.Write(&b)
	return b.String()
}

// TestI22MaxSeriesCounter: I22 — a counter keeps at most max_series label
// combinations; extra ones are dropped and counted, existing ones still move.
func TestI22MaxSeriesCounter(t *testing.T) {
	r := New(3)
	c := r.NewCounter("wwz_t_total", "t", "key")
	for i := 0; i < 10; i++ {
		c.Inc("k" + strconv.Itoa(i))
	}
	c.Inc("k0")
	out := exposition(r)
	if n := strings.Count(out, "wwz_t_total{"); n != 3 {
		t.Fatalf("%d series, want 3:\n%s", n, out)
	}
	if c.Value("k0") != 2 || c.Total() != 4 {
		t.Fatalf("k0 %d total %d", c.Value("k0"), c.Total())
	}
	if !strings.Contains(out, "wwz_metrics_series_dropped_total 7\n") {
		t.Fatalf("dropped counter missing or wrong:\n%s", out)
	}
}

// TestI22MaxSeriesGaugeHistogram: I22 — gauges and histograms obey the same cap.
func TestI22MaxSeriesGaugeHistogram(t *testing.T) {
	r := New(2)
	g := r.NewGauge("wwz_g", "g", "model")
	h := r.NewHistogram("wwz_h", "h", []float64{1}, "model")
	for i := 0; i < 5; i++ {
		m := "m" + strconv.Itoa(i)
		g.Inc(m)
		h.Observe(0.5, m)
	}
	out := exposition(r)
	if n := strings.Count(out, "wwz_g{"); n != 2 {
		t.Fatalf("gauge series %d:\n%s", n, out)
	}
	if n := strings.Count(out, "wwz_h_count{"); n != 2 {
		t.Fatalf("histogram series %d:\n%s", n, out)
	}
	if strings.Contains(out, "wwz_metrics_series_dropped_total 0\n") {
		t.Fatalf("drops not counted:\n%s", out)
	}
}

// TestI22MaxSeriesDefault: I22 — a non-positive cap falls back to 5000.
func TestI22MaxSeriesDefault(t *testing.T) {
	if r := New(0); r.maxSeries != 5000 {
		t.Fatalf("default %d", r.maxSeries)
	}
}
