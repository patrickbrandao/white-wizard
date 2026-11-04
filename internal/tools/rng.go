package tools

import (
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// newRNG derives a deterministic generator from a seed and labels.
func newRNG(seed int64, parts ...string) *rand.Rand {
	h1 := fnv.New64a()
	h2 := fnv.New64()
	for _, s := range append([]string{strconv.FormatInt(seed, 10)}, parts...) {
		_, _ = h1.Write([]byte(s))
		_, _ = h1.Write([]byte{0})
		_, _ = h2.Write([]byte(s))
		_, _ = h2.Write([]byte{0})
	}
	return rand.New(rand.NewPCG(h1.Sum64(), h2.Sum64()))
}

// randomSeed returns a non-zero seed for seed 0 ("random").
func randomSeed() int64 {
	return rand.Int64N(1<<62) + 1
}

func drawLatency(r *rand.Rand, rg Range) time.Duration {
	if rg.Max <= rg.Min {
		return rg.Min
	}
	return rg.Min + time.Duration(r.Int64N(int64(rg.Max-rg.Min)+1))
}

// drawOutcome draws timeout, then error, then invalid_payload; "" = run.
func drawOutcome(r *rand.Rand, rt Rates) Outcome {
	u := r.Float64() * 100
	switch {
	case u < rt.Timeout:
		return OutcomeTimeout
	case u < rt.Timeout+rt.Error:
		return OutcomeError
	case u < rt.Timeout+rt.Error+rt.InvalidPayload:
		return OutcomeInvalidPayload
	}
	return ""
}

var lorem = strings.Fields(`lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod
tempor incididunt ut labore et dolore magna aliqua enim ad minim veniam quis nostrud
exercitation ullamco laboris nisi aliquip ex ea commodo consequat duis aute irure in
reprehenderit voluptate velit esse cillum fugiat nulla pariatur excepteur sint occaecat
cupidatat non proident sunt culpa qui officia deserunt mollit anim id est laborum`)

func words(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(lorem[r.IntN(len(lorem))])
	}
	return b.String()
}

// sentence is n lorem words, capitalised, ending with a period.
func sentence(r *rand.Rand, n int) string {
	return capitalize(words(r, n)) + "."
}

func paragraph(r *rand.Rand) string {
	n := 2 + r.IntN(4)
	ss := make([]string, n)
	for i := range ss {
		ss[i] = sentence(r, 6+r.IntN(10))
	}
	return strings.Join(ss, " ")
}

func capitalize(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}
