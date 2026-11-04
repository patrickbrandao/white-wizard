package metrics

import "testing"

// BenchmarkCounterInc: one increment of an existing series, the hot path of
// every request (task 268).
func BenchmarkCounterInc(b *testing.B) {
	c := New(0).NewCounter("c", "c", "driver", "model", "endpoint", "status")
	b.ReportAllocs()
	for range b.N {
		c.Inc("openai", "wwz-fast", "chat", "200")
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := New(0).NewHistogram("h", "h", nil, "driver", "model")
	b.ReportAllocs()
	for range b.N {
		h.Observe(0.01, "openai", "wwz-fast")
	}
}

func BenchmarkGaugeInc(b *testing.B) {
	g := New(0).NewGauge("g", "g", "driver")
	b.ReportAllocs()
	for range b.N {
		g.Inc("openai")
	}
}
