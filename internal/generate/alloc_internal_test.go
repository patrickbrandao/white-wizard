package generate

import (
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// oldRNG is NewRNG as it was before task 270 (hash/fnv and a separate
// PCG): the sequences must not change.
func oldRNG(seed int64, parts ...string) *rand.Rand {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return rand.New(rand.NewPCG(uint64(seed), h.Sum64())) //nolint:gosec // same reinterpretation as NewRNG
}

func TestNewRNGSameSequences(t *testing.T) {
	cases := []struct {
		seed  int64
		parts []string
	}{
		{1, nil},
		{42, []string{"req", "abc", "conv-1"}},
		{-7, []string{"", "ids", "\x00x"}},
		{1, []string{"request-id", "token-ç"}},
	}
	for _, c := range cases {
		got, want := NewRNG(c.seed, c.parts...), oldRNG(c.seed, c.parts...)
		for i := range 50 {
			if g, w := got.Uint64(), want.Uint64(); g != w {
				t.Fatalf("seed %d parts %q draw %d: %d, want %d", c.seed, c.parts, i, g, w)
			}
		}
	}
	// Seed 0 is random and still usable.
	a, b := NewRNG(0, "x"), NewRNG(0, "x")
	if a1, b1, a2, b2 := a.Uint64(), b.Uint64(), a.Uint64(), b.Uint64(); a1 == b1 && a2 == b2 {
		t.Fatal("seed 0 should not be reproducible")
	}
}

func TestNewRNGOneAllocation(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() { _ = NewRNG(42, "req", "abc", "conv") }); n != 1 {
		t.Fatalf("NewRNG allocs = %v, want 1", n)
	}
}

func TestSplitFastPathMatchesRunes(t *testing.T) {
	texts := append(slices.Clone(Sentences), ReasoningPhrases...)
	texts = append(texts, "a", " ", "abcdefghijklmno", "ação  é  ótima!!", "x\ty\nz", "日本語のテキスト", "a1b2c3d4e5f6g7")
	for _, s := range texts {
		got, want := (Simulated{}).Split(s), splitRunes(s)
		if !slices.Equal(got, want) {
			t.Fatalf("Split(%q) = %q, want %q", s, got, want)
		}
		if strings.Join(got, "") != s {
			t.Fatalf("Split(%q) does not join back", s)
		}
	}
	// Invalid UTF-8 keeps the rune path (U+FFFD for the bad byte).
	if got := (Simulated{}).Split("a\xffb"); strings.Join(got, "") != "a�b" {
		t.Fatalf("invalid UTF-8: %q", got)
	}
}

// joinTok is a tokenizer whose tokens are not the text (a space each),
// so TrimToTokens must join them.
type joinTok struct{}

func (joinTok) Count(text string) int { return len(text) }
func (joinTok) Split(text string) []string {
	out := make([]string, len(text))
	for i := range out {
		out[i] = "-"
	}
	return out
}

// subTok splits byte by byte: its tokens are the text, so the cut is a
// prefix.
type subTok struct{}

func (subTok) Count(text string) int { return len(text) }
func (subTok) Split(text string) []string {
	out := make([]string, len(text))
	for i := range out {
		out[i] = text[i : i+1]
	}
	return out
}

func TestTrimToTokens(t *testing.T) {
	sim := Simulated{}
	text := strings.Join(Sentences[:4], " ")
	for _, max := range []int{0, 1, 5, 17, 1000} {
		toks := sim.Split(text)
		want := text
		if len(toks) > max {
			want = strings.Join(toks[:max], "")
		}
		if got := TrimToTokens(sim, text, max); got != want {
			t.Fatalf("max %d: %q, want %q", max, got, want)
		}
	}
	if got := TrimToTokens(sim, "ab\xffcd ef", 2); got != "ab�" {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if got := TrimToTokens(subTok{}, "abcdef", 3); got != "abc" {
		t.Fatalf("prefix: %q", got)
	}
	if got := TrimToTokens(subTok{}, "abc", 3); got != "abc" {
		t.Fatalf("short: %q", got)
	}
	if got := TrimToTokens(joinTok{}, "abcdef", 3); got != "---" {
		t.Fatalf("join: %q", got)
	}
	if _, ok := prefixOf("ab", []string{"abc"}); ok {
		t.Fatal("prefixOf past the end")
	}
}
