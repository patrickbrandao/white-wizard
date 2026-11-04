package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/oaicheck"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// validateAs runs the request validator of the driver name with the models
// v knows (none: every model), as the server's wrapper does.
func validateAs(t *testing.T, name string, v *validation, kind driver.Kind, body string, h driver.Headers) *core.Error {
	t.Helper()
	drv, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	check, ok := driver.As[driver.RequestValidator](drv)
	if !ok {
		t.Fatalf("%s has no validator", name)
	}
	if v == nil {
		v = &validation{}
	}
	return check.ValidateRequest(kind, []byte(body), h, v)
}

const hiMsgs = `"messages":[{"role":"user","content":"hi"}]`

// oaiEnvelope is the OpenAI error envelope with every field.
type oaiEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestServerStrictKeyRefused: server.strict no longer exists (ADR 0042).
func TestServerStrictKeyRefused(t *testing.T) {
	_, err := config.Parse([]byte("server: {strict: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("server.strict accepted: %v", err)
	}
}

func TestValidationAlwaysOn(t *testing.T) {
	h := start(t, fastCfg, nil)
	ok := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"wwz-fast","temperature":0.2,"top_p":1,"stop":null,"user":null,"stream":false,` + hiMsgs + `}`},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"user","content":"hi","foo":1}]}`},
		{"/v1/chat/completions", `{"model":"wwz-fast","stream":true,"stream_options":{"include_usage":true},` + hiMsgs + `}`},
		{"/v1/chat/completions", `{"model":"wwz-fast","logprobs":true,"top_logprobs":2,"metadata":{"a":"b"},"logit_bias":{"1":-1},` + hiMsgs + `}`},
		{"/v1/chat/completions", `{"model":"wwz-fast","tool_choice":"auto","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` + hiMsgs + `}`},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"user","content":[{"type":"text","text":"x"}]}]}`},
		{"/v1/completions", `{"model":"wwz-fast","prompt":"hi","echo":false}`},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":10,"system":"s","top_k":5,"temperature":1,"top_p":0.5,"metadata":{"user_id":"u"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`},
		{"/v1/messages", `{"model":"wwz-fast","max_tokens":0,"container":"c","service_tier":"auto",` + hiMsgs + `}`},
		{"/v1/messages/count_tokens", `{"model":"wwz-fast",` + hiMsgs + `}`},
	}
	for _, c := range ok {
		auth := oaiAuth
		if strings.HasPrefix(c.path, "/v1/messages") {
			auth = anthAuth
		}
		if resp := h.do("POST", c.path, auth, c.body); resp.StatusCode() != 200 {
			t.Errorf("%s %s: %d %s", c.path, c.body, resp.StatusCode(), resp.Body())
		}
	}
}

