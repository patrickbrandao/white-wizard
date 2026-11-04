package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// Task 196 (ADR 0053): every model counts with its own tokenizer.

const tokenizerCfg = `
server: {seed: 7}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {output_tokens: {dist: fixed, value: 40}}
models:
  - {id: gpt-4o-mini, latency: fast, generation: fixed}
  - {id: gpt-4o-sim, tokenizer: simulated, latency: fast, generation: fixed}
  - {id: wwz-fast, tokenizer: "tiktoken:o200k_base", latency: fast, generation: fixed}
  - {id: gpt-3.5-turbo, latency: fast, generation: fixed}
  - {id: wwz-plain, latency: fast, generation: fixed}
keys:
  - {key: k-tok, label: tok}
`

func TestTokenizerPerModel(t *testing.T) {
	e := newEngine(t, tokenizerCfg)
	h := map[string]string{"Authorization": "Bearer k-tok"}
	probe := `"messages":[{"role":"user","content":"Responda só: ok"}]`
	cl := tokenize.Cl100k.Count("Responda só: ok")
	for _, c := range []struct {
		model string
		tk    generate.Tokenizer
		want  int
	}{
		{"gpt-4o-mini", tokenize.O200k, 13},
		{"gpt-4o-sim", generate.Simulated{}, 12},
		{"wwz-fast", tokenize.O200k, 13},
		{"gpt-3.5-turbo", tokenize.Cl100k, 7 + cl},
		{"wwz-plain", generate.Simulated{}, 12},
	} {
		w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"`+c.model+`",`+probe+`}`)
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(w.Body(), &out); err != nil || w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", c.model, w.StatusCode(), w.Body())
		}
		if out.Usage.Prompt != c.want {
			t.Errorf("%s: prompt_tokens %d, want %d", c.model, out.Usage.Prompt, c.want)
		}
		content := out.Choices[0].Message.Content
		if n := c.tk.Count(content); n != out.Usage.Completion || n == 0 || n > 40 {
			t.Errorf("%s: completion_tokens %d, content counts %d", c.model, out.Usage.Completion, n)
		}
	}
}

// TestTokenizerStream: a BPE model streams whole UTF-8 characters and the
// deltas rebuild the text whose count is completion_tokens.
func TestTokenizerStream(t *testing.T) {
	e := newEngine(t, tokenizerCfg)
	h := map[string]string{"Authorization": "Bearer k-tok"}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"olá 🎉"}]}`)
	var text strings.Builder
	completion := 0
	for _, line := range strings.Split(string(w.Body()), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
		for _, ch := range ev.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if ev.Usage != nil {
			completion = ev.Usage.Completion
		}
	}
	if completion == 0 || tokenize.O200k.Count(text.String()) != completion {
		t.Fatalf("streamed %q: completion_tokens %d, o200k %d", text.String(), completion, tokenize.O200k.Count(text.String()))
	}
}

// TestRealPromptTokensOpenAI: the requests of the real OpenAI fixtures
// (testdata/real/openai/cache/) give the prompt_tokens the real answered
// (o200k_base with the chat template, spec 007), and the cached part of it
// in sequence (spec 007, ADR 0046).
func TestRealPromptTokensOpenAI(t *testing.T) {
	type usage struct {
		Prompt  int `json:"prompt_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	e := newEngine(t, tokenizerCfg)
	h := map[string]string{"Authorization": "Bearer k-tok"}
	files, _ := filepath.Glob("../../testdata/real/openai/cache/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Steps []struct {
				Request struct {
					Body json.RawMessage `json:"body"`
				} `json:"request"`
				Response struct {
					Body struct {
						Usage usage `json:"usage"`
					} `json:"body"`
				} `json:"response"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		for i, s := range fx.Steps {
			w := call(t, e, "openai", "/v1/chat/completions", h, string(s.Request.Body))
			var out struct {
				Usage usage `json:"usage"`
			}
			_ = json.Unmarshal(w.Body(), &out)
			if want := s.Response.Body.Usage; want.Prompt == 0 || out.Usage != want {
				t.Errorf("%s step %d: usage %+v, real %+v (%.200s)", filepath.Base(f), i, out.Usage, want, w.Body())
			}
		}
	}
}
