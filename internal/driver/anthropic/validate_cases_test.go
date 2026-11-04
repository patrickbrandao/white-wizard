package anthropic

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The cases of internal/server's TestValidationAnthropic (task 388: the
// validator moved here), run on the validator itself; the server test keeps
// them over HTTP, through the driver's envelope.

const hiMsgs = `"messages":[{"role":"user","content":"hi"}]`

var anthHeaders = driver.MapHeaders{"x-api-key": "k", "anthropic-version": "2023-06-01"}

func TestValidateAnthropicCases(t *testing.T) {
	d := &Driver{}
	msg := func(extra string) string { return `{"model":"wwz-fast","max_tokens":5,` + hiMsgs + `,` + extra + `}` }
	cases := []struct{ path, body, msg string }{
		{"/v1/messages", ``, "The request body is not valid JSON: EOF while parsing a value at line 1 column 0"},
		{"/v1/messages", `{"model": "claude-haiku-4-5", `, "The request body is not valid JSON: EOF while parsing a value at line 1 column 30"},
		{"/v1/messages", `{"model": x}`, "The request body is not valid JSON: expected value at line 1 column 11"},
		{"/v1/messages", `{"model": "a"} x`, "The request body is not valid JSON: trailing characters at line 1 column 16"},
		{"/v1/messages", msg(`"output_format":{}`), "output_format: This field is deprecated. Use 'output_config.format' instead. See https://platform.claude.com/docs/en/build-with-claude/structured-outputs for more information."},
		{"/v1/messages", msg(`"mcp_servers":[]`), "mcp_servers: this parameter requires anthropic-beta: mcp-client-2026-09-15 (or mcp-client-2025-11-20)"},
		{"/v1/messages", `{"max_tokens":5,"foo":1,"messages":[{"role":"robot","content":"hi"}]}`, "model: Field required"},
		{"/v1/messages", `{"model":5,"max_tokens":5,` + hiMsgs + `,"tool_choice":1}`, "model: Input should be a valid string"},
		{"/v1/messages", `{"model":"","max_tokens":5,` + hiMsgs + `}`, "model: String should have at least 1 character"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":1,"tool_choice":1}`, "tool_choice: Input should be an object"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":1,"tools":1}`, "tools: Input should be a valid array"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":null,"system":1}`, "messages: Input should be a valid array"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5}`, "messages: Field required"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"temperature":3,"messages":[{"role":"robot","content":"hi"}]}`, `messages: Unexpected role "robot". Allowed roles are "user", "assistant" or "system"`},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"content":"hi"}]}`, "messages.0.role: Field required"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"role":1,"content":"hi"}]}`, "messages.0.role: Input should be 'user', 'assistant', 'system'"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"role":"user"}]}`, "messages.0.content: Field required"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"role":"user","content":5}]}`, "messages.0.content: Input should be a valid array"},
		{"/v1/messages", `{"model":"wwz-fast","messages":[{"role":"user","content":"hi","foo":1}]}`, "messages.0.foo: Extra inputs are not permitted"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"hi","foo":1}]}]}`, "messages.0.content.0.text.foo: Extra inputs are not permitted"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text"}]}]}`, "messages.0.content.0.text.text: Field required"},
		{"/v1/messages", msg(`"system":1`), "system: Input should be a valid array"},
		{"/v1/messages", msg(`"thinking":1`), "thinking: Input should be an object"},
		{"/v1/messages", msg(`"output_config":1`), "output_config: Input does not match the expected shape."},
		{"/v1/messages", msg(`"context_management":{}`), "context_management: Extra inputs are not permitted"},
		{"/v1/messages", `{"model":"wwz-fast",` + hiMsgs + `,"foo":1}`, "max_tokens: Field required"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":"5",` + hiMsgs + `}`, "max_tokens: Input should be a valid integer"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":-1,"temperature":"x",` + hiMsgs + `}`, "max_tokens: must be greater than or equal to 0"},
		{"/v1/messages", msg(`"metadata":1`), "metadata: Input does not match the expected shape."},
		{"/v1/messages", msg(`"metadata":{"foo":1}`), "metadata.foo: Extra inputs are not permitted"},
		{"/v1/messages", msg(`"stop_sequences":1`), "stop_sequences: Input should be a valid array"},
		{"/v1/messages", msg(`"service_tier":1`), "service_tier: Input should be 'auto' or 'standard_only'"},
		{"/v1/messages", msg(`"stream":"x"`), "stream: Input should be a valid boolean"},
		{"/v1/messages", msg(`"container":1`), "container.ContainerParams: Input does not match the expected shape."},
		{"/v1/messages", msg(`"inference_geo":1`), "inference_geo: Input should be a valid string"},
		{"/v1/messages", msg(`"temperature":"x","top_k":"x"`), "temperature: Input should be a valid number"},
		{"/v1/messages", msg(`"top_k":1.5`), "top_k: Input should be a valid integer"},
		{"/v1/messages", msg(`"top_p":"x"`), "top_p: Input should be a valid number"},
		{"/v1/messages", msg(`"foo":1,"bar":2`), "foo: Extra inputs are not permitted"},
		{"/v1/messages", msg(`"temperature":3,"bar":2`), "bar: Extra inputs are not permitted"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[],"foo":1}`, "foo: Extra inputs are not permitted"},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":5,"messages":[]}`, "messages: at least one message is required"},
		{"/v1/messages", msg(`"temperature":3,"top_p":"x"`), "temperature: range: 0..1"},
		{"/v1/messages", msg(`"temperature":0.5,"top_p":"x","foo":1`), "foo: Extra inputs are not permitted"},
		{"/v1/messages", msg(`"tools":[{"name":"t","input_schema":{"type":"object"},"foo":1}]`), "tools.0.custom.foo: Extra inputs are not permitted"},
		{"/v1/messages", msg(`"tools":[1,{"type":"web_search_20250305","name":"w","foo":1},{"type":"custom","name":"t","input_schema":{},"bar":1}]`), "tools.2.custom.bar: Extra inputs are not permitted"},
		{"/v1/messages", msg(`"temperature":3`), "temperature: range: 0..1"},
		{"/v1/messages", msg(`"temperature":-1`), "`temperature` cannot be set to -1 for this model."},
		{"/v1/messages", msg(`"top_p":3`), "top_p: range: 0..1"},
		{"/v1/messages/count_tokens", `{"model":"wwz-fast",` + hiMsgs + `,"max_tokens":5,"foo":1}`, "max_tokens: Extra inputs are not permitted"},
	}
	for _, c := range cases {
		kind := driver.KindMessages
		if strings.HasSuffix(c.path, "/count_tokens") {
			kind = driver.KindCountTokens
		}
		e := d.ValidateRequest(kind, []byte(c.body), anthHeaders, nil)
		if e == nil || e.Message != c.msg {
			t.Errorf("%s:\n got  %+v\n want %q", c.body, e, c.msg)
		}
	}
	// With the betas, the gated fields pass the check.
	beta := driver.MapHeaders{"anthropic-beta": "context-management-2025-06-27, mcp-client-2025-11-20", "anthropic-version": "2023-06-01"}
	if e := d.ValidateRequest(driver.KindMessages, []byte(msg(`"context_management":{},"mcp_servers":[]`)), beta, nil); e != nil {
		t.Fatalf("betas: %+v", e)
	}
}
