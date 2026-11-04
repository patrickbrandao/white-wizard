package generate

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

func TestRNGRandomAndEdges(t *testing.T) {
	// Seed 0 means random: two generators differ (the parts are ignored).
	a, b := NewRNG(0, "x"), NewRNG(0, "x")
	same := true
	for i := 0; i < 8; i++ {
		if a.Uint64() != b.Uint64() {
			same = false
		}
	}
	if same {
		t.Fatal("seed 0 must be random")
	}
	// A fixed seed is reproducible, negative seeds included.
	if NewRNG(-5, "p").Uint64() != NewRNG(-5, "p").Uint64() {
		t.Fatal("fixed seed must be reproducible")
	}
	r := NewRNG(1, "c")
	if r.Chance(0) || r.Chance(-3) || !r.Chance(100) || !r.Chance(250) {
		t.Fatal("chance bounds")
	}
	hits := 0
	for i := 0; i < 2000; i++ {
		if r.Chance(50) {
			hits++
		}
	}
	if hits < 800 || hits > 1200 {
		t.Fatalf("Chance(50) hit %d of 2000", hits)
	}
	if got := r.Between(5, 5); got != 5 {
		t.Fatalf("Between(5,5) = %d", got)
	}
	if got := r.Between(9, 2); got != 9 {
		t.Fatalf("Between(9,2) = %d", got)
	}
	for i := 0; i < 50; i++ {
		if v := r.Between(2, 4); v < 2 || v > 4 {
			t.Fatalf("Between out of range: %d", v)
		}
	}
}

func TestGeneratorTexts(t *testing.T) {
	r := NewRNG(2, "g")
	g := NewGenerator(Simulated{}, []string{"only sentence here."})
	if len(g.Sentences) != 1 {
		t.Fatalf("custom sentences ignored: %v", g.Sentences)
	}
	if g.Text(r, 0) != "" || g.Lorem(r, -1) != "" || g.Reasoning(r, 0) != "" {
		t.Fatal("non-positive targets give empty text")
	}
	if got := NewGenerator(Simulated{}, nil).Sentences; len(got) != len(Sentences) {
		t.Fatal("default corpus expected")
	}
	if n := Simulated.Count(Simulated{}, g.Text(r, 10)); n != 10 {
		t.Fatalf("text tokens = %d", n)
	}
	lorem := g.Lorem(r, 30)
	if !strings.HasSuffix(lorem, ".") || strings.Count(lorem, " ") != 29 || lorem[0] < 'A' || lorem[0] > 'Z' {
		t.Fatalf("lorem = %q", lorem)
	}
	if !strings.Contains(lorem, ". ") {
		t.Fatalf("lorem must break sentences every 12 words: %q", lorem)
	}
	reasoning := g.Reasoning(r, 25)
	if n := (Simulated{}).Count(reasoning); n != 25 {
		t.Fatalf("reasoning tokens = %d", n)
	}
}

func TestSampleOutputTokensForms(t *testing.T) {
	r := NewRNG(3, "o")
	if got := SampleOutputTokens(r, nil, 0); got != 100 {
		t.Fatalf("default = %d", got)
	}
	if got := SampleOutputTokens(r, &config.TokenDist{Dist: "fixed", Value: 42}, 0); got != 42 {
		t.Fatalf("fixed = %d", got)
	}
	if got := SampleOutputTokens(r, &config.TokenDist{Dist: "fixed", Value: 42}, 10); got != 10 {
		t.Fatalf("max_tokens cap = %d", got)
	}
	if got := SampleOutputTokens(r, &config.TokenDist{Dist: "fixed", Value: -4}, 0); got != 1 {
		t.Fatalf("floor = %d", got)
	}
	for i := 0; i < 50; i++ {
		if got := SampleOutputTokens(r, &config.TokenDist{Dist: "uniform", Min: 5, Max: 8}, 0); got < 5 || got > 8 {
			t.Fatalf("uniform = %d", got)
		}
		if got := SampleOutputTokens(r, &config.TokenDist{Dist: "normal", Mean: 50, StdDev: 5}, 0); got < 1 {
			t.Fatalf("normal = %d", got)
		}
	}
	// Histogram: malformed buckets are skipped; the last bucket is the fallback.
	h := &config.TokenDist{Dist: "histogram", Buckets: [][]float64{{100, 1}, {7}, {200, 1}}}
	for i := 0; i < 100; i++ {
		if got := SampleOutputTokens(r, h, 0); got < 50 || got > 200 {
			t.Fatalf("histogram = %d", got)
		}
	}
	// No weight at all: x is 0 so the first well-formed bucket is taken.
	if got := SampleOutputTokens(r, &config.TokenDist{Buckets: [][]float64{{40, 0}}}, 0); got < 20 || got > 40 {
		t.Fatalf("zero-weight histogram = %d", got)
	}
	// Empty histogram keeps the default.
	if got := SampleOutputTokens(r, &config.TokenDist{}, 0); got != 100 {
		t.Fatalf("empty histogram = %d", got)
	}
}

func TestSampleReasoningTokensEffort(t *testing.T) {
	r := NewRNG(4, "r")
	if SampleReasoningTokens(r, nil, "high", 0) != 0 {
		t.Fatal("nil config means no reasoning")
	}
	if SampleReasoningTokens(r, &config.ReasoningTokens{Min: 10, Max: 10, Probability: 0}, "", 0) != 0 {
		t.Fatal("probability 0 means no reasoning")
	}
	rt := &config.ReasoningTokens{Min: 40, Max: 40, Probability: 100}
	for effort, want := range map[string]int{"": 40, "medium": 40, "minimal": 10, "low": 20, "high": 80, "xhigh": 120, "max": 120} {
		if got := SampleReasoningTokens(r, rt, effort, 0); got != want {
			t.Fatalf("effort %q = %d, want %d", effort, got, want)
		}
	}
	if got := SampleReasoningTokens(r, rt, "high", 30); got != 30 {
		t.Fatalf("budget cap = %d", got)
	}
	if got := SampleReasoningTokens(r, &config.ReasoningTokens{Min: 1, Max: 1, Probability: 100}, "minimal", 0); got != 1 {
		t.Fatalf("floor = %d", got)
	}
}

func TestCountPromptToolResultsOnUserTurn(t *testing.T) {
	req := &core.Request{Messages: []core.Message{
		{Role: "user", ToolResults: []core.ToolResult{{ToolCallID: "t1", Content: "three little words"}}},
	}}
	b := CountPrompt(Simulated{}, req, 0)
	if want := (Simulated{}).Count("three little words") + 6; b.ToolResults != want {
		t.Fatalf("tool results = %d, want %d", b.ToolResults, want)
	}
}

func TestWritePyStringInvalidUTF8(t *testing.T) {
	var b strings.Builder
	writePyString(&b, "a\xffb", true)
	if got := b.String(); got != `"a\ufffdb"` {
		t.Fatalf("got %q", got)
	}
}
