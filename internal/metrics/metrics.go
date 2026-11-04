// Package metrics is a small dependency-free Prometheus text exposition:
// counters, gauges and histograms with labels, bounded in cardinality.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultBuckets for latency histograms (seconds): the 1-1.5-2-3-5-7.5 series
// per decade from 1 ms to 10 s plus a 15-60 s tail (ADR 0050), so a
// percentile estimated from the buckets is off by at most ~67 %.
var DefaultBuckets = []float64{
	0.001, 0.0015, 0.002, 0.003, 0.005, 0.0075,
	0.01, 0.015, 0.02, 0.03, 0.05, 0.075,
	0.1, 0.15, 0.2, 0.3, 0.5, 0.75,
	1, 1.5, 2, 3, 5, 7.5,
	10, 15, 20, 30, 60,
}

// Registry holds all metrics.
type Registry struct {
	mu        sync.RWMutex
	metrics   []metric
	maxSeries int
	dropped   atomic.Int64
}

type metric interface {
	write(w io.Writer)
	name() string
}

// New creates a registry; maxSeries bounds label combinations per metric.
func New(maxSeries int) *Registry {
	if maxSeries <= 0 {
		maxSeries = 5000
	}
	return &Registry{maxSeries: maxSeries}
}

func labelKey(labels []string, values []string) string {
	var buf [keyBuf]byte
	return string(appendLabelKey(buf[:0], labels, values))
}

// keyBuf is the stack buffer of a series lookup: keys up to this size cost
// no allocation unless the series is new (task 268).
const keyBuf = 256

// appendLabelKey appends the series key of the label values to dst. Lookups
// index the map with string(key), which Go does without allocating.
func appendLabelKey(dst []byte, labels []string, values []string) []byte {
	for i := range labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, labels[i]...)
		dst = append(dst, '=', '"')
		v := ""
		if i < len(values) {
			v = values[i] // fewer values than labels: the missing ones are empty
		}
		dst = appendEscaped(dst, v)
		dst = append(dst, '"')
	}
	return dst
}

// appendEscaped appends s with backslash, quote and newline escaped as the
// exposition format requires.
func appendEscaped(dst []byte, s string) []byte {
	if !strings.ContainsAny(s, "\\\"\n") {
		return append(dst, s...)
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '"':
			dst = append(dst, '\\', '"')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// Counter is a labelled counter.
type Counter struct {
	reg    *Registry
	nm     string
	help   string
	labels []string
	mu     sync.RWMutex
	series map[string]*atomic.Int64
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{reg: r, nm: name, help: help, labels: labels, series: map[string]*atomic.Int64{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, c)
	r.mu.Unlock()
	return c
}

func (c *Counter) name() string { return c.nm }

// Add increments by n for the label values.
func (c *Counter) Add(n int64, values ...string) {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], c.labels, values)
	c.mu.RLock()
	v, ok := c.series[string(k)]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		v, ok = c.series[string(k)]
		if !ok {
			if len(c.series) >= c.reg.maxSeries {
				c.mu.Unlock()
				c.reg.dropped.Add(1)
				return
			}
			v = new(atomic.Int64)
			c.series[string(k)] = v
		}
		c.mu.Unlock()
	}
	v.Add(n)
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

func (c *Counter) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.nm, c.help, c.nm)
	c.mu.RLock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			fmt.Fprintf(w, "%s %d\n", c.nm, c.series[k].Load())
		} else {
			fmt.Fprintf(w, "%s{%s} %d\n", c.nm, k, c.series[k].Load())
		}
	}
	c.mu.RUnlock()
}

// Gauge is a labelled gauge.
type Gauge struct {
	reg    *Registry
	nm     string
	help   string
	labels []string
	mu     sync.RWMutex
	series map[string]*atomic.Int64
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{reg: r, nm: name, help: help, labels: labels, series: map[string]*atomic.Int64{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, g)
	r.mu.Unlock()
	return g
}

func (g *Gauge) name() string { return g.nm }

func (g *Gauge) get(values []string) *atomic.Int64 {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], g.labels, values)
	g.mu.RLock()
	v, ok := g.series[string(k)]
	g.mu.RUnlock()
	if ok {
		return v
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok = g.series[string(k)]; ok {
		return v
	}
	if len(g.series) >= g.reg.maxSeries {
		g.reg.dropped.Add(1)
		return new(atomic.Int64)
	}
	v = new(atomic.Int64)
	g.series[string(k)] = v
	return v
}

// Set, Add, Inc, Dec manipulate the gauge.
func (g *Gauge) Set(n int64, values ...string) { g.get(values).Store(n) }
func (g *Gauge) Add(n int64, values ...string) { g.get(values).Add(n) }
func (g *Gauge) Inc(values ...string)          { g.get(values).Add(1) }
func (g *Gauge) Dec(values ...string)          { g.get(values).Add(-1) }
func (g *Gauge) Value(values ...string) int64  { return g.get(values).Load() }

func (g *Gauge) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.nm, g.help, g.nm)
	g.mu.RLock()
	keys := make([]string, 0, len(g.series))
	for k := range g.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			fmt.Fprintf(w, "%s %d\n", g.nm, g.series[k].Load())
		} else {
			fmt.Fprintf(w, "%s{%s} %d\n", g.nm, k, g.series[k].Load())
		}
	}
	g.mu.RUnlock()
}

