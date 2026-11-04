package gtok

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

func TestTokenizerPieces(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"Hello world", []string{"Hello", " world"}},
		{"internationalization", []string{"internatio", "nalization"}},
		{"só", []string{"só"}},
		{"atenção", []string{"aten", "ção"}},
		{"2026", []string{"2", "0", "2", "6"}},
		{" 42", []string{" ", "4", "2"}},
		{"a: b", []string{"a", ":", " b"}},
		{"x ({", []string{"x", " ({"}},
		{"\"}},", []string{"\"}", "},"}},
		{"    indented", []string{"   ", " indented"}},
		{"a  ", []string{"a", "  "}},
		{"a\n\n\n\nb", []string{"a", "\n\n\n", "\n", "b"}},
		{"a \nb", []string{"a", " ", "\n", "b"}},
		{"日本語のテ", []string{"日本", "語の", "テ"}},
		{"٣٤", []string{"٣", "٤"}},
		{"é", []string{"é"}},
		{"\t🚀🔥!", []string{"\t🚀", "🔥!"}},
	} {
		got := Tokenizer{}.Split(c.text)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("Split(%q) = %q, want %q", c.text, got, c.want)
		}
		if n := (Tokenizer{}).Count(c.text); n != len(c.want) {
			t.Errorf("Count(%q) = %d, want %d", c.text, n, len(c.want))
		}
		if strings.Join(got, "") != c.text {
			t.Errorf("Split(%q) does not rebuild the text", c.text)
		}
	}
}

// one counts every text as one token, so the template's own costs show.
type one struct{}

func (one) Count(s string) int {
	if s == "" {
		return 0
	}
	return 1
}
func (one) Split(s string) []string { return []string{s} }

func TestCountPromptBuckets(t *testing.T) {
	req := &core.Request{
		System: "s",
		Tools:  []core.ToolDef{{Name: "f"}},
		Messages: []core.Message{
			{Role: "user", Content: "u", ImageParts: 1, AudioParts: 1},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
			{Role: "user", ToolResults: []core.ToolResult{{ToolCallID: "c1", Content: `{}`}, {ToolCallID: "g", Content: `{}`}}},
		},
	}
	b := CountPrompt(one{}, req)
	// tool: specials 3 + ":f{}" 1; call: 3 + ":f" 1 + "{}" ; results alike.
	if b.System != 1 || b.ToolSchemas != 4 || b.User != 2+1 || b.Assistant != 1 || b.ToolCalls != 4 || b.ToolResults != 8 ||
		b.Images != ImageTokens+AudioTokens || b.Total != b.System+b.ToolSchemas+b.User+b.Assistant+b.ToolCalls+b.ToolResults+b.Images {
		t.Errorf("breakdown %+v", b)
	}
	if CountPrompt(one{}, &core.Request{}).Total != 0 {
		t.Error("empty request costs")
	}
}

func TestDeclarationRender(t *testing.T) {
	var got []string
	params := `{"type":"object","properties":{"p":{"type":"string","description":"d","enum":["a","b"]},"q":{"type":"array","items":{"type":"integer"}},"r":{"type":["string","null"]}},"required":["p"],"additionalProperties":false,"x-n":1.5}`
	n := Declaration(recorder{&got}, core.ToolDef{Name: "f", Description: "desc", Parameters: json.RawMessage(params)})
	want := []string{
		":f{", ":", "desc", ",", ":{additionalProperties:false,", ":{p:{", ":", "d", ",", ":[", "a", ",", "b", "],",
		":", "STRING", "},q:{", ":{", ":", "INTEGER", "},", ":", "ARRAY", "},r:{", ":[", "string", ",", "null", "]}},",
		":[", "p", "],", ":", "OBJECT", ",x-n:1.5}}",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("runs\n got  %q\n want %q", got, want)
	}
	if n != len(got)+3+12+22 { // around and "declaration", 12 keywords, 11 escaped strings
		t.Errorf("%d tokens", n)
	}
	// Parameters without a description, not JSON, a scalar schema.
	for _, c := range []struct {
		params string
		want   int
	}{{`{"type":"object"}`, 12}, {`{`, 4}, {`"x"`, 10}} {
		if n := Declaration(one{}, core.ToolDef{Name: "f", Parameters: json.RawMessage(c.params)}); n != c.want {
			t.Errorf("%s: %d tokens, want %d", c.params, n, c.want)
		}
	}
}

// recorder keeps the text runs it is asked to count.
type recorder struct{ runs *[]string }

func (r recorder) Count(s string) int {
	*r.runs = append(*r.runs, s)
	return 1
}
func (r recorder) Split(s string) []string { return []string{s} }

func TestCallAndResponseValues(t *testing.T) {
	var got []string
	Call(recorder{&got}, "f", `{"b":[1,true,null,"s"],"a":{"x":2}}`)
	if s := strings.Join(got, "|"); s != `:f{a:{x:2},b:[1,true,null,|s|]}` {
		t.Errorf("call runs = %q", s)
	}
	got = nil
	Response(recorder{&got}, "f", `not json`)
	if s := strings.Join(got, "|"); s != ":f{}" {
		t.Errorf("bad response runs = %q", s)
	}
	got = nil
	Response(recorder{&got}, "f", `[1]`)
	if s := strings.Join(got, "|"); s != ":f[1]" {
		t.Errorf("array response runs = %q", s)
	}
}
