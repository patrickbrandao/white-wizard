package scenario

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// TestResolverTokenizer: vars and steps render {{ text n }} with the
// model's tokenizer (ADR 0053); without one, the simulated tokenizer.
func TestResolverTokenizer(t *testing.T) {
	s := mustParse(t, "name: tk\nvars: {v: \"{{ text 30 }}\"}\nsteps:\n  - id: s\n    content: \"{{ .vars.v }}|{{ text 20 }}\"\n")
	for _, tk := range []generate.Tokenizer{tokenize.O200k, nil} {
		res, err := (&Resolver{Tokenizer: tk}).Resolve(Selection{Scenario: s}, toolReq(""), "c")
		if err != nil {
			t.Fatal(err)
		}
		count := (&Resolver{Tokenizer: tk}).tokenizer().Count
		v, step, ok := strings.Cut(res.Content, "|")
		if !ok || count(v) != 30 || count(step) != 20 {
			t.Fatalf("%T: %q (%d, %d tokens)", tk, res.Content, count(v), count(step))
		}
	}
}
