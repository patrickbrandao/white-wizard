package metrics

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGaugeFuncReadsAtExposition(t *testing.T) {
	r := New(10)
	var n atomic.Int64
	g := r.NewGaugeFunc("wwz_connections_open", "Open TCP connections", n.Load)
	if g.name() != "wwz_connections_open" || g.Value() != 0 {
		t.Fatalf("name %q value %d", g.name(), g.Value())
	}
	n.Store(3)
	var buf bytes.Buffer
	r.Write(&buf)
	out := buf.String()
	for _, want := range []string{"# TYPE wwz_connections_open gauge\n", "\nwwz_connections_open 3\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
