package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPromptCacheToolsFirstReal replays testdata/real/openai/cache/
// tools_prefix.json (task 352), recorded on gpt-4o-mini to check the order
// of ADR 0063 (task 303): the tool definitions head the OpenAI prompt. The
// four steps are a prompt of more than 1 024 tokens with tools, the same
// prompt again, the same prompt with one tool description changed, and the
// original tools with only the last message changed. The real answered
// cached_tokens 0, a hit, 0 (changing the tools invalidates the whole
// prefix, the system message included) and a hit up to the last block of
// 128 before the new message. With the real tokenizer and the default
// OpenAI rule (1 024 tokens, steps of 128), the wwz answers the same
// prompt_tokens and cached_tokens, token for token.
func TestPromptCacheToolsFirstReal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "openai", "cache", "tools_prefix.json"))
	if err != nil {
		t.Fatal(err)
	}
	type usage struct {
		Prompt  int `json:"prompt_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	var fx struct {
		Steps []struct {
			Request struct {
				Body json.RawMessage `json:"body"`
			} `json:"request"`
			Response struct {
				Status int `json:"status"`
				Body   struct {
					Usage usage `json:"usage"`
				} `json:"body"`
			} `json:"response"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	names := []string{"first", "repeat", "tools_changed", "last_message_changed"}
	if len(fx.Steps) != len(names) {
		t.Fatalf("fixture has %d steps, want %d", len(fx.Steps), len(names))
	}
	// What the real answered, as the rule of ADR 0063 predicts it.
	want := make([]usage, len(names))
	for i, s := range fx.Steps {
		if s.Response.Status != 200 {
			t.Fatalf("%s: real status %d", names[i], s.Response.Status)
		}
		want[i] = s.Response.Body.Usage
	}
	if want[0].Details.Cached != 0 || want[1].Details.Cached == 0 || want[2].Details.Cached != 0 ||
		want[3].Details.Cached == 0 || want[3].Details.Cached%128 != 0 || want[3].Details.Cached >= want[3].Prompt {
		t.Fatalf("fixture does not show the tools-first order: %+v", want)
	}

	e := newEngine(t, tokenizerCfg)
	h := map[string]string{"Authorization": "Bearer k-tok"}
	for i, s := range fx.Steps {
		w := call(t, e, "openai", "/v1/chat/completions", h, string(s.Request.Body))
		if w.StatusCode() != 200 {
			t.Fatalf("%s: wwz status %d: %s", names[i], w.StatusCode(), w.Body())
		}
		var out struct {
			Usage usage `json:"usage"`
		}
		if err := json.Unmarshal(w.Body(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Usage != want[i] {
			t.Errorf("%s: wwz prompt %d cached %d, real prompt %d cached %d", names[i],
				out.Usage.Prompt, out.Usage.Details.Cached, want[i].Prompt, want[i].Details.Cached)
		}
	}
}