func TestValidationOpenAI(t *testing.T) {
	h := start(t, fastCfg, nil)
	chat := func(extra string) string { return `{"model":"wwz-fast",` + hiMsgs + `,` + extra + `}` }
	cases := []struct{ path, body, msg, param, code string }{
		{"/v1/chat/completions", `{"model":`, oaicheck.BadJSON, "<nil>", "<nil>"},
		{"/v1/chat/completions", `[1]`, oaicheck.BadJSON, "<nil>", "<nil>"},
		{"/v1/chat/completions", `{"model":5,` + hiMsgs + `}`, oaicheck.BadJSON, "<nil>", "<nil>"},
		{"/v1/chat/completions", `{` + hiMsgs + `}`, "you must provide a model parameter", "<nil>", "<nil>"},
		{"/v1/chat/completions", `{"model":"",` + hiMsgs + `}`, "you must provide a model parameter", "<nil>", "<nil>"},
		{"/v1/chat/completions", `{"model":"wwz-fast","foo":1}`, "Missing required parameter: 'messages'.", "messages", "missing_required_parameter"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":null}`, "Invalid type for 'messages': expected an array of objects, but got null instead.", "messages", "invalid_type"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[]}`, "Invalid 'messages': empty array. Expected an array with minimum length 1, but got an empty array instead.", "messages", "empty_array"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":["hi"]}`, "Invalid type for 'messages[0]': expected an object, but got a string instead.", "messages[0]", "invalid_type"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"content":"hi"}]}`, "Missing required parameter: 'messages[0].role'.", "messages[0].role", "missing_required_parameter"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":5,"content":"hi"}]}`, "Invalid type for 'messages[0].role': expected one of 'system', 'assistant', 'user', 'function', 'tool', or 'developer', but got an integer instead.", "messages[0].role", "invalid_type"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"user","content":"a"},{"role":"robot","content":"hi"}],"foo":1}`, "Invalid value: 'robot'. Supported values are: 'system', 'assistant', 'user', 'function', 'tool', and 'developer'.", "messages[1].role", "invalid_value"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"user"}]}`, "Invalid value for 'content': expected a string, got null.", "messages.[0].content", "<nil>"},
		{"/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"user","content":5}]}`, "Invalid type for 'messages[0].content': expected one of a string or array of objects, but got an integer instead.", "messages[0].content", "invalid_type"},
		{"/v1/chat/completions", chat(`"n":"b","top_p":"a"`), "Invalid type for 'top_p': expected a decimal, but got a string instead.", "top_p", "invalid_type"},
		{"/v1/chat/completions", chat(`"temperature":true`), "Invalid type for 'temperature': expected a decimal, but got a boolean instead.", "temperature", "invalid_type"},
		{"/v1/chat/completions", chat(`"temperature":2.5,"foo":1`), "Invalid 'temperature': decimal above maximum value. Expected a value <= 2, but got 2.5 instead.", "temperature", "decimal_above_max_value"},
		{"/v1/chat/completions", chat(`"frequency_penalty":-3`), "Invalid 'frequency_penalty': decimal below minimum value. Expected a value >= -2, but got -3 instead.", "frequency_penalty", "decimal_below_min_value"},
		{"/v1/chat/completions", chat(`"n":0`), "Invalid 'n': integer below minimum value. Expected a value >= 1, but got 0 instead.", "n", "integer_below_min_value"},
		{"/v1/chat/completions", chat(`"n":12345`), "Invalid 'n': integer above maximum value. Expected a value <= 128, but got 12345 instead.", "n", "integer_above_max_value"},
		{"/v1/chat/completions", chat(`"n":1.5`), "Invalid type for 'n': expected an integer, but got a decimal number instead.", "n", "invalid_type"},
		{"/v1/chat/completions", chat(`"stream":"yes"`), "Invalid type for 'stream': expected a boolean, but got a string instead.", "stream", "invalid_type"},
		{"/v1/chat/completions", chat(`"user":{}`), "Invalid type for 'user': expected a string, but got an object instead.", "user", "invalid_type"},
		{"/v1/chat/completions", chat(`"stop":1`), "Invalid type for 'stop': expected one of a string or array of strings, but got an integer instead.", "stop", "invalid_type"},
		{"/v1/chat/completions", chat(`"modalities":1`), "Invalid type for 'modalities': expected an array of one of 'text' or 'audio', but got an integer instead.", "modalities", "invalid_type"},
		{"/v1/chat/completions", chat(`"service_tier":1`), "Invalid type for 'service_tier': expected one of 'auto', 'default', 'fast', 'flex', or 'priority', but got an integer instead.", "service_tier", "invalid_type"},
		{"/v1/chat/completions", chat(`"tools":1`), "Invalid type for 'tools': expected an array of objects, but got an integer instead.", "tools", "invalid_type"},
		{"/v1/chat/completions", chat(`"functions":{}`), "Invalid type for 'functions': expected an array of function definitions, but got an object instead.", "functions", "invalid_type"},
		{"/v1/chat/completions", chat(`"tool_choice":1`), "Invalid type for 'tool_choice': expected one of one of 'none', 'auto', or 'required' or object, but got an integer instead.", "tool_choice", "invalid_type"},
		{"/v1/chat/completions", chat(`"tool_choice":{}`), "Missing required parameter: 'tool_choice.type'.", "tool_choice.type", "missing_required_parameter"},
		{"/v1/chat/completions", chat(`"function_call":{"x":1}`), "Missing required parameter: 'function_call.name'.", "function_call.name", "missing_required_parameter"},
		{"/v1/chat/completions", chat(`"response_format":{}`), "Missing required parameter: 'response_format.type'.", "response_format.type", "missing_required_parameter"},
		{"/v1/chat/completions", chat(`"audio":1`), "Invalid type for 'audio': expected an object, but got an integer instead.", "audio", "invalid_type"},
		{"/v1/chat/completions", chat(`"metadata":1`), "Invalid type for 'metadata': expected a metadata object, but got an integer instead.", "metadata", "invalid_type"},
		{"/v1/chat/completions", chat(`"metadata":{"x":[1]}`), "Invalid type for 'metadata.x': expected a string, but got an array instead.", "metadata.x", "invalid_type"},
		{"/v1/chat/completions", chat(`"logit_bias":{"x":[1]}`), "Invalid type for 'logit_bias.x': expected a decimal, but got an array instead.", "logit_bias.x", "invalid_type"},
		{"/v1/chat/completions", chat(`"stream_options":{"x":[1]}`), "Unknown parameter: 'stream_options.x'.", "stream_options.x", "unknown_parameter"},
		{"/v1/chat/completions", chat(`"web_search_options":1`), "Invalid type for 'web_search_options': expected an object, but got an integer instead.", "web_search_options", "invalid_type"},
		{"/v1/chat/completions", chat(`"prompt_cache_retention":1`), "Invalid type for 'prompt_cache_retention': expected one of 'in_memory' or '24h', but got an integer instead.", "prompt_cache_retention", "invalid_type"},
		{"/v1/chat/completions", chat(`"top_logprobs":2,"foo":1`), "The 'top_logprobs' parameter is only allowed when 'logprobs' is enabled.", "top_logprobs", "<nil>"},
		{"/v1/chat/completions", chat(`"stream_options":{"include_usage":true},"foo":1`), "The 'stream_options' parameter is only allowed when 'stream' is enabled.", "stream_options", "<nil>"},
		{"/v1/chat/completions", chat(`"foo":{"a":[1]}`), "Unrecognized request argument supplied: foo", "<nil>", "<nil>"},
		{"/v1/chat/completions", chat(`"zeta":1,"alpha":2,"Mid":3,"temperature":null,"foo":null`), "Unrecognized request arguments supplied: Mid, alpha, foo, zeta", "<nil>", "<nil>"},
		{"/v1/completions", `{"model":"wwz-fast","prompt":"hi","messages":[]}`, "Unrecognized request argument supplied: messages", "<nil>", "<nil>"},
		{"/v1/completions", `{"prompt":"hi"}`, "you must provide a model parameter", "<nil>", "<nil>"},
	}
	for _, c := range cases {
		resp := h.do("POST", c.path, oaiAuth, c.body)
		var out oaiEnvelope
		if resp.StatusCode() != 400 || json.Unmarshal(resp.Body(), &out) != nil {
			t.Errorf("%s: %d %s", c.body, resp.StatusCode(), resp.Body())
			continue
		}
		e := out.Error
		if e.Message != c.msg || e.Type != "invalid_request_error" || str(e.Param) != c.param || str(e.Code) != c.code {
			t.Errorf("%s:\n got  %s\n want %q param %s code %s", c.body, resp.Body(), c.msg, c.param, c.code)
		}
	}
}

func TestValidationAnthropic(t *testing.T) {
	h := start(t, fastCfg, nil)
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
		resp := h.do("POST", c.path, anthAuth, c.body)
		var out struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if resp.StatusCode() != 400 || json.Unmarshal(resp.Body(), &out) != nil {
			t.Errorf("%s: %d %s", c.body, resp.StatusCode(), resp.Body())
			continue
		}
		if out.Type != "error" || out.Error.Type != "invalid_request_error" || out.Error.Message != c.msg {
			t.Errorf("%s:\n got  %s\n want %q", c.body, resp.Body(), c.msg)
		}
	}
	// With the betas, the gated fields pass the check.
	beta := map[string]string{"anthropic-beta": "context-management-2025-06-27, mcp-client-2025-11-20"}
	for k, v := range anthAuth {
		beta[k] = v
	}
	if resp := h.do("POST", "/v1/messages", beta, msg(`"context_management":{},"mcp_servers":[]`)); resp.StatusCode() != 200 {
		t.Fatalf("betas: %d %s", resp.StatusCode(), resp.Body())
	}
}

