package latency

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestNormalStdDevClamped(t *testing.T) {
	r := generate.NewRNG(5, "norm")
	d := &config.Dist{Kind: "normal", Mean: 100 * time.Millisecond, StdDev: time.Second}
	for i := 0; i < 200; i++ {
		if v := Sample(d, r); v < 30*time.Millisecond || v > 170*time.Millisecond {
			t.Fatalf("normal out of clamp: %v", v)
		}
	}
}
