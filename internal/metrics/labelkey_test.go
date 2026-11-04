package metrics

import (
	"strings"
	"testing"
)

// TestLabelKeyFewerValues: fewer values than labels never panic (task 242);
// the missing ones are empty.
func TestLabelKeyFewerValues(t *testing.T) {
	r := New(0)
	c := r.NewCounter("c_few", "few", "a", "b")
	c.Inc("x")
	c.Inc()
	if c.Value("x", "") != 1 || c.Value("", "") != 1 {
		t.Fatal("missing label values should be empty")
	}
	if got := labelKey([]string{"a", "b"}, []string{"x"}); got != `a="x",b=""` {
		t.Fatalf("labelKey = %q", got)
	}
}

// TestLabelKeyEscapeAndLongKeys: values with backslash, quote and newline
// are escaped, keys larger than the stack buffer still find their series,
// and a lookup of an existing series allocates nothing (task 268).
func TestLabelKeyEscapeAndLongKeys(t *testing.T) {
	if got := labelKey([]string{"a"}, []string{"x\\y\"z\nw"}); got != `a="x\\y\"z\nw"` {
		t.Fatalf("labelKey = %q", got)
	}
	r := New(0)
	c := r.NewCounter("c_long", "long", "a")
	long := strings.Repeat("m", 2*keyBuf)
	c.Inc(long)
	c.Inc(long)
	if c.Value(long) != 2 {
		t.Fatalf("long key value = %d", c.Value(long))
	}
	g := r.NewGauge("g_esc", "esc", "a")
	g.Inc("q\"")
	h := r.NewHistogram("h_esc", "esc", nil, "a")
	h.Observe(0.5, "q\"")
	if _, n := h.Sum("q\""); n != 1 || g.Value("q\"") != 1 {
		t.Fatal("escaped series lookup failed")
	}
	short := long[:100]
	c.Inc(short)
	if allocs := testing.AllocsPerRun(100, func() { c.Inc(short); g.Inc("q\""); h.Observe(0.1, "q\"") }); allocs != 0 {
		t.Fatalf("existing series lookups allocate: %v", allocs)
	}
}