func TestValidationGateways(t *testing.T) {
	ro := start(t, fastCfg+"drivers: [{name: openrouter}, {name: opencode}]\n", nil)
	for _, body := range []string{
		`{"model":"wwz-fast","transforms":[],"foo":1,` + hiMsgs + `}`,
	} {
		if resp := ro.do("POST", "/api/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 200 {
			t.Errorf("openrouter %s: %d %s", body, resp.StatusCode(), resp.Body())
		}
	}
	for _, c := range []struct{ body, want string }{
		{`{"model":`, `{"error":{"message":"Input required: specify \"prompt\" or \"messages\"","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","messages":[]}`, `{"error":{"message":"Input required: specify \"prompt\" or \"messages\"","code":400},"user_id":"user_wwz"}`},
		{`{"model":5,` + hiMsgs + `}`, `{"error":{"message":"No models provided","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","top_p":"x","temperature":"x",` + hiMsgs + `}`, `{"error":{"message":"temperature: Invalid input: expected number, received string","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","top_p":[],` + hiMsgs + `}`, `{"error":{"message":"top_p: Invalid input: expected number, received array","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","temperature":{},` + hiMsgs + `}`, `{"error":{"message":"temperature: Invalid input: expected number, received object","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","temperature":true,` + hiMsgs + `}`, `{"error":{"message":"temperature: Invalid input: expected number, received boolean","code":400},"user_id":"user_wwz"}`},
		{`{"model":"wwz-fast","stream":"x",` + hiMsgs + `}`, `{"error":{"message":"stream: Invalid input","code":400},"user_id":"user_wwz"}`},
	} {
		resp := ro.do("POST", "/api/v1/chat/completions", oaiAuth, c.body)
		if resp.StatusCode() != 400 || string(resp.Body()) != c.want || string(resp.Header.ContentType()) != "application/json" {
			t.Errorf("openrouter %s: %d %s", c.body, resp.StatusCode(), resp.Body())
		}
	}
	oc := "/inference/go/openai/v1/chat/completions"
	ocSess := map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}
	if resp := ro.do("POST", oc, ocSess, `{"model":"wwz-fast","foo":1,`+hiMsgs+`}`); resp.StatusCode() != 200 {
		t.Errorf("opencode unknown field: %d %s", resp.StatusCode(), resp.Body())
	}
	if resp := ro.do("POST", oc, ocSess, `{"model": "x", `); resp.StatusCode() != 400 || len(resp.Body()) != 0 {
		t.Errorf("opencode bad json: %d %q", resp.StatusCode(), resp.Body())
	}
	want := "{\"error\":{\"message\":\"`model` must be a non-empty string\",\"type\":\"invalid_request_error\",\"param\":\"model\",\"code\":null}}"
	if resp := ro.do("POST", oc, ocSess, `{"model":5,`+hiMsgs+`}`); resp.StatusCode() != 400 || string(resp.Body()) != want {
		t.Errorf("opencode model: %d %s", resp.StatusCode(), resp.Body())
	}
	// A prompt stands in for messages; models for model.
	for _, body := range []string{`{"model":"m","prompt":"hi"}`, `{"models":["m"],"messages":[{}]}`} {
		if err := validateAs(t, "openrouter", nil, driver.KindChat, body, nil); err != nil {
			t.Errorf("openrouter %s: %+v", body, err)
		}
	}
	// Other kinds are not checked by the gateway validators.
	for _, name := range []string{"openrouter", "opencode"} {
		if err := validateAs(t, name, nil, driver.KindMessages, `x`, nil); err != nil {
			t.Errorf("%s messages: %+v", name, err)
		}
	}
}

