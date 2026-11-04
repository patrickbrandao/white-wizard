// Package generate produces invented text, reasoning and token counts.
package generate

import "math/rand/v2"

// RNG is a per-request random source. It is seeded deterministically from
// the global seed and the request identity so runs can be reproduced. The
// PCG source lives inside the struct, so a generator is one allocation
// (task 270).
type RNG struct {
	rand.Rand
	pcg rand.PCG
}

// FNV-64a, inlined so seeding hashes the parts without allocating.
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// NewRNG derives a generator from a global seed and any number of parts.
// A global seed of 0 means "random", in which case parts are ignored and
// the generator is seeded from the OS.
func NewRNG(globalSeed int64, parts ...string) *RNG {
	r := &RNG{}
	if globalSeed == 0 {
		r.pcg.Seed(rand.Uint64(), rand.Uint64())
	} else {
		// FNV-64a over every part followed by a NUL byte.
		h := uint64(fnvOffset64)
		for _, p := range parts {
			for i := 0; i < len(p); i++ {
				h ^= uint64(p[i])
				h *= fnvPrime64
			}
			h *= fnvPrime64 // ^= 0 is a no-op
		}
		// The seed bits are reinterpreted as-is; negative seeds are valid and
		// must keep producing the same sequences they always did.
		r.pcg.Seed(uint64(globalSeed), h) //nolint:gosec // G115: intentional two's-complement reinterpretation of the seed
	}
	r.Rand = *rand.New(&r.pcg)
	return r
}

// Chance returns true with probability pct (0..100).
func (r *RNG) Chance(pct float64) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return r.Float64()*100 < pct
}

// Between returns an int in [lo, hi].
func (r *RNG) Between(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + r.IntN(hi-lo+1)
}
