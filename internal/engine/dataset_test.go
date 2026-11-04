package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Generation mode dataset (spec 007, ADR 0068, task 381).

const datasetCfg = `
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    ds: {mode: dataset, dataset_path: testdata/dataset.jsonl}
models:
  - {id: ds-model, latency: fast, generation: ds}
  - {id: ds-inline, latency: fast, generation: {mode: dataset, dataset_path: testdata/dataset.jsonl}}
keys:
  - {key: k-audit, label: audit}
`

const datasetStory = "Once upon a time, in a quiet village by the sea, an old fisherman found a bottle with a map inside. He followed it across the hills, through the forest and over the river, until he reached a cave full of light."

var datasetResponses = []string{"Paris is the capital of France.", datasetStory, "Blue.", "An apple.", "A fox."}

// storyMessages is the story entry's history with a system prompt and
// different whitespace: both are normalized away.
const storyMessages = `[{"role":"system","content":"be nice"},{"role":"user","content":"  hi "},{"role":"assistant","content":"Hello!   How can I help?"},{"role":"user","content":"tell me\na story"}]`

func datasetChat(t *testing.T, e *engine.Engine, model, messages, extra string, headers map[string]string) (content, finish string) {
	t.Helper()
	h := map[string]string{"Authorization": "Bearer k-audit"}
	for k, v := range headers {
		h[k] = v
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"`+model+`","messages":`+messages+extra+`}`)
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Choices[0].Message.Content, out.Choices[0].FinishReason
}

// TestDatasetSameHistorySameResponse: the history hash finds the entry,
// with a named or an inline profile, request after request.
func TestDatasetSameHistorySameResponse(t *testing.T) {
	e := newEngine(t, datasetCfg)
	for _, model := range []string{"ds-model", "ds-inline", "ds-model"} {
		got, finish := datasetChat(t, e, model, storyMessages, "", nil)
		if got != datasetStory || finish != "stop" {
			t.Fatalf("%s: %q %q", model, got, finish)
		}
		got, _ = datasetChat(t, e, model, `[{"role":"user","content":"What is the capital of France?"}]`, "", nil)
		if got != "Paris is the capital of France." {
			t.Fatalf("%s: %q", model, got)
		}
	}
	// The same history on Anthropic Messages (system outside the messages).
	w := call(t, e, "anthropic", "/v1/messages", auditAnth, `{"model":"ds-model","max_tokens":500,"system":"x","messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"text":"Paris is the capital of France."`) {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
}

// TestDatasetNoMatchDeterministicBySeed: without a match the response is a
// dataset entry drawn by the request seed.
func TestDatasetNoMatchDeterministicBySeed(t *testing.T) {
	e := newEngine(t, datasetCfg)
	msgs := `[{"role":"user","content":"something nobody asked"}]`
	seen := map[string]bool{}
	for seed := range 12 {
		h := map[string]string{"x-wwz-seed": strconv.Itoa(seed + 1)}
		a, _ := datasetChat(t, e, "ds-model", msgs, "", h)
		b, _ := datasetChat(t, e, "ds-model", msgs, "", h)
		if a != b {
			t.Fatalf("seed %v: %q != %q", h, a, b)
		}
		if !slices.Contains(datasetResponses, a) {
			t.Fatalf("not from the dataset: %q", a)
		}
		seen[a] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every seed drew the same entry: %v", seen)
	}
}

// TestDatasetMaxTokensCut: a response longer than max_tokens is cut at the
// ceiling with each dialect's truncation reason.
func TestDatasetMaxTokensCut(t *testing.T) {
	e := newEngine(t, datasetCfg)
	got, finish := datasetChat(t, e, "ds-model", storyMessages, `,"max_tokens":5`, nil)
	if finish != "length" || got == "" || got == datasetStory || !strings.HasPrefix(datasetStory, got) {
		t.Fatalf("openai: %q %q", got, finish)
	}
	w := call(t, e, "anthropic", "/v1/messages", auditAnth, `{"model":"ds-model","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Hello! How can I help?"},{"role":"user","content":"tell me a story"}]}`)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"stop_reason":"max_tokens"`) || !strings.Contains(string(w.Body()), `"text":"Once`) {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
	status, body := auditGemini(t, e, "ds-model", "k-audit", `{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"Hello! How can I help?"}]},{"role":"user","parts":[{"text":"tell me a story"}]}],"generationConfig":{"maxOutputTokens":5}}`)
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if status != 200 || json.Unmarshal(body, &g) != nil || len(g.Candidates) != 1 || g.Candidates[0].FinishReason != "MAX_TOKENS" ||
		!strings.HasPrefix(g.Candidates[0].Content.Parts[0].Text, "Once") || len(g.Candidates[0].Content.Parts[0].Text) >= len(datasetStory) {
		t.Fatalf("gemini: %d %s", status, body)
	}
}

// TestDatasetFileGoneAfterValidation: a file removed after the boot keeps
// the version already loaded; one never loaded answers random text instead
// of failing the request.
func TestDatasetFileGoneAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.jsonl")
	if err := os.WriteFile(path, []byte(`{"prompt":"q","response":"only answer"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.ReplaceAll(datasetCfg, "testdata/dataset.jsonl", path)
	e := newEngine(t, yaml)
	if got, _ := datasetChat(t, e, "ds-model", `[{"role":"user","content":"zzz"}]`, "", nil); got != "only answer" {
		t.Fatalf("loaded: %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := datasetChat(t, e, "ds-model", `[{"role":"user","content":"zzz"}]`, "", nil); got != "only answer" {
		t.Fatalf("kept: %q", got)
	}
	// A new configuration (reload) pointing at a file that is gone.
	gone := filepath.Join(dir, "never.jsonl")
	if err := os.WriteFile(gone, []byte(`{"prompt":"q","response":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(strings.ReplaceAll(datasetCfg, "testdata/dataset.jsonl", gone)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	e.SetConfig(cfg)
	if got, _ := datasetChat(t, e, "ds-model", `[{"role":"user","content":"zzz"}]`, "", nil); got == "" || got == "x" {
		t.Fatalf("random fallback: %q", got)
	}
}