// TestWithValidationKeepsInterfaces: the wrapper exposes exactly the
// optional interfaces of the driver it wraps.
func TestWithValidationKeepsInterfaces(t *testing.T) {
	for _, name := range driver.Names() {
		drv, err := driver.New(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		w := withValidation(drv, nil)
		_, m1 := drv.(driver.IDMinter)
		_, m2 := w.(driver.IDMinter)
		_, n1 := drv.(driver.ModelNamer)
		_, n2 := w.(driver.ModelNamer)
		_, s1 := drv.(slugRouter)
		_, s2 := w.(slugRouter)
		if m1 != m2 || n1 != n2 || s1 != s2 {
			t.Fatalf("%s: IDMinter %v->%v ModelNamer %v->%v slugRouter %v->%v", name, m1, m2, n1, n2, s1, s2)
		}
		if driver.ConventionsOf(w) != driver.ConventionsOf(drv) {
			t.Fatalf("%s: Conventions differ", name)
		}
		if m1 && w.(driver.IDMinter).RequestID(driver.KindChat, "abc") != drv.(driver.IDMinter).RequestID(driver.KindChat, "abc") {
			t.Fatalf("%s: RequestID differs", name)
		}
		if n1 {
			mc := &config.ModelConfig{ID: "x"}
			a, _ := w.(driver.ModelNamer).UnprefixModel("white-wizard/x")
			b, _ := drv.(driver.ModelNamer).UnprefixModel("white-wizard/x")
			if w.(driver.ModelNamer).ModelName(mc) != drv.(driver.ModelNamer).ModelName(mc) || a != b {
				t.Fatalf("%s: ModelNamer differs", name)
			}
		}
		if s1 && w.(slugRouter).ModelPathBySlug("/api/v1/model/a/b") != drv.(slugRouter).ModelPathBySlug("/api/v1/model/a/b") {
			t.Fatalf("%s: slugRouter differs", name)
		}
	}
}

// plainDriver hides the IDMinter of the driver it wraps and keeps its
// validator reachable (driver.As).
type plainDriver struct{ driver.Driver }

func (p plainDriver) Unwrap() driver.Driver { return p.Driver }

// otherDriver hides every optional interface, the validator included.
type otherDriver struct{ driver.Driver }

func TestWithValidationPlainDriver(t *testing.T) {
	drv, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := withValidation(plainDriver{drv}, nil)
	if _, ok := w.(driver.IDMinter); ok {
		t.Fatal("wrapper invented an IDMinter")
	}
	if _, perr := w.ParseRequest(driver.KindChat, []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"x":1}`), driver.MapHeaders{}); perr == nil || perr.Message != "Unrecognized request argument supplied: x" {
		t.Fatalf("got %+v", perr)
	}
	if _, perr := w.ParseRequest(driver.KindModels, []byte(`{"x":1}`), driver.MapHeaders{}); perr != nil && strings.Contains(perr.Message, "Unrecognized") {
		t.Fatalf("kind without rules was checked: %+v", perr)
	}
	m := withValidation(drv, nil).(driver.IDMinter)
	if id := m.ToolCallID(driver.KindChat, generate.NewRNG(1)); !strings.HasPrefix(id, "call_") {
		t.Fatalf("tool call id %q", id)
	}
	if id := m.ResponseID(driver.KindChat, generate.NewRNG(1), time.Time{}); id == "" {
		t.Fatal("empty response id")
	}
	o := otherDriver{drv}
	if withValidation(o, nil) != driver.Driver(o) {
		t.Fatal("driver without validator was wrapped")
	}
	if err := validateAs(t, "anthropic", nil, driver.KindModels, "", nil); err != nil {
		t.Fatalf("anthropic models: %+v", err)
	}
}

func strictTool(name string, strict bool, params string) string {
	return `{"type":"function","function":{"name":"` + name + `","strict":` + strconv.FormatBool(strict) + `,"parameters":` + params + `}}`
}

const (
	apMsg  = "'additionalProperties' is required to be supplied and to be false."
	reqMsg = "'required' is required to be supplied and to be an array including every key in properties. Missing '"
)

func TestStrictSchemasOpenAI(t *testing.T) {
	h := start(t, fastCfg, nil)
	const (
		good    = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`
		noAP    = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
		partReq = `{"type":"object","properties":{"city":{"type":"string"},"unit":{"type":"string"}},"required":["city"],"additionalProperties":false}`
		deep    = `{"type":"object","properties":{"loc":{"type":"object","properties":{"lat":{"type":"number"}},"required":[],"additionalProperties":false}},"required":["loc"],"additionalProperties":false}`
	)
	chat := func(extra string) string {
		return `{"model":"wwz-fast",` + hiMsgs + `,` + extra + `}`
	}
	rf := func(strict bool, schema string) string {
		return `"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":` + strconv.FormatBool(strict) + `,"schema":` + schema + `}}`
	}
	accepted := []string{
		chat(`"tools":[` + strictTool("w", true, good) + `]`),
		chat(`"tools":[` + strictTool("w", false, noAP) + `]`),
		chat(rf(true, good)),
		chat(rf(false, noAP)),
	}
	for _, body := range accepted {
		if resp := h.do("POST", "/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", body, resp.StatusCode(), resp.Body())
		}
	}
	cases := []struct{ body, param, code, msg string }{
		{
			chat(`"temperature":"x","tools":[` + strictTool("w", true, noAP) + `]`), "tools[0].function.parameters", "invalid_function_parameters",
			"Invalid schema for function 'w': In context=(), " + apMsg,
		},
		{
			chat(`"tools":[` + strictTool("a", false, noAP) + `,` + strictTool("b", true, good) + `,` + strictTool("c", true, partReq) + `,` + strictTool("d", true, noAP) + `]`),
			"tools[2].function.parameters", "invalid_function_parameters",
			"Invalid schema for function 'c': In context=(), " + reqMsg + "unit'.",
		},
		{
			chat(`"tools":[` + strictTool("w", true, deep) + `]`), "tools[0].function.parameters", "invalid_function_parameters",
			"Invalid schema for function 'w': In context=('properties', 'loc'), " + reqMsg + "lat'.",
		},
		{
			chat(`"tools":[` + strictTool("w", true, good) + `],` + rf(true, noAP)), "response_format", "<nil>",
			"Invalid schema for response_format 'out': In context=(), " + apMsg,
		},
		{
			chat(rf(true, partReq)), "response_format", "<nil>",
			"Invalid schema for response_format 'out': In context=(), " + reqMsg + "unit'.",
		},
	}
	for _, c := range cases {
		resp := h.do("POST", "/v1/chat/completions", oaiAuth, c.body)
		var out oaiEnvelope
		if resp.StatusCode() != 400 || json.Unmarshal(resp.Body(), &out) != nil {
			t.Fatalf("%s: %d %s", c.body, resp.StatusCode(), resp.Body())
		}
		e := out.Error
		if e.Message != c.msg || e.Type != "invalid_request_error" || str(e.Param) != c.param || str(e.Code) != c.code {
			t.Fatalf("%s:\n%s", c.body, resp.Body())
		}
	}
}

