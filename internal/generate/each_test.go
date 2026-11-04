package generate

import (
	"strings"
	"testing"
)

type splitOnly struct{}

func (splitOnly) Count(s string) int      { return len(Simulated{}.Split(s)) }
func (splitOnly) Split(s string) []string { return Simulated{}.Split(s) }

// TestEachMatchesSplit: Each yields exactly the tokens of Split (the prompt
// cache hashes them, spec 007); EachToken falls back to Split.
func TestEachMatchesSplit(t *testing.T) {
	tk := Simulated{}
	for _, s := range []string{"", "a", "  hello,  world!! ", "abcdefghijklmn op", "ünïcødé 日本語 x1y2z3w4v5", "a\tb\nc", "tok-en.ize?"} {
		var got []string
		EachToken(tk, s, func(tok string) { got = append(got, tok) })
		if want := tk.Split(s); strings.Join(got, "|") != strings.Join(want, "|") || len(got) != tk.Count(s) {
			t.Errorf("%q: Each %q, Split %q, Count %d", s, got, want, tk.Count(s))
		}
	}
	var got []string
	EachToken(splitOnly{}, "a b", func(tok string) { got = append(got, tok) })
	if strings.Join(got, "|") != "a| b" {
		t.Errorf("Split fallback: %q", got)
	}
}
