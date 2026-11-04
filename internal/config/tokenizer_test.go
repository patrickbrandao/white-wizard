package config

import (
	"strings"
	"testing"
)

// TestTokenizerValidation: only auto, simulated and the two tiktoken
// encodings are accepted; the default is auto (spec 003, ADR 0053).
func TestTokenizerValidation(t *testing.T) {
	cfg, err := Parse([]byte("models:\n  - {id: a}\n  - {id: b, tokenizer: simulated}\n  - {id: c, tokenizer: \"tiktoken:o200k_base\"}\n  - {id: d, tokenizer: \"tiktoken:cl100k_base\"}\n  - {id: e, tokenizer: auto}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models[0].Tokenizer != TokenizerAuto {
		t.Fatalf("default tokenizer %q", cfg.Models[0].Tokenizer)
	}
	for _, bad := range []string{"tiktoken:p50k_base", "o200k_base", "tiktoken", "Simulated"} {
		_, err := Parse([]byte("models:\n  - {id: ok}\n  - {id: x, tokenizer: \"" + bad + "\"}\n"))
		if err == nil || !strings.Contains(err.Error(), "models[1].tokenizer") {
			t.Errorf("tokenizer %q: err %v", bad, err)
		}
	}
}

func TestModelEncoding(t *testing.T) {
	cases := []struct {
		m    ModelConfig
		want string
	}{
		{ModelConfig{ID: "gpt-4o-mini"}, "o200k_base"},
		{ModelConfig{ID: "openai/gpt-5.1", Tokenizer: TokenizerAuto}, "o200k_base"},
		{ModelConfig{ID: "wwz-fast", Version: "GPT-4.1-2025-04-14"}, "o200k_base"},
		{ModelConfig{ID: "my-model", Aliases: []string{"x", "o3-mini"}}, "o200k_base"},
		{ModelConfig{ID: "chatgpt-4o-latest"}, "o200k_base"},
		{ModelConfig{ID: "gpt-oss-120b"}, "o200k_base"},
		{ModelConfig{ID: "o1"}, "o200k_base"},
		{ModelConfig{ID: "o4-mini"}, "o200k_base"},
		{ModelConfig{ID: "gpt-4.5-preview"}, "o200k_base"},
		{ModelConfig{ID: "gpt-4"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-4-turbo-2024-04-09"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-4-0613"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-3.5-turbo"}, "cl100k_base"},
		{ModelConfig{ID: "text-embedding-3-small"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-4-32k"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-3.5-turbo-16k"}, "cl100k_base"},
		{ModelConfig{ID: "gpt-4.1"}, "o200k_base"},
		{ModelConfig{ID: "gpt-4.1-nano"}, "o200k_base"},
		{ModelConfig{ID: "claude-haiku-4-5"}, "simulated"},
		{ModelConfig{ID: "anthropic/claude-sonnet-4.5"}, "simulated"},
		{ModelConfig{ID: "wwz-fast"}, "simulated"},
		{ModelConfig{ID: "gpt-4o-mini", Tokenizer: TokenizerSimulated}, "simulated"},
		{ModelConfig{ID: "wwz-fast", Tokenizer: TokenizerO200k}, "o200k_base"},
		{ModelConfig{ID: "gpt-4o", Tokenizer: TokenizerCl100k}, "cl100k_base"},
	}
	for _, c := range cases {
		if got := c.m.Encoding(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.m, got, c.want)
		}
	}
}
