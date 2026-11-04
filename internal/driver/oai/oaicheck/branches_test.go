package oaicheck

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

const (
	goodSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`
	noAP       = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
)

// TestValidateOpenAIAccepts: valid values of every checked field pass (the
// HTTP twin is internal/server's TestValidationAlwaysOn).
func TestValidateOpenAIAccepts(t *testing.T) {
	chat := func(extra string) string { return `{"model":"wwz-fast",` + hiMsgs + `,` + extra + `}` }
	for _, c := range []struct {
		kind driver.Kind
		body string
	}{
		{driver.KindChat, chat(`"temperature":0.2,"top_p":1,"stop":null,"user":null,"stream":false`)},
		{driver.KindChat, chat(`"stream":true,"stream_options":{"include_usage":true}`)},
		{driver.KindChat, chat(`"logprobs":true,"top_logprobs":2,"metadata":{"a":"b"},"logit_bias":{"1":-1}`)},
		{driver.KindChat, chat(`"tool_choice":"auto","tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":` + goodSchema + `}}]`)},
		{driver.KindChat, chat(`"tool_choice":{"type":"function"},"function_call":"auto","response_format":{"type":"json_schema","json_schema":{"name":"o","strict":true,"schema":` + goodSchema + `}}`)},
		{driver.KindChat, chat(`"response_format":{"type":"json_schema","json_schema":{"name":"o","strict":false,"schema":` + noAP + `}}`)},
		{driver.KindChat, chat(`"n":2,"stop":["a"],"modalities":["text"],"service_tier":"auto","prompt_cache_retention":"24h"`)},
		{driver.KindChat, `{"model":"wwz-fast","messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"user","content":[{"type":"text","text":"x"}]}]}`},
		{driver.KindCompletions, `{"model":"wwz-fast","prompt":"hi","echo":false}`},
		{driver.KindResponses, `{"model":"gpt","input":"x","temperature":1.5,"top_p":0.5,"stream":true,"metadata":{"a":"b"},"text":{"format":{}},"conversation":null}`},
		{driver.KindResponses, `{"model":"gpt","input":[],"tools":[{"type":"web_search"},{"type":"function","name":"f"},{"type":"function","name":"g","strict":true,"parameters":` + goodSchema + `}]}`},
		{driver.KindModels, `x`},
	} {
		if e := Validate(models{}, c.kind, []byte(c.body)); e != nil {
			t.Errorf("%s %s: %+v", c.kind, c.body, e)
		}
	}
}

