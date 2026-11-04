package oai_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func parseOK(t *testing.T, body string) *core.Request {
	t.Helper()
	req, e := oai.ParseChat([]byte(body), driver.MapHeaders{}, "openai")
	if e != nil {
		t.Fatalf("ParseChat(%s): %v", body, e)
	}
	return req
}

func TestParseChatErrors(t *testing.T) {
	cases := []struct {
		name, body, param, msg string
	}{
		{"invalid json", `{`, "", "could not parse the JSON body"},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, "model", "model parameter"},
		{"no messages", `{"model":"m","messages":[]}`, "messages", "too short"},
		{"message not object", `{"model":"m","messages":[1]}`, "messages[0]", "Invalid value at 'messages[0]'"},
		{"bad role", `{"model":"m","messages":[{"role":"robot","content":"x"}]}`, "messages[0].role", "'robot'"},
		{"bad tool type", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"web","function":{"name":"f"}}]}`, "tools[0].type", "'web'"},
		{"tool without name", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`, "tools[0].function.name", "Missing required parameter"},
		{"named choice unknown", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"g"}}}`, "tool_choice", "no tool named 'g'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, e := oai.ParseChat([]byte(c.body), driver.MapHeaders{}, "openai")
			if e == nil || req != nil {
				t.Fatalf("want error, got req=%+v", req)
			}
			if e.Kind != core.ErrInvalidRequest || e.Param != c.param || !strings.Contains(e.Message, c.msg) {
				t.Fatalf("error = %+v, want param %q containing %q", e, c.param, c.msg)
			}
		})
	}
}

func TestParseChatFields(t *testing.T) {
	req := parseOK(t, `{
		"model":"gpt-x","stream":true,"stream_options":{"include_usage":true},
		"max_tokens":10,"max_completion_tokens":20,"n":2,"parallel_tool_calls":false,
		"reasoning_effort":"high","temperature":0.5,"seed":7,
		"metadata":{"a":"b"},"user":"u1",
		"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":{"type":"object"}}},
		"messages":[
			{"role":"developer","content":"be terse"},
			{"role":"user","content":[{"type":"text","text":"a"},{"type":"input_text","text":"b"},{"type":"image_url","image_url":{"url":"x"}},{"type":"input_image"},{"type":"input_audio"},{"type":"audio"},{"type":"file"}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}],"function_call":{"name":"g","arguments":"{\"x\":1}"}},
			{"role":"function","name":"g","content":"42"},
			{"role":"tool","tool_call_id":"c1","content":7}
		]}`)
	if req.Driver != "openai" || req.Endpoint != core.EndpointChat || req.Model != "gpt-x" || !req.Stream || !req.IncludeUsage {
		t.Fatalf("basic fields: %+v", req)
	}
	if req.MaxTokens != 20 || req.N != 2 || req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("max_completion_tokens wins, n, parallel: %+v", req)
	}
	if !req.Reasoning.Requested || req.Reasoning.Effort != "high" {
		t.Fatalf("reasoning_effort: %+v", req.Reasoning)
	}
	if *req.Temperature != 0.5 || *req.Seed != 7 {
		t.Fatalf("temperature/seed: %v %v", *req.Temperature, *req.Seed)
	}
	if req.Metadata["a"] != "b" || req.Metadata["user"] != "u1" {
		t.Fatalf("metadata: %v", req.Metadata)
	}
	if req.ResponseFormat != "json_schema" || string(req.ResponseSchema) != `{"type":"object"}` {
		t.Fatalf("response_format: %q %s", req.ResponseFormat, req.ResponseSchema)
	}
	// The system message stays only in Messages (task 245).
	if req.System != "" || req.SystemPrompt() != "be terse" || len(req.Messages) != 5 || req.Messages[0].Role != "system" {
		t.Fatalf("developer becomes system: %q %+v", req.System, req.Messages)
	}
	u := req.Messages[1]
	if u.Content != "a\nb" || u.ImageParts != 2 || u.AudioParts != 2 {
		t.Fatalf("content parts: %+v", u)
	}
	a := req.Messages[2]
	if a.Content != "" || len(a.ToolCalls) != 2 || a.ToolCalls[0] != (core.ToolCall{ID: "c1", Name: "f", Arguments: "{}"}) || a.ToolCalls[1].Name != "g" {
		t.Fatalf("assistant tool calls: %+v", a)
	}
	if f := req.Messages[3]; f.Role != "tool" || f.Name != "g" || f.Content != "42" {
		t.Fatalf("function becomes tool: %+v", f)
	}
	if tm := req.Messages[4]; tm.ToolCallID != "c1" || tm.Content != "7" {
		t.Fatalf("non-string content kept raw: %+v", tm)
	}
	if req.ToolChoice.Mode != core.ToolChoiceAuto {
		t.Fatalf("default tool_choice: %+v", req.ToolChoice)
	}
}

func TestParseChatMaxTokensAndUserMetadata(t *testing.T) {
	req := parseOK(t, `{"model":"m","max_tokens":5,"user":"u","messages":[{"role":"user","content":"x"},{"role":"system","content":"late"}]}`)
	if req.MaxTokens != 5 || req.Metadata["user"] != "u" || req.System != "" {
		t.Fatalf("max_tokens / user / late system: %+v", req)
	}
}

func TestParseChatReasoningObject(t *testing.T) {
	cases := []struct {
		body string
		want core.ReasoningRequest
	}{
		{`{"effort":"low","max_tokens":100}`, core.ReasoningRequest{Requested: true, Effort: "low", BudgetTokens: 100}},
		{`{"enabled":false,"exclude":true}`, core.ReasoningRequest{}},
		{`{"effort":"none"}`, core.ReasoningRequest{Effort: "none"}},
		{`{"enabled":true,"exclude":true}`, core.ReasoningRequest{Requested: true, Exclude: true}},
		{`"high"`, core.ReasoningRequest{}}, // not an object: ignored
	}
	for _, c := range cases {
		req := parseOK(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"reasoning":`+c.body+`}`)
		if req.Reasoning != c.want {
			t.Errorf("reasoning %s = %+v, want %+v", c.body, req.Reasoning, c.want)
		}
	}
}

