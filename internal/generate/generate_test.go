package generate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestPromptBreakdownAppendJSON: the x-wwz-input-breakdown form (I16) is
// compact, ordered and matches encoding/json field for field.
func TestPromptBreakdownAppendJSON(t *testing.T) {
	b := PromptBreakdown{System: 1, ToolSchemas: 2, User: 3, Assistant: 4, ToolCalls: 5, ToolResults: 6, Images: 7, Cached: 8, Total: 28}
	want := `{"system":1,"tool_schemas":2,"user":3,"assistant":4,"tool_calls":5,"tool_results":6,"images":7,"cached":8,"total":28}`
	if got := string(b.AppendJSON([]byte("x"))); got != "x"+want {
		t.Fatalf("got %s", got)
	}
	std, err := json.Marshal(b)
	if err != nil || string(std) != want {
		t.Fatalf("encoding/json %s (%v) differs from %s", std, err, want)
	}
	if CountPrompt(Simulated{}, &core.Request{Messages: []core.Message{{Role: "user", Content: "hi"}}}, 85).Cached != 0 {
		t.Fatal("CountPrompt must leave cached at 0")
	}
}

func TestSimulatedTokenizerRoundTrip(t *testing.T) {
	tk := Simulated{}
	for _, s := range []string{"Hello, world! This is a test.", "internationalization tokens", "a  b\nc", ""} {
		parts := tk.Split(s)
		if got := strings.Join(parts, ""); got != s {
			t.Fatalf("split lost bytes: %q -> %q", s, got)
		}
		if len(parts) != tk.Count(s) {
			t.Fatalf("count mismatch for %q: split=%d count=%d", s, len(parts), tk.Count(s))
		}
	}
}

func TestTextLength(t *testing.T) {
	g := NewGenerator(Simulated{}, nil)
	r := NewRNG(42, "x")
	for _, target := range []int{1, 10, 100, 1000} {
		txt := g.Text(r, target)
		n := g.Tokenizer.Count(txt)
		if n > target || n == 0 {
			t.Fatalf("target %d got %d", target, n)
		}
	}
}

func TestSampleOutputTokensHistogram(t *testing.T) {
	r := NewRNG(1, "h")
	d := &config.TokenDist{Dist: "histogram", Buckets: [][]float64{{50, 0.5}, {200, 0.5}}}
	for i := 0; i < 100; i++ {
		n := SampleOutputTokens(r, d, 120)
		if n < 1 || n > 120 {
			t.Fatalf("out of bounds: %d", n)
		}
	}
}

func TestReasoningEffort(t *testing.T) {
	r := NewRNG(1, "r")
	rt := &config.ReasoningTokens{Min: 100, Max: 100, Probability: 100}
	if n := SampleReasoningTokens(r, rt, "low", 0); n != 50 {
		t.Fatalf("low: %d", n)
	}
	if n := SampleReasoningTokens(r, rt, "high", 150); n != 150 {
		t.Fatalf("budget cap: %d", n)
	}
}
