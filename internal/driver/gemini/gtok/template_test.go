package gtok

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestTemplateIsThePackage: Template answers what the package functions
// do, and Text generates with Tokenizer (task 402).
func TestTemplateIsThePackage(t *testing.T) {
	var tp Template
	tk := Tokenizer{}
	req := &core.Request{System: "be brief", Messages: []core.Message{{Role: "user", Content: "hello there"}}}
	if tp.CountPrompt(tk, req) != CountPrompt(tk, req) {
		t.Error("CountPrompt")
	}
	tool := core.ToolDef{Name: "weather", Description: "the weather", Parameters: []byte(`{"type":"object"}`)}
	if tp.Declaration(tk, tool) != Declaration(tk, tool) {
		t.Error("Declaration")
	}
	if tp.Call(tk, "weather", `{"city":"Recife"}`) != Call(tk, "weather", `{"city":"Recife"}`) {
		t.Error("Call")
	}
	if tp.Response(tk, "weather", `{"temp":30}`) != Response(tk, "weather", `{"temp":30}`) {
		t.Error("Response")
	}
	if tp.ContentOverhead() != ContentOverhead {
		t.Error("ContentOverhead")
	}
	if n := tp.MediaTokens(2, 3); n != 2*ImageTokens+3*AudioTokens {
		t.Errorf("MediaTokens: %d", n)
	}
	if _, ok := Text.Tokenizer.(Tokenizer); !ok {
		t.Errorf("Text counts with %T", Text.Tokenizer)
	}
}