// GaugeFunc is an unlabelled gauge whose value is read from fn at every
// exposition (for values another component already counts, such as the
// open connections of the HTTP server).
type GaugeFunc struct {
	nm   string
	help string
	fn   func() int64
}

// NewGaugeFunc registers a gauge read from fn when /metrics is rendered.
func (r *Registry) NewGaugeFunc(name, help string, fn func() int64) *GaugeFunc {
	g := &GaugeFunc{nm: name, help: help, fn: fn}
	r.mu.Lock()
	r.metrics = append(r.metrics, g)
	r.mu.Unlock()
	return g
}

func (g *GaugeFunc) name() string { return g.nm }

// Value calls fn.
func (g *GaugeFunc) Value() int64 { return g.fn() }

func (g *GaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.nm, g.help, g.nm, g.nm, g.fn())
}

// Histogram is a labelled histogram with fixed buckets.
type Histogram struct {
	reg     *Registry
	nm      string
	help    string
	labels  []string
	buckets []float64
	mu      sync.RWMutex
	series  map[string]*histSeries
}

type histSeries struct {
	counts []atomic.Int64
	sum    atomic.Int64 // in micro-units of the value (value*1e6)
	count  atomic.Int64
}

// NewHistogram registers a histogram.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	h := &Histogram{reg: r, nm: name, help: help, labels: labels, buckets: buckets, series: map[string]*histSeries{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, h)
	r.mu.Unlock()
	return h
}

func (h *Histogram) name() string { return h.nm }

// Observe records a value.
func (h *Histogram) Observe(v float64, values ...string) {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], h.labels, values)
	h.mu.RLock()
	s, ok := h.series[string(k)]
	h.mu.RUnlock()
	if !ok {
		h.mu.Lock()
		s, ok = h.series[string(k)]
		if !ok {
			if len(h.series) >= h.reg.maxSeries {
				h.mu.Unlock()
				h.reg.dropped.Add(1)
				return
			}
			s = &histSeries{counts: make([]atomic.Int64, len(h.buckets)+1)}
			h.series[string(k)] = s
		}
		h.mu.Unlock()
	}
	idx := sort.SearchFloat64s(h.buckets, v)
	s.counts[idx].Add(1)
	s.sum.Add(int64(v * 1e6))
	s.count.Add(1)
}

func (h *Histogram) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.nm, h.help, h.nm)
	h.mu.RLock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		cum := int64(0)
		sep := ""
		if k != "" {
			sep = k + ","
		}
		for i, b := range h.buckets {
			cum += s.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{%sle=\"%s\"} %d\n", h.nm, sep, strconv.FormatFloat(b, 'g', -1, 64), cum)
		}
		cum += s.counts[len(h.buckets)].Load()
		fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", h.nm, sep, cum)
		if k == "" {
			fmt.Fprintf(w, "%s_sum %s\n%s_count %d\n", h.nm, strconv.FormatFloat(float64(s.sum.Load())/1e6, 'f', 6, 64), h.nm, s.count.Load())
		} else {
			fmt.Fprintf(w, "%s_sum{%s} %s\n%s_count{%s} %d\n", h.nm, k, strconv.FormatFloat(float64(s.sum.Load())/1e6, 'f', 6, 64), h.nm, k, s.count.Load())
		}
	}
	h.mu.RUnlock()
}

// Write renders the exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.RLock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.RUnlock()
	for _, m := range ms {
		m.write(w)
	}
	fmt.Fprintf(w, "# HELP wwz_metrics_series_dropped_total Series dropped because max_series was reached\n# TYPE wwz_metrics_series_dropped_total counter\nwwz_metrics_series_dropped_total %d\n", r.dropped.Load())
}

// Quantile estimates a quantile from histogram buckets for one label set
// (used by /admin/stats; linear interpolation like Prometheus histogram_quantile).
func (h *Histogram) Quantile(q float64, values ...string) float64 {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], h.labels, values)
	h.mu.RLock()
	s, ok := h.series[string(k)]
	h.mu.RUnlock()
	if !ok {
		return 0
	}
	total := s.count.Load()
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	cum := int64(0)
	prevBound := 0.0
	for i, b := range h.buckets {
		c := s.counts[i].Load()
		if float64(cum+c) >= target {
			if c == 0 {
				return b
			}
			frac := (target - float64(cum)) / float64(c)
			return prevBound + (b-prevBound)*frac
		}
		cum += c
		prevBound = b
	}
	return math.Inf(1)
}

// Sum returns the sum and count for one label set.
func (h *Histogram) Sum(values ...string) (float64, int64) {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], h.labels, values)
	h.mu.RLock()
	s, ok := h.series[string(k)]
	h.mu.RUnlock()
	if !ok {
		return 0, 0
	}
	return float64(s.sum.Load()) / 1e6, s.count.Load()
}

// CounterValue returns the value of a counter series.
func (c *Counter) Value(values ...string) int64 {
	var buf [keyBuf]byte
	k := appendLabelKey(buf[:0], c.labels, values)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.series[string(k)]; ok {
		return v.Load()
	}
	return 0
}

// Total sums all series of a counter.
func (c *Counter) Total() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var t int64
	for _, v := range c.series {
		t += v.Load()
	}
	return t
}
