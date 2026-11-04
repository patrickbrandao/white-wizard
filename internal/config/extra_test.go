package config

import (
	"strings"
	"testing"
)

// TestModelExtraKeys: a models[].extra key must exist in the /models item of
// a dialect that serves the model; the error cites models[i] (spec 003).
func TestModelExtraKeys(t *testing.T) {
	for _, c := range []struct {
		name, model, want string
	}{
		{"openrouter key on openai dialect", `{id: m, drivers: [openrouter], protocols: [openai], extra: {benchmarks: {}}}`, ""},
		{"capabilities on anthropic", `{id: m, protocols: [anthropic], extra: {capabilities: {}}}`, ""},
		{"gemini key", `{id: m, protocols: [gemini], extra: {description: x}}`, ""},
		{"opencode key", `{id: m, drivers: [opencode], extra: {family: x}}`, ""},
		{"no extra", `{id: m}`, ""},
		{"key of a driver that does not serve it", `{id: m, drivers: [gemini], extra: {benchmarks: {}}}`, `models[1].extra: key "benchmarks" unknown to dialects [gemini]`},
		{"unknown everywhere", `{id: m, extra: {nope: 1}}`, `models[1].extra: key "nope" unknown to dialects [anthropic gemini openai]`},
	} {
		_, err := Parse([]byte("models:\n  - {id: ok}\n  - " + c.model + "\n"))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}
