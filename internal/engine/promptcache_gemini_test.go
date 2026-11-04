package engine

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
)

// TestPCGeminiBlocksMatchCountPrompt: the implicit cache of a Gemini
// request walks the prompt as gtok.CountPrompt counts it, not with the chat
// template (task 235).
func TestPCGeminiBlocksMatchCountPrompt(t *testing.T) {
	tk := gtok.Tokenizer{}
	req := &core.Request{
		System: "You are terse.",
		Tools:  []core.ToolDef{{Name: "weather", Description: "Weather of a city", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		Messages: []core.Message{
			{Role: "user", Content: "Weather in Recife?", ImageParts: 1},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c1", Name: "weather", Arguments: `{"city":"Recife"}`}, {Name: "weather", Arguments: `{}`}}},
			{Role: "user", ToolResults: []core.ToolResult{{ToolCallID: "c1", Content: `{"temp":30}`}, {ToolCallID: "c9", Content: "x"}}, AudioParts: 1},
			{Role: "assistant", Content: "30 degrees."},
		},
	}
	blocks := pcTemplateBlocks(tk, req, gtok.Template{})
	if got, want := blocksTokens(tk, blocks), gtok.CountPrompt(tk, req).Total; got != want {
		t.Fatalf("blocks %d tokens, CountPrompt %d", got, want)
	}
	if len(blocks) != 12 { // system, tool, 4 contents, 2 calls, 2 responses, image, audio
		t.Fatalf("%d blocks", len(blocks))
	}
	if n := len(pcTemplateBlocks(tk, &core.Request{Messages: []core.Message{{Role: "user", Content: "a"}}}, gtok.Template{})); n != 1 {
		t.Fatalf("no system nor tools: %d blocks", n)
	}
}
