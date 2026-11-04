package generate

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// TestAnthropicToolsText: Python's json.dumps(sort_keys=True) at every level,
// ASCII escapes, no description key when empty, {} for a missing schema.
func TestAnthropicToolsText(t *testing.T) {
	got := AnthropicToolsText([]core.ToolDef{
		{Name: "a", Description: "Olá \"x\"\\ \n\r\t\b\f\x01\x7f \U0001F600", Parameters: json.RawMessage(`{"type":"object","properties":{"z":{"type":"number","default":1.50},"b":{"enum":["é",null,true]}}}`)},
		{Name: "b", Parameters: json.RawMessage(`not json`)},
	})
	want := "<functions>\n" +
		`<function>{"description": "Ol\u00e1 \"x\"\\ \n\r\t\b\f\u0001\u007f \ud83d\ude00", "name": "a", "parameters": {"properties": {"b": {"enum": ["\u00e9", null, true]}, "z": {"default": 1.50, "type": "number"}}, "type": "object"}}</function>` + "\n" +
		`<function>{"name": "b", "parameters": {}}</function>` + "\n</functions>"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// TestAnthropicToolTokens: the fixed overhead by tool_choice and
// disable_parallel_tool_use, the system join, nothing without tools, and the
// BPE path of countToolsText.
func TestAnthropicToolTokens(t *testing.T) {
	no := false
	tools := []core.ToolDef{{Name: "a", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	base := func() *core.Request {
		return &core.Request{Driver: "anthropic", Endpoint: core.EndpointMessages, Tools: tools, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAuto}}
	}
	text := countToolsText(Simulated{}, AnthropicToolsText(tools))
	cases := []struct {
		name string
		edit func(r *core.Request)
		want int
	}{
		{"auto", func(*core.Request) {}, claude45.toolsAuto},
		{"none", func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceNone }, claude45.toolsAuto},
		{"serial", func(r *core.Request) { r.ParallelToolCalls = &no }, claude45.toolsSerial},
		{"any", func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceRequired }, claude45.toolsAny},
		{"any serial", func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceRequired; r.ParallelToolCalls = &no }, claude45.toolsAny + 1},
		{"tool", func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "a"} }, claude45.toolsNamed + 1},
		{"system", func(r *core.Request) { r.System = "s" }, claude45.toolsAuto + 1},
		{"count_tokens", func(r *core.Request) { r.Endpoint = core.EndpointCountTokens }, claude45.toolsAuto},
	}
	for _, c := range cases {
		r := base()
		c.edit(r)
		if got := ToolSchemaTokens(Simulated{}, r); got != text+c.want {
			t.Errorf("%s: %d, want %d", c.name, got, text+c.want)
		}
	}
	r := base()
	r.Tools = nil
	if got := ToolSchemaTokens(Simulated{}, r); got != 0 {
		t.Errorf("no tools: %d", got)
	}
	if got, want := countToolsText(tokenize.O200k, `{"a": 1}`), tokenize.O200k.Count(`{"a": 1}`); got != want {
		t.Errorf("BPE: %d, want %d", got, want)
	}
	// \u followed by non-hex is plain text.
	if countToolsText(Simulated{}, `\u00zz`) != (Simulated{}).Count(`\u00zz`) {
		t.Error("\\u00zz")
	}
	if countToolsText(Simulated{}, `\u00e9`) != 4 {
		t.Error("\\u00e9 counts 4")
	}
}