func TestParseChatResponseFormat(t *testing.T) {
	for body, want := range map[string]string{
		`{"type":"json_object"}`:                  "",
		`{"type":"json_schema","json_schema":1}`:  "",
		`{"type":"json_schema"}`:                  "",
		`{"type":"json_schema","json_schema":{}}`: "",
	} {
		req := parseOK(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":`+body+`}`)
		if string(req.ResponseSchema) != want || req.ResponseFormat == "" {
			t.Errorf("%s: format %q schema %s", body, req.ResponseFormat, req.ResponseSchema)
		}
	}
}

func TestParseChatTools(t *testing.T) {
	const base = `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],"functions":[{"name":"legacy"}]`
	cases := []struct {
		extra string
		want  core.ToolChoice
	}{
		{``, core.ToolChoice{Mode: core.ToolChoiceAuto}},
		{`,"tool_choice":"none"`, core.ToolChoice{Mode: core.ToolChoiceNone}},
		{`,"tool_choice":"required"`, core.ToolChoice{Mode: core.ToolChoiceRequired}},
		{`,"tool_choice":"any"`, core.ToolChoice{Mode: core.ToolChoiceRequired}},
		{`,"tool_choice":"auto"`, core.ToolChoice{Mode: core.ToolChoiceAuto}},
		{`,"tool_choice":{"type":"function","function":{"name":"f"}}`, core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "f"}},
		{`,"tool_choice":{"type":"tool","name":"legacy"}`, core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "legacy"}},
		{`,"tool_choice":{"type":"function"}`, core.ToolChoice{Mode: core.ToolChoiceAuto}},
		{`,"tool_choice":42`, core.ToolChoice{Mode: core.ToolChoiceAuto}},
		{`,"function_call":"none"`, core.ToolChoice{Mode: core.ToolChoiceNone}},
		{`,"function_call":{"name":"legacy"}`, core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "legacy"}},
	}
	for _, c := range cases {
		req := parseOK(t, base+c.extra+`}`)
		if req.ToolChoice != c.want {
			t.Errorf("%s: tool choice %+v, want %+v", c.extra, req.ToolChoice, c.want)
		}
		if len(req.Tools) != 2 || req.Tools[0].Name != "f" || req.Tools[0].Description != "d" || string(req.Tools[0].Parameters) != `{"type":"object"}` || req.Tools[1].Name != "legacy" {
			t.Fatalf("tools: %+v", req.Tools)
		}
	}
}

func TestParseCompletions(t *testing.T) {
	if _, e := oai.ParseCompletions([]byte(`[`), "openai"); e == nil || e.Kind != core.ErrInvalidRequest {
		t.Fatalf("invalid json: %v", e)
	}
	if _, e := oai.ParseCompletions([]byte(`{"prompt":"x"}`), "openai"); e == nil || e.Param != "model" {
		t.Fatalf("missing model: %v", e)
	}
	cases := []struct{ prompt, want string }{
		{`"hello"`, "hello"},
		{`["a","b"]`, "a\nb"},
		{`[1,2]`, "[1,2]"},
	}
	for _, c := range cases {
		req, e := oai.ParseCompletions([]byte(`{"model":"m","prompt":`+c.prompt+`}`), "openai")
		if e != nil {
			t.Fatal(e)
		}
		if req.Endpoint != core.EndpointCompletions || len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != c.want {
			t.Errorf("prompt %s: %+v", c.prompt, req.Messages)
		}
		if req.ToolChoice.Mode != core.ToolChoiceNone || req.Metadata != nil || req.MaxTokens != 0 {
			t.Errorf("defaults: %+v", req)
		}
	}
	req, e := oai.ParseCompletions([]byte(`{"model":"m","prompt":"p","max_tokens":3,"stream":true,"stream_options":{"include_usage":true},"echo":true}`), "opencode")
	if e != nil {
		t.Fatal(e)
	}
	if req.Driver != "opencode" || req.MaxTokens != 3 || !req.Stream || !req.IncludeUsage || req.Metadata["echo"] != "true" {
		t.Fatalf("completions fields: %+v", req)
	}
}