// TestValidateOpenAIRefusals covers what the server's tables reach only
// over HTTP: the model checks, the strict schemas and the Responses API
// (internal/server's TestValidateResponsesRules and TestStrictSchemasOpenAI).
func TestValidateOpenAIRefusals(t *testing.T) {
	head := `{"model":"gpt","input":"x",`
	cases := []struct {
		kind                   driver.Kind
		body, param, code, msg string
		ek                     core.ErrorKind
	}{
		{driver.KindChat, `{"model":null,` + hiMsgs + `}`, "", "", "you must provide a model parameter", core.ErrInvalidRequest},
		{driver.KindChat, `{"model":"nope",` + hiMsgs + `}`, "", "", "The model `nope` does not exist or you do not have access to it.", core.ErrModelNotFound},
		{driver.KindChat, `{"model":"m",` + hiMsgs + `,"tools":[{"type":"function","function":{"name":"w","strict":true,"parameters":` + noAP + `}}]}`, "tools[0].function.parameters", "invalid_function_parameters", "Invalid schema for function 'w': In context=(), 'additionalProperties' is required to be supplied and to be false.", core.ErrInvalidRequest},
		{driver.KindChat, `{"model":"m",` + hiMsgs + `,"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":` + noAP + `}}}`, "response_format", "", "Invalid schema for response_format 'out': In context=(), 'additionalProperties' is required to be supplied and to be false.", core.ErrInvalidRequest},
		{driver.KindResponses, `{`, "", "invalid_json", oai.ResponsesBadJSON, core.ErrInvalidRequest},
		{driver.KindResponses, `[1]`, "", "invalid_json", oai.ResponsesBadJSON, core.ErrInvalidRequest},
		{driver.KindResponses, `{"model":"gpt",` + hiMsgs + `}`, "", "unsupported_parameter", responsesMessagesMoved, core.ErrInvalidRequest},
		{driver.KindResponses, `{"input":"x"}`, "model", "missing_required_parameter", "Missing required parameter: 'model'.", core.ErrInvalidRequest},
		{driver.KindResponses, `{"model":1,"input":"x"}`, "model", "invalid_type", "Invalid type for 'model': expected a string, but got an integer instead.", core.ErrInvalidRequest},
		{driver.KindResponses, `{"model":"gpt"}`, "input", "missing_required_parameter", "Missing required parameter: 'input'.", core.ErrInvalidRequest},
		{driver.KindResponses, `{"model":"gpt","input":1}`, "input", "invalid_type", "Invalid type for 'input': expected one of a string or array of input items, but got an integer instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"foo":1}`, "foo", "unknown_parameter", "Unknown parameter: 'foo'.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"temperature":-1}`, "temperature", "decimal_below_min_value", "Invalid 'temperature': decimal below minimum value. Expected a value >= 0.0, but got -1 instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"temperature":3}`, "temperature", "decimal_above_max_value", "Invalid 'temperature': decimal above maximum value. Expected a value <= 2.0, but got 3 instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"top_p":"a"}`, "top_p", "invalid_type", "Invalid type for 'top_p': expected a decimal, but got a string instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"tools":{}}`, "tools", "invalid_type", "Invalid type for 'tools': expected an array of objects, but got an object instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"tools":[1]}`, "tools[0]", "invalid_type", "Invalid type for 'tools[0]': expected an object, but got an integer instead.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"tools":[{"name":"f"}]}`, "tools[0].type", "missing_required_parameter", "Missing required parameter: 'tools[0].type'.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"tools":[{"type":"web_search"},{"type":"function"}]}`, "tools[1].name", "missing_required_parameter", "Missing required parameter: 'tools[1].name'.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"tools":[{"type":"function","name":"f","strict":true,"parameters":` + noAP + `}]}`, "tools[0].parameters", "invalid_function_parameters", "Invalid schema for function 'f': In context=(), 'additionalProperties' is required to be supplied and to be false.", core.ErrInvalidRequest},
		{driver.KindResponses, head + `"conversation":{"id":"conv_1"}}`, "", "", "Conversation with id 'conv_1' not found.", core.ErrNotFound},
		{driver.KindResponses, head + `"conversation":"conv_2"}`, "", "", "Conversation with id 'conv_2' not found.", core.ErrNotFound},
		{driver.KindResponses, head + `"prompt":{"id":"pmpt_1"}}`, "prompt", "unsupported_parameter", "Unsupported parameter: 'prompt'.", core.ErrInvalidRequest},
		{driver.KindResponses, `{"model":"nope","input":"x"}`, "", "", "The model `nope` does not exist or you do not have access to it.", core.ErrModelNotFound},
	}
	for _, c := range cases {
		e := Validate(models{}, c.kind, []byte(c.body))
		if e == nil || e.Kind != c.ek || e.Param != c.param || e.Code != c.code || e.Message != c.msg {
			t.Errorf("%s\n got  %+v\n want %s %s %q", c.body, e, c.param, c.code, c.msg)
		}
	}
	// The 404s of the Responses API come from the API, not the edge.
	if e := Validate(models{}, driver.KindResponses, []byte(`{"model":"nope","input":"x"}`)); e == nil || e.Extra[oai.ExtraAPILayout] != true {
		t.Errorf("unknown model layout: %+v", e)
	}
	if e := Validate(models{}, driver.KindResponses, []byte(`{"model":"old","input":"x"}`)); e == nil {
		t.Error("retired model accepted")
	}
}
