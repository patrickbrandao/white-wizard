package oaicheck

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The cases of internal/server's TestValidationOpenAI (task 388: the
// validator moved here), run on the validator itself; the server test keeps
// them over HTTP, through the driver's envelope.

const hiMsgs = `"messages":[{"role":"user","content":"hi"}]`

// models knows every model but those named nope*, and old is retired.
type models struct{}

func (models) KnownModel(kind driver.Kind, name string) bool {
	known, _ := models{}.Model(kind, name)
	return known
}

func (models) Model(_ driver.Kind, name string) (known, retired bool) {
	if name == "old" {
		return false, true
	}
	return !strings.HasPrefix(name, "nope"), false
}

func kindOf(path string) driver.Kind {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return driver.KindChat
	case strings.HasSuffix(path, "/completions"):
		return driver.KindCompletions
	}
	return driver.KindResponses
}

func nilStr(s string) string {
	if s == "" {
		return "<nil>"
	}
	return s
}

func TestValidateOpenAICases(t *testing.T) {
	chat := func(extra string) string { return `{"model":"wwz-fast",` + hiMsgs + `,` + extra + `}` }
	cases := []struct{ path, body, msg, param, code string }{
		{"/v1/chat/completions", `{"model":`, BadJSON, "<nil>", "<nil>"},
		{"/v1/chat/completions", `[1]`, BadJSON, "<nil>", "<nil>"},
		{"/v1/chat/completions", `{"model":5,` + hiMsgs + `}`, BadJSON, "<nil>", "<nil>"},
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
		e := Validate(models{}, kindOf(c.path), []byte(c.body))
		if e == nil {
			t.Errorf("%s: accepted", c.body)
			continue
		}
		if e.Message != c.msg || nilStr(e.Param) != c.param || nilStr(e.Code) != c.code {
			t.Errorf("%s:\n got  %q %s %s\n want %q %s %s", c.body, e.Message, e.Param, e.Code, c.msg, c.param, c.code)
		}
	}
}
