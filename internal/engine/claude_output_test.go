package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestClaudeOutputScale (task 342, spec 007): a Claude 4.7 or later on
// Anthropic's dialect reports output_tokens as its tokenizer counts the
// text, ~32% more than Claude 4.5 counts the same text, so the same token
// budget is less text; a Claude 4.5 is unchanged.
func TestClaudeOutputScale(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {output_tokens: {dist: fixed, value: 300}}
models:
  - {id: claude-opus-5-5, latency: fast, generation: fixed}
  - {id: claude-haiku-4-5, latency: fast, generation: fixed}
keys:
  - {key: k-claude, label: claude}
`)
	for model, ratio := range map[string][2]float64{"claude-opus-5-5": {0.74, 0.78}, "claude-haiku-4-5": {1, 1}} {
		w := call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "k-claude", "anthropic-version": "2023-06-01"},
			`{"model":"`+model+`","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`)
		var out struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(w.Body(), &out); err != nil || w.StatusCode() != 200 || len(out.Content) == 0 {
			t.Fatalf("%s: %d %s", model, w.StatusCode(), w.Body())
		}
		if out.Usage.Output != 300 {
			t.Errorf("%s: output_tokens %d, want 300", model, out.Usage.Output)
		}
		sim := float64(generate.Simulated{}.Count(out.Content[0].Text)) / 300
		if sim < ratio[0] || sim > ratio[1] {
			t.Errorf("%s: the text counts %.3f of output_tokens on Claude 4.5, want %v", model, sim, ratio)
		}
	}
}
