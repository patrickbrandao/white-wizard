package metrics

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCounterExpositionLabelledAndPlain(t *testing.T) {
	r := New(0)
	plain := r.NewCounter("c_plain", "plain")
	lab := r.NewCounter("c_lab", "labelled", "a", "b")
	plain.Inc()
	plain.Add(2)
	lab.Add(5, "x", `q"uo\te`+"\n")
	lab.Inc("x", "y")

	if plain.name() != "c_plain" || plain.Value() != 3 || plain.Total() != 3 {
		t.Fatalf("plain: %d", plain.Value())
	}
	if lab.Value("x", "y") != 1 || lab.Value("nope", "nope") != 0 || lab.Total() != 6 {
		t.Fatal("labelled values/total")
	}
	out := exposition(r)
	for _, want := range []string{
		"# TYPE c_plain counter\nc_plain 3\n",
		`c_lab{a="x",b="q\"uo\\te\n"} 5`,
		`c_lab{a="x",b="y"} 1`,
		"wwz_metrics_series_dropped_total 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGaugeOperationsAndExposition(t *testing.T) {
	r := New(0)
	plain := r.NewGauge("g_plain", "plain")
	lab := r.NewGauge("g_lab", "labelled", "k")

	plain.Set(10)
	plain.Add(5)
	plain.Inc()
	plain.Dec()
	plain.Dec()
	if plain.name() != "g_plain" || plain.Value() != 14 {
		t.Fatalf("plain value %d", plain.Value())
	}
	lab.Set(7, "a")
	lab.Inc("a")
	if lab.Value("a") != 8 || lab.Value("b") != 0 {
		t.Fatal("labelled gauge")
	}
	out := exposition(r)
	for _, want := range []string{"# TYPE g_plain gauge\ng_plain 14\n", `g_lab{k="a"} 8`, `g_lab{k="b"} 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGaugeFuncNameAndValue(t *testing.T) {
	r := New(0)
	g := r.NewGaugeFunc("gf", "func", func() int64 { return 42 })
	if g.name() != "gf" || g.Value() != 42 {
		t.Fatal("GaugeFunc name/value")
	}
}

func TestHistogramExpositionPlainAndLabelled(t *testing.T) {
	r := New(0)
	plain := r.NewHistogram("h_plain", "plain", []float64{1, 2})
	lab := r.NewHistogram("h_lab", "labelled", []float64{1, 2}, "k")
	if plain.name() != "h_plain" {
		t.Fatal("name")
	}
	plain.Observe(0.5)
	plain.Observe(5)
	lab.Observe(1.5, "a")
	out := exposition(r)
	for _, want := range []string{
		`h_plain_bucket{le="1"} 1`,
		`h_plain_bucket{le="2"} 1`,
		`h_plain_bucket{le="+Inf"} 2`,
		"h_plain_sum 5.500000\nh_plain_count 2\n",
		`h_lab_bucket{k="a",le="2"} 1`,
		`h_lab_sum{k="a"} 1.500000`,
		`h_lab_count{k="a"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if sum, n := plain.Sum(); sum != 5.5 || n != 2 {
		t.Fatalf("Sum %v %d", sum, n)
	}
	if sum, n := lab.Sum("missing"); sum != 0 || n != 0 {
		t.Fatalf("Sum of unknown series: %v %d", sum, n)
	}
}

func TestHistogramQuantile(t *testing.T) {
	r := New(0)
	h := r.NewHistogram("q", "q", []float64{1, 2, 4}, "k")

	if got := h.Quantile(0.5, "none"); got != 0 {
		t.Fatalf("unknown series: %v", got)
	}
	// A series with no observations (defensive branch) also yields 0.
	h.series[labelKey(h.labels, []string{"empty"})] = &histSeries{counts: make([]atomic.Int64, 4)}
	if got := h.Quantile(0.5, "empty"); got != 0 {
		t.Fatalf("empty series: %v", got)
	}

	for i := 0; i < 4; i++ {
		h.Observe(1.5, "a") // bucket (1,2]
	}
	// Interpolates inside the bucket: p50 -> 1 + (2-1)*0.5.
	if got := h.Quantile(0.5, "a"); math.Abs(got-1.5) > 1e-9 {
		t.Fatalf("p50 = %v", got)
	}
	// q=0 lands on the first bucket upper bound (it is empty).
	if got := h.Quantile(0, "a"); got != 1 {
		t.Fatalf("p0 = %v", got)
	}
	// Beyond the last bucket: +Inf.
	if got := h.Quantile(1.5, "a"); !math.IsInf(got, 1) {
		t.Fatalf("q>1 = %v", got)
	}
}

// A concurrent creator that loses the race must reuse the series the winner
// stored (double-checked locking in Gauge.get).
func TestGaugeGetReusesSeriesCreatedWhileWaiting(t *testing.T) {
	r := New(0)
	g := r.NewGauge("g", "g", "k")

	// Many goroutines race to create each new series; the losers must find the
	// winner's series after taking the write lock, so no increment is lost.
	const workers, keys = 32, 300
	var wg sync.WaitGroup
	for k := 0; k < keys; k++ {
		start := make(chan struct{})
		name := strconv.Itoa(k)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				g.Inc(name)
			}()
		}
		close(start)
	}
	wg.Wait()
	for k := 0; k < keys; k++ {
		if got := g.Value(strconv.Itoa(k)); got != workers {
			t.Fatalf("series %d = %d, want %d", k, got, workers)
		}
	}
}

func TestGaugeDropsBeyondMaxSeriesButStaysUsable(t *testing.T) {
	r := New(1)
	g := r.NewGauge("g", "g", "k")
	g.Set(1, "a")
	g.Set(9, "b") // dropped
	if g.Value("b") != 0 || g.Value("a") != 1 {
		t.Fatal("overflow series must not be stored")
	}
	if !strings.Contains(exposition(r), "wwz_metrics_series_dropped_total 2") {
		t.Fatalf("dropped counter:\n%s", exposition(r))
	}
}
