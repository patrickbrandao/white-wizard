package engine

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestCoveragePromptCacheHelpers(t *testing.T) {
	tk := tokenizerFor(&config.ModelConfig{ID: "gpt-4o-mini"})
	if msgOverhead(tk, "user", "bob") != msgOverhead(tk, "user", "")+generate.NameOverhead {
		t.Fatal("named message costs more")
	}
	if text, blocks, isText := pcContent(nil); text != "" || blocks != nil || isText {
		t.Fatal("empty content")
	}
	blocks := pcAutoBlocks(tk, &core.Request{Messages: []core.Message{{Role: "user", Content: "look", ImageParts: 1, AudioParts: 1}}})
	found := false
	for _, b := range blocks {
		for _, p := range b.parts {
			if p.syn == "image" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("image tokens are a block of their own")
	}
	var body pcRawBody
	raw := `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"cache_control":{"type":"ephemeral"}}]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	out := pcExplicitBlocks(tk, &body)
	if len(out) < 3 || out[len(out)-2].bp == 0 {
		t.Fatalf("message-level cache_control marks the last block: %+v", out)
	}
}

func TestCoverageBreakToolsBadSchema(t *testing.T) {
	r := &request{
		req: &core.Request{Tools: []core.ToolDef{{Name: "t", Parameters: json.RawMessage(`"not a schema"`)}}},
		rng: generate.NewRNG(1),
	}
	e := &Engine{}
	calls, broke := e.breakTools(r, []core.ToolCall{{Name: "t", Arguments: `{"a":1}`}}, "invalid_json_truncated", "")
	if !broke || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("an unparsable schema leaves the arguments as they are: %v %+v", broke, calls)
	}
}

func TestCoverageGenerateHelpers(t *testing.T) {
	m := &config.ModelConfig{ID: "gpt-4o-mini"}
	drv := covDriver(t, "openai")
	text := generatorOf(m, driver.EngineDeclOf(drv))
	r := &request{
		in: Input{Driver: drv}, req: &core.Request{Messages: []core.Message{{Role: "user", Content: "hi"}}},
		model: m, text: text, tk: text.Tokenizer, rng: generate.NewRNG(1),
	}
	e := &Engine{}
	if got := e.content(r, 0); got == "" {
		t.Fatal("a target of 0 still produces one token")
	}
	r.setInputBreakdown() // no planned response: nothing to stamp

	r.in.Driver = nil
	if e.toolBreakDriverAllowed(&request{errs: config.ErrorProfile{ToolMisbehavior: &config.ToolMisbehavior{Drivers: []string{"openai"}}}}) {
		t.Fatal("no driver is not on the list")
	}
}
