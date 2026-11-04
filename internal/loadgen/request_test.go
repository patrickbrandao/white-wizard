package loadgen

import (
	"encoding/json"
	"testing"

	"github.com/valyala/fasthttp"
)

func conversation() []Message {
	return []Message{
		{Role: "system", Content: "s1"},
		{Role: "system", Content: "s2"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "vou ler", Reasoning: "r", Signature: "sig", ToolCalls: []ToolCall{
			{ID: "c1", Name: "read", Arguments: `{"p":"a"}`},
			{ID: "c2", Name: "write", Arguments: `not json`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "conteúdo"},
		{Role: "tool", ToolCallID: "c2", Content: "falhou", IsError: true},
		{Role: "assistant"},
		{Role: "tool", ToolCallID: "c3", Content: "x"},
	}
}

func TestOpenAIBody(t *testing.T) {
	r := &Request{
		Protocol: OpenAI, Model: "m", Stream: true, MaxTokens: 9, Messages: conversation(),
		Tools: []Tool{{Name: "read"}, {Name: "write", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	b, err := r.BuildBody()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[` +
		`{"role":"system","content":"s1"},{"role":"system","content":"s2"},{"role":"user","content":"u"},` +
		`{"role":"assistant","content":"vou ler","reasoning_content":"r","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"p\":\"a\"}"}},` +
		`{"id":"c2","type":"function","function":{"name":"write","arguments":"not json"}}]},` +
		`{"role":"tool","content":"conteúdo","tool_call_id":"c1"},` +
		`{"role":"tool","content":"error: falhou","tool_call_id":"c2"},` +
		`{"role":"assistant","content":""},` +
		`{"role":"tool","content":"x","tool_call_id":"c3"}],` +
		`"max_tokens":9,"stream":true,"stream_options":{"include_usage":true},"tools":[` +
		`{"type":"function","function":{"name":"read","parameters":{"type":"object","additionalProperties":true}}},` +
		`{"type":"function","function":{"name":"write","description":"d","parameters":{"type":"object"}}}]}`
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	// Tool calls without text: content null.
	r = &Request{Protocol: OpenAI, Model: "m", Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "f", Arguments: "{}"}}}}}
	b, _ = r.BuildBody()
	if want := `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`; string(b) != want {
		t.Fatalf("got %s", b)
	}
}

func TestAnthropicBody(t *testing.T) {
	r := &Request{Protocol: Anthropic, Model: "m", Stream: true, MaxTokens: 9, Messages: conversation(), Tools: []Tool{{Name: "read"}}}
	b, err := r.BuildBody()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","max_tokens":9,"system":"s1\n\ns2","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"u"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"r","signature":"sig"},{"type":"text","text":"vou ler"},` +
		`{"type":"tool_use","id":"c1","name":"read","input":{"p":"a"}},{"type":"tool_use","id":"c2","name":"write","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"conteúdo"},{"type":"tool_result","tool_use_id":"c2","content":"falhou","is_error":true}]},` +
		`{"role":"assistant","content":[{"type":"text","text":""}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c3","content":"x"}]}],` +
		`"stream":true,"tools":[{"name":"read","input_schema":{"type":"object","additionalProperties":true}}]}`
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
}

func TestFillHeaders(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	r := &Request{
		Protocol: Anthropic, URL: "http://h/v1/messages", Key: "k", Model: "m", Stream: true, Seed: 42,
		Headers: []Header{{"x-test-id", "noite"}}, Body: []byte(`{"pre":1}`),
	}
	if err := r.fill(req); err != nil {
		t.Fatal(err)
	}
	h := &req.Header
	if string(h.Peek("x-api-key")) != "k" || string(h.Peek("anthropic-version")) != AnthropicVersion || len(h.Peek("Authorization")) != 0 ||
		string(h.Peek("x-wwz-seed")) != "42" || string(h.Peek("x-test-id")) != "noite" || string(h.Peek("Accept")) != "text/event-stream" ||
		string(req.Body()) != `{"pre":1}` || string(h.Method()) != "POST" || string(h.ContentType()) != "application/json" {
		t.Fatalf("anthropic headers:\n%s", req.String())
	}
	req.Reset()
	r = &Request{Protocol: OpenAI, URL: "http://h/v1/chat/completions", Key: "k", Model: "m"}
	if err := r.fill(req); err != nil {
		t.Fatal(err)
	}
	if string(h.Peek("Authorization")) != "Bearer k" || len(h.Peek("x-wwz-seed")) != 0 || len(h.Peek("Accept")) != 0 || len(h.Peek("anthropic-version")) != 0 {
		t.Fatalf("openai headers:\n%s", req.String())
	}
	req.Reset()
	r.Key = ""
	_ = r.fill(req)
	if len(h.Peek("Authorization")) != 0 {
		t.Fatal("no key, no auth")
	}
	req.Reset()
	r = &Request{Protocol: Anthropic, URL: "http://h/v1/messages"}
	_ = r.fill(req)
	if len(h.Peek("x-api-key")) != 0 || string(h.Peek("anthropic-version")) != AnthropicVersion {
		t.Fatal("anthropic without key")
	}
}

func TestParseProtocolAndHeader(t *testing.T) {
	for in, want := range map[string]Protocol{"openai": OpenAI, "Anthropic": Anthropic} {
		if p, err := ParseProtocol(in); err != nil || p != want {
			t.Fatalf("%s: %v %v", in, p, err)
		}
	}
	if _, err := ParseProtocol("gemini"); err == nil {
		t.Fatal("gemini")
	}
	if OpenAI.DefaultPath() != "/v1/chat/completions" || Anthropic.DefaultPath() != "/v1/messages" {
		t.Fatal("paths")
	}
	if h, err := ParseHeader(" x-test-id :  noite 01 "); err != nil || h != (Header{"x-test-id", "noite 01"}) {
		t.Fatalf("%+v %v", h, err)
	}
	if h, err := ParseHeader("x-empty:"); err != nil || h != (Header{"x-empty", ""}) {
		t.Fatalf("%+v %v", h, err)
	}
	for _, bad := range []string{"nocolon", ": v", "a b: v"} {
		if _, err := ParseHeader(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
