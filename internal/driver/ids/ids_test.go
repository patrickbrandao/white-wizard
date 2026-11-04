package ids_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestFormats(t *testing.T) {
	now := time.Unix(1729282847, 0)
	cases := []struct {
		name string
		fn   func(*generate.RNG) string
		re   string
	}{
		{"chatcmpl", ids.OpenAIChat, `^chatcmpl-[0-9A-Za-z]{29}$`},
		{"cmpl", ids.OpenAICompletion, `^cmpl-[0-9A-Za-z]{29}$`},
		{"call", ids.OpenAICall, `^call_[0-9A-Za-z]{24}$`},
		{"openai req", ids.OpenAIRequest, `^req_[0-9a-f]{32}$`},
		{"msg", ids.AnthropicMessage, `^msg_011C[0-9A-Za-z]{20}$`},
		{"toolu", ids.AnthropicToolUse, `^toolu_01[0-9A-Za-z]{22}$`},
		{"anthropic req", ids.AnthropicRequest, `^req_011C[0-9A-Za-z]{20}$`},
		{"gen", func(r *generate.RNG) string { return ids.OpenRouterGen(r, now) }, `^gen-1729282847-[0-9A-Za-z]{20}$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re := regexp.MustCompile(tc.re)
			a := tc.fn(generate.NewRNG(42, "x"))
			if !re.MatchString(a) {
				t.Fatalf("%q does not match %s", a, tc.re)
			}
			if b := tc.fn(generate.NewRNG(42, "x")); a != b {
				t.Fatalf("not deterministic: %q vs %q", a, b)
			}
			if c := tc.fn(generate.NewRNG(43, "x")); a == c {
				t.Fatalf("seed ignored: %q", c)
			}
		})
	}
}

// A real id (task 235) has the same shape and length.
func TestAnthropicLengthMatchesReal(t *testing.T) {
	const real = "msg_011Cgabzfnu6w5BgM1qV3TQ6" // observed on 2026-10-31
	if got := ids.AnthropicMessage(generate.NewRNG(1, "x")); len(got) != len(real) {
		t.Fatalf("len %d, real %d", len(got), len(real))
	}
}

func TestFromToken(t *testing.T) {
	a := ids.OpenAIRequest(ids.FromToken("tok-1"))
	if a != ids.OpenAIRequest(ids.FromToken("tok-1")) {
		t.Fatal("same token must give the same id")
	}
	if a == ids.OpenAIRequest(ids.FromToken("tok-2")) {
		t.Fatal("different tokens must give different ids")
	}
}
