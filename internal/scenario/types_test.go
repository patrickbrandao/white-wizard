package scenario

import (
	"math"
	"testing"
)

func TestParseTokenRangeUint64Saturates(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{uint64(7), 7},
		{uint64(math.MaxUint64), math.MaxInt},
		{map[string]any{"tokens": uint64(12)}, 12},
		{map[string]any{"tokens": uint64(math.MaxUint64)}, math.MaxInt},
	}
	for _, c := range cases {
		got, err := parseTokenRange(c.in)
		if err != nil {
			t.Fatalf("parseTokenRange(%v): %v", c.in, err)
		}
		if got.Min != c.want || got.Max != c.want {
			t.Fatalf("parseTokenRange(%v) = %+v, want %d", c.in, got, c.want)
		}
	}
}
