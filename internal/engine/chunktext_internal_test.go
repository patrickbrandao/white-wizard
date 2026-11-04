package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// oldChunkText is chunkText before task 270 (a copy per chunk): the
// substrings must be the same chunks.
func oldChunkText(tk generate.Tokenizer, text string, tokens, runes int) []string {
	if text == "" {
		return nil
	}
	if runes > 0 {
		rs := []rune(text)
		var out []string
		for i := 0; i < len(rs); i += runes {
			out = append(out, string(rs[i:min(i+runes, len(rs))]))
		}
		return out
	}
	if tokens <= 0 {
		tokens = 1
	}
	var out []string
	var b strings.Builder
	for i, p := range tk.Split(text) {
		b.WriteString(p)
		if (i+1)%tokens == 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// dashTok's tokens are not substrings of the text.
type dashTok struct{}

func (dashTok) Count(text string) int { return len(text) }
func (dashTok) Split(text string) []string {
	return slices.Repeat([]string{"-"}, len(text))
}

func TestChunkTextSameChunks(t *testing.T) {
	texts := []string{
		"", "a", strings.Join(generate.Sentences[:3], " "), "ação é ótima, 日本語!", "a\xffb c\xfe",
	}
	tks := []generate.Tokenizer{generate.Simulated{}, dashTok{}}
	for _, tk := range tks {
		for _, s := range texts {
			for _, tokens := range []int{0, 1, 3, 7} {
				for _, runes := range []int{0, 1, 4} {
					got, want := chunkText(tk, s, tokens, runes), oldChunkText(tk, s, tokens, runes)
					if !slices.Equal(got, want) {
						t.Fatalf("%T %q tokens %d runes %d: %q, want %q", tk, s, tokens, runes, got, want)
					}
				}
			}
		}
	}
}

func TestContiguous(t *testing.T) {
	if !contiguous("abc", []string{"a", "bc"}) {
		t.Fatal("a+bc is abc")
	}
	if contiguous("abc", []string{"a", "b"}) {
		t.Fatal("a+b is not abc")
	}
	if contiguous("ab", []string{"a", "bc"}) {
		t.Fatal("past the end")
	}
	if contiguous("abc", []string{"x", "bc"}) {
		t.Fatal("different bytes")
	}
}

func TestChunkTextNoCopy(t *testing.T) {
	s := strings.Join(generate.Sentences[:3], " ")
	if n := testing.AllocsPerRun(50, func() { _ = chunkText(generate.Simulated{}, s, 2, 0) }); n != 2 {
		t.Fatalf("chunkText allocs = %v, want 2 (token list and chunk list)", n)
	}
}
