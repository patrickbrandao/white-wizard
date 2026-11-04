package server

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// TestValidateResponsesRules covers the checks the fixtures do not reach:
// the texts not conferred keep the shape of the conferred ones.
func TestValidateResponsesRules(t *testing.T) {
	cfg, err := config.Parse([]byte("models:\n  - {id: gpt-4o-mini}\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := &validation{cfg: cfg, protocol: "openai"}
	head := `{"model":"gpt-4o-mini","input":"x",`
	cases := []struct {
		body, param, code, msg string
		kind                   core.ErrorKind
	}{
		{head + `"temperature":-1}`, "temperature", "decimal_below_min_value", "Invalid 'temperature': decimal below minimum value. Expected a value >= 0.0, but got -1 instead.", core.ErrInvalidRequest},
		{head + `"top_p":"a"}`, "top_p", "invalid_type", "Invalid type for 'top_p': expected a decimal, but got a string instead.", core.ErrInvalidRequest},
		{head + `"tools":{}}`, "tools", "invalid_type", "Invalid type for 'tools': expected an array of objects, but got an object instead.", core.ErrInvalidRequest},
		{head + `"tools":[1]}`, "tools[0]", "invalid_type", "Invalid type for 'tools[0]': expected an object, but got an integer instead.", core.ErrInvalidRequest},
		{head + `"tools":[{"name":"f"}]}`, "tools[0].type", "missing_required_parameter", "Missing required parameter: 'tools[0].type'.", core.ErrInvalidRequest},
		{head + `"tools":[{"type":"web_search"},{"type":"function"}]}`, "tools[1].name", "missing_required_parameter", "Missing required parameter: 'tools[1].name'.", core.ErrInvalidRequest},
		{head + `"conversation":{"id":"conv_1"}}`, "", "", "Conversation with id 'conv_1' not found.", core.ErrNotFound},
		{head + `"prompt":{"id":"pmpt_1"}}`, "prompt", "unsupported_parameter", "Unsupported parameter: 'prompt'.", core.ErrInvalidRequest},
		{`[1]`, "", "invalid_json", oai.ResponsesBadJSON, core.ErrInvalidRequest},
	}
	for _, tc := range cases {
		e := validateAs(t, "openai", v, driver.KindResponses, tc.body, nil)
		if e == nil || e.Kind != tc.kind || e.Param != tc.param || e.Code != tc.code || e.Message != tc.msg {
			t.Errorf("%s\n got  %+v\n want %s %s %q", tc.body, e, tc.param, tc.code, tc.msg)
		}
	}
	ok := []string{
		head + `"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object","properties":{},"additionalProperties":false,"required":[]}}],"temperature":1.5,"conversation":null}`,
	}
	for _, b := range ok {
		if e := validateAs(t, "openai", v, driver.KindResponses, b, nil); e != nil {
			t.Errorf("%s: %+v", b, e)
		}
	}
	if e := validateAs(t, "openai", v, driver.KindResponses, `{"model":"nope","input":"x"}`, nil); e == nil || e.Extra[oai.ExtraAPILayout] != true {
		t.Errorf("unknown model: %+v", e)
	}
}