func TestStrictSchemasOtherDrivers(t *testing.T) {
	noAP := `{"type":"object","properties":{"city":{"type":"string"}}}`
	h := start(t, fastCfg, nil)
	an := `{"model":"wwz-fast","max_tokens":10,` + hiMsgs + `,"tools":[{"name":"w","strict":true,"input_schema":` + noAP + `}]}`
	if resp := h.do("POST", "/v1/messages", anthAuth, an); resp.StatusCode() != 200 {
		t.Fatalf("anthropic: %d %s", resp.StatusCode(), resp.Body())
	}
	// /v1/completions has no tools field: the unknown-field check answers.
	resp := h.do("POST", "/v1/completions", oaiAuth, `{"model":"wwz-fast","prompt":"hi","tools":[`+strictTool("w", true, noAP)+`]}`)
	if resp.StatusCode() != 400 || !strings.Contains(string(resp.Body()), "Unrecognized request argument supplied: tools") {
		t.Fatalf("completions: %d %s", resp.StatusCode(), resp.Body())
	}
}

// TestRawReplyFallsBack: an error without a raw reply goes to the driver.
func TestRawReplyFallsBack(t *testing.T) {
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := withValidation(drv, nil)
	mw := core.NewMemWriter()
	w.WriteError(mw, nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "x"}, nil)
	if mw.StatusCode() != 400 || !strings.Contains(string(mw.Body()), `"metadata"`) {
		t.Fatalf("driver envelope not used: %d %s", mw.StatusCode(), mw.Body())
	}
}
