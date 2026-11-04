package openrouter

import (
	"encoding/json"
	"testing"
)

// TestReasoningField: the final object carries the null-field object only
// for openai/gpt-4.1* (effort first when the request sent reasoning).
func TestReasoningField(t *testing.T) {
	plain, withReq := &respEcho{}, &respEcho{Reasoning: json.RawMessage(`{"effort":"low"}`)}
	cases := []struct {
		model string
		e     *respEcho
		final bool
		want  string
	}{
		{"openai/gpt-4.1-nano", plain, true, `{"context":null,"effort":null,"summary":null}`},
		{"openai/gpt-4.1-nano", withReq, true, `{"effort":null,"context":null,"summary":null}`},
		{"openai/gpt-4.1-nano", &respEcho{Reasoning: json.RawMessage("null")}, true, `{"context":null,"effort":null,"summary":null}`},
		{"openai/gpt-4.1-nano", plain, false, "null"},
		{"openai/gpt-4o-mini", plain, true, "null"},
	}
	for _, c := range cases {
		b, err := json.Marshal(reasoningField(c.model, c.e, c.final))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s final=%v: %s, want %s", c.model, c.final, b, c.want)
		}
	}
}
