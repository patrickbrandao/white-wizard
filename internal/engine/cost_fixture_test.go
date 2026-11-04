package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// The cost of every priced answer recorded from OpenRouter on 2026-10-31
// (testdata/real/openrouter/shape/ and responses/): the engine's pico-dollar
// split, written as JSON, is the real cost, prompt and completion figures
// (task 246). A whole micro-dollar would make 13 tokens at $0.10/M cost
// 0.000001 instead of 0.0000013.

// fixturePrices are the list prices of the fixtures' models (USD per
// million tokens) on the recording day.
var fixturePrices = map[string]config.Pricing{
	"openai/gpt-4.1-nano":        {InputPerM: 0.1, OutputPerM: 0.4},
	"openai/gpt-4o-mini":         {InputPerM: 0.15, OutputPerM: 0.6},
	"anthropic/claude-haiku-4.5": {InputPerM: 1, OutputPerM: 5},
}

var (
	reFxModel      = regexp.MustCompile(`"model":"([^"]+)"`)
	reFxPrompt     = regexp.MustCompile(`"(?:prompt_tokens|input_tokens)":(\d+)`)
	reFxCompletion = regexp.MustCompile(`"(?:completion_tokens|output_tokens)":(\d+)`)
	reFxCost       = regexp.MustCompile(`"cost":([0-9.e+-]+)`)
	reFxPromptCost = regexp.MustCompile(`"upstream_inference_(?:prompt|input)_cost":([0-9.e+-]+)`)
	reFxComplCost  = regexp.MustCompile(`"upstream_inference_(?:completions|output)_cost":([0-9.e+-]+)`)
)

// lastMatch is the group of the last match of re in s ("" without one).
func lastMatch(re *regexp.Regexp, s string) string {
	m := re.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

func dollarsJSON(t *testing.T, pico int64) string {
	t.Helper()
	b, err := json.Marshal(float64(pico) / 1e12)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCostMatchesOpenRouterFixtures(t *testing.T) {
	checked := 0
	for _, dir := range []string{"shape", "responses"} {
		files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "openrouter", dir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx struct {
				Response struct {
					Status int    `json:"status"`
					Body   string `json:"body"`
				} `json:"response"`
			}
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			body := fx.Response.Body
			cost := lastMatch(reFxCost, body)
			if fx.Response.Status != 200 || cost == "" {
				continue
			}
			name := dir + "/" + filepath.Base(f)
			model := lastMatch(reFxModel, body)
			p, ok := fixturePrices[model]
			if !ok {
				t.Fatalf("%s: no price for %q", name, model)
			}
			in, _ := strconv.Atoi(lastMatch(reFxPrompt, body))
			out, _ := strconv.Atoi(lastMatch(reFxCompletion, body))
			u := core.Usage{PromptTokens: in, CompletionTokens: out}
			setCost(p, &u)
			for _, c := range []struct {
				field string
				pico  int64
				real  string
			}{
				{"cost", u.PromptCostPicoUSD + u.CompletionCostPicoUSD, cost},
				{"prompt", u.PromptCostPicoUSD, lastMatch(reFxPromptCost, body)},
				{"completion", u.CompletionCostPicoUSD, lastMatch(reFxComplCost, body)},
			} {
				if got := dollarsJSON(t, c.pico); got != c.real {
					t.Errorf("%s (%s, %d+%d tokens): %s %s, real %s", name, model, in, out, c.field, got, c.real)
				}
			}
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("only %d priced fixtures", checked)
	}
}

// TestCostPicoSplit: cache reads and writes go to the prompt part,
// reasoning (inside the completion tokens) to the completion part, and the
// whole micro-dollars are the truncated sum.
func TestCostPicoSplit(t *testing.T) {
	p := config.Pricing{InputPerM: 1, OutputPerM: 5, CacheReadPerM: 0.1, CacheWritePerM: 1.25}
	u := core.Usage{PromptTokens: 1000, CachedTokens: 100, CacheWriteTokens: 300, CacheWrite1hTokens: 100, CompletionTokens: 10, ReasoningTokens: 4}
	setCost(p, &u)
	if u.PromptCostPicoUSD != (600+10+250+200)*1e6 || u.CompletionCostPicoUSD != 50*1e6 || u.CostMicroUSD != 1110 {
		t.Fatalf("split %+v", u)
	}
	u = core.Usage{PromptTokens: 3, CompletionTokens: 1}
	setCost(config.Pricing{InputPerM: 0.1, OutputPerM: 0.4}, &u)
	if u.PromptCostPicoUSD != 300_000 || u.CompletionCostPicoUSD != 400_000 || u.CostMicroUSD != 0 {
		t.Fatalf("sub-micro %+v", u)
	}
}
