package config

import (
	"fmt"
	"strings"
)

// models[].tokenizer values (spec 003, ADR 0053).
const (
	TokenizerAuto      = "auto"
	TokenizerSimulated = "simulated"
	TokenizerO200k     = "tiktoken:o200k_base"  //nolint:gosec // G101: a tokenizer name, not a credential
	TokenizerCl100k    = "tiktoken:cl100k_base" //nolint:gosec // G101: a tokenizer name, not a credential
)

func (m *ModelConfig) validateTokenizer(i int) error {
	switch m.Tokenizer {
	case "", TokenizerAuto, TokenizerSimulated, TokenizerO200k, TokenizerCl100k:
		return nil
	}
	return fmt.Errorf("config: models[%d].tokenizer must be auto | simulated | %s | %s, got %q", i, TokenizerO200k, TokenizerCl100k, m.Tokenizer)
}

// Encoding is the tokenizer the model counts with: "o200k_base",
// "cl100k_base" or "simulated". auto (or empty) looks at version, id and
// aliases, without an OpenRouter author prefix: the current OpenAI family
// is o200k_base; gpt-4, gpt-4-*, gpt-3.5-turbo* and text-embedding-* are
// cl100k_base; every other model (no public tokenizer) is simulated.
func (m *ModelConfig) Encoding() string {
	switch m.Tokenizer {
	case TokenizerSimulated:
		return TokenizerSimulated
	case TokenizerO200k, TokenizerCl100k:
		return strings.TrimPrefix(m.Tokenizer, "tiktoken:")
	}
	for _, n := range append([]string{m.Version, m.ID}, m.Aliases...) {
		if enc := encodingFor(n); enc != "" {
			return enc
		}
	}
	return TokenizerSimulated
}

var (
	o200kFamily  = []string{"gpt-4o", "chatgpt-4o", "gpt-4.1", "gpt-4.5", "gpt-5", "o1", "o3", "o4", "gpt-oss"}
	cl100kFamily = []string{"gpt-4-", "gpt-3.5-turbo", "text-embedding-"}
)

// encodingFor is the encoding of an OpenAI model name ("" for the others).
func encodingFor(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	for _, p := range o200kFamily {
		if strings.HasPrefix(name, p) {
			return "o200k_base"
		}
	}
	if name == "gpt-4" {
		return "cl100k_base"
	}
	for _, p := range cl100kFamily {
		if strings.HasPrefix(name, p) {
			return "cl100k_base"
		}
	}
	return ""
}
