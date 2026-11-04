package generate

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// TestToolsText: the TypeScript-like rendering of the shapes the fixtures
// do not cover (task 252).
func TestToolsText(t *testing.T) {
	cases := map[string]string{
		`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]},"b":{"enum":[1,2]},"c":{"type":"object"},"d":{"type":"x"}},"required":["a"]}`: "{ a: string | number, b?: 1 | 2, c?: object, d?: any }",
		`{"type":"object","properties":{"a":{"type":"string","description":"x"}},"additionalProperties":true}`:                                                             "{\n// x\na?: string,\n[key: string]: any,\n}",
		`{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}}}`:                                                                               "{ a?: { [key: string]: any } }",
		`{"type":"object","properties":{"a":{"type":"string","default":"<b>"}}}`:                                                                                           "{\n" + `a?: string, // default: "<b>"` + "\n}",
	}
	for params, want := range cases {
		got := ToolsText([]core.ToolDef{{Name: "f", Description: "one\ntwo", Parameters: json.RawMessage(params)}})
		if w := "namespace functions {\n\n// one\n// two\ntype f = (_: " + want + ") => any;\n\n} // namespace functions"; got != w {
			t.Errorf("%s:\n%s\nwant\n%s", params, got, w)
		}
	}
	for _, bad := range []string{``, `{`, `[1,`, `{"properties":{"a":{"enum":[1,}}}`, `{"a":"\x"}`, `{"properties":[1,{]}`, `{1:2}`, `{"a"1}`} {
		if got := ToolsText([]core.ToolDef{{Name: "f", Parameters: json.RawMessage(bad)}}); got != "namespace functions {\n\ntype f = () => any;\n\n} // namespace functions" {
			t.Errorf("%q: %s", bad, got)
		}
	}
	if got := ResponseFormatText("o", "", json.RawMessage(`{"type":"object","properties":{"required":{"type":"string"}},"required":["required"],"additionalProperties":false,"$defs":{"x":{"type":"object","required":[]}},"anyOf":[{"required":[]}]}`)); got != "# Response Formats\n\n## o\n\n"+`{"type":"object","properties":{"required":{"type":"string"}},"$defs":{"x":{"type":"object"}},"anyOf":[{}]}` {
		t.Errorf("schema: %s", got)
	}
}

// TestToolSchemaTokensEdges: tool_choice, a system prompt and a body
// without the schema name.
func TestToolSchemaTokensEdges(t *testing.T) {
	tk := tokenize.O200k
	tools := []core.ToolDef{{Name: "f"}}
	base := ToolSchemaTokens(tk, &core.Request{Tools: tools})
	if got := ToolSchemaTokens(tk, &core.Request{Tools: tools, System: "s"}); got != base-chatInstructionFrame+chatInstructionFrameSystem {
		t.Fatalf("top-level system: %d, base %d", got, base)
	}
	if got := ToolSchemaTokens(tk, &core.Request{Tools: tools, Messages: []core.Message{{Role: "developer"}}}); got != base-chatInstructionFrame+chatInstructionFrameSystem {
		t.Fatalf("developer message: %d, base %d", got, base)
	}
	if got := ToolSchemaTokens(tk, &core.Request{Tools: tools, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceNone}}); got != base+toolChoiceNone {
		t.Fatalf("none: %d", got)
	}
	if got := ToolSchemaTokens(tk, &core.Request{ResponseFormat: "json_schema", Raw: []byte(`{`), ResponseSchema: json.RawMessage(`{}`)}); got != chatInstructionFrame+tk.Count("# Response Formats\n\n## \n\n{}") {
		t.Fatalf("schema without name: %d", got)
	}
	if n, d := schemaNameDesc([]byte(`{"text":{}}`)); n != "" || d != "" {
		t.Fatal(n, d)
	}
	if CompactArguments(`{"a": 1`) != `{"a": 1` || CompactArguments(`{"a": 1}`) != `{"a":1}` {
		t.Fatal("CompactArguments")
	}
}
