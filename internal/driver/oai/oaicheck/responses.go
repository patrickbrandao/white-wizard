package oaicheck

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	js "github.com/patrickbrandao/white-wizard/internal/driver/jsonscan"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/structured"
)

// OpenAI Responses API (POST /v1/responses), as recorded on 2026-10-31
// (testdata/real/openai/responses/, spec 004 "Responses API", ADR 0055).
// Every error carries param and code and comes in the API's layout (2
// spaces, application/json), an unparsable body and an unknown model
// included. Order conferred against the real API (order_* fixtures):
//  1. the body is a JSON object (invalid_json);
//  2. messages, the Chat Completions field (unsupported_parameter);
//  3. model present and a string, then input present and of the right type;
//  4. the first unknown top-level field, in body order;
//  5. max_output_tokens, then the other known fields;
//  6. tools with strict: true;
//  7. the model exists (404), then the conversation (404, never found: the
//     wwz keeps no conversations, ADR 0055).

// openAIResponsesFields are the top-level fields the Responses API accepts.
var openAIResponsesFields = js.Set(
	"model", "input", "instructions", "max_output_tokens", "temperature", "top_p", "top_logprobs",
	"tools", "tool_choice", "parallel_tool_calls", "max_tool_calls", "text", "reasoning", "store",
	"previous_response_id", "include", "metadata", "prompt_cache_key", "prompt_cache_retention",
	"prompt_cache_options", "safety_identifier", "service_tier", "stream", "stream_options",
	"truncation", "user", "background", "conversation", "prompt",
)

// openAIResponsesOrder are the type and range checks of the known fields:
// conferred for max_output_tokens, temperature, top_p and stream (type_*,
// range_*); the others reuse the texts of Chat Completions (not conferred).
var openAIResponsesOrder = []openAIField{
	{"max_output_tokens", oaiInt(ptr(16), nil)},
	{"instructions", oaiString},
	{"temperature", respDecimal(0, 2)},
	{"top_p", respDecimal(0, 1)},
	{"top_logprobs", oaiInt(ptr(0), ptr(20))},
	{"parallel_tool_calls", oaiBool},
	{"max_tool_calls", oaiInt(nil, nil)},
	{"store", oaiBool},
	{"stream", oaiBool},
	{"background", oaiBool},
	{"previous_response_id", oaiString},
	{"prompt_cache_key", oaiString},
	{"prompt_cache_retention", oaiEnum("one of 'in_memory' or '24h'")},
	{"safety_identifier", oaiString},
	{"user", oaiString},
	{"service_tier", oaiEnum("one of 'auto', 'default', 'flex', 'scale', or 'priority'")},
	{"metadata", oaiMap("a metadata object", "a string", func(k js.Kind) bool { return k == js.String })},
	{"include", oaiArray("an array of strings")},
	{"text", oaiClosedObject("format", "verbosity")},
	{"reasoning", oaiClosedObject("effort", "summary", "generate_summary")},
	{"stream_options", oaiClosedObject("include_obfuscation")},
}

// The messages field of Chat Completions, moved to input (chat_messages_field).
const responsesMessagesMoved = "Unsupported parameter: 'messages'. In the Responses API, this parameter has moved to 'input'. Try again with the new parameter. See the API documentation for more information: https://platform.openai.com/docs/api-reference/responses/create."

func openAIResponses(models driver.Models, body []byte) *core.Error {
	err := openAIResponsesCheck(models, body)
	if err != nil && (err.Kind == core.ErrModelNotFound || err.Kind == core.ErrNotFound) {
		// The 404s of the Responses API come from the API, not the edge.
		err.Extra = map[string]any{oai.ExtraAPILayout: true}
	}
	return err
}

func openAIResponsesCheck(models driver.Models, body []byte) *core.Error {
	fs, name, err := ResponsesBody(body)
	if err != nil {
		return err
	}
	if known, retired := models.Model(driver.KindResponses, name); retired {
		return &core.Error{Kind: core.ErrModelNotFound, Message: "The model `" + name + "` has been deprecated, learn more here: https://platform.openai.com/docs/deprecations", Extra: map[string]any{oai.ExtraAPILayout: true}}
	} else if !known {
		return &core.Error{Kind: core.ErrModelNotFound, Message: "The model `" + name + "` does not exist or you do not have access to it."}
	}
	return ResponsesTail(fs)
}

// ResponsesBody runs the checks 1 to 6 of the order above and returns the
// members of the body and the model name.
func ResponsesBody(body []byte) ([]js.Field, string, *core.Error) {
	fs, ok := js.Members(body)
	if !ok || !json.Valid(body) {
		return nil, "", oaiErr(oai.ResponsesBadJSON, "", "invalid_json")
	}
	if _, has := js.Lookup(fs, "messages"); has {
		return nil, "", oaiErr(responsesMessagesMoved, "", "unsupported_parameter")
	}
	model, has := js.Present(fs, "model")
	if !has {
		return nil, "", oaiMissing("model")
	}
	name, isStr := js.StringValue(model)
	if !isStr {
		return nil, "", oaiType("model", "a string", model)
	}
	in, has := js.Present(fs, "input")
	if !has {
		return nil, "", oaiMissing("input")
	}
	if k := js.KindOf(in); k != js.String && k != js.Array {
		return nil, "", oaiType("input", "one of a string or array of input items", in)
	}
	for _, f := range fs {
		if _, known := openAIResponsesFields[f.Key]; !known {
			return nil, "", oaiErr("Unknown parameter: '"+f.Key+"'.", f.Key, "unknown_parameter")
		}
	}
	for _, f := range openAIResponsesOrder {
		if val, set := js.Present(fs, f.name); set {
			if err := f.check(f.name, val); err != nil {
				return nil, "", err
			}
		}
	}
	if tools, set := js.Present(fs, "tools"); set {
		if err := responsesTools(tools); err != nil {
			return nil, "", err
		}
	}
	return fs, name, nil
}

// ResponsesTail runs the checks after the model (7): the conversation,
// then the prompt template.
func ResponsesTail(fs []js.Field) *core.Error {
	if c, set := js.Present(fs, "conversation"); set {
		id, _ := js.StringValue(c)
		if cf, ok := js.Members(c); ok { // {id: ...}
			idv, _ := js.Lookup(cf, "id")
			id, _ = js.StringValue(idv)
		}
		return &core.Error{Kind: core.ErrNotFound, Message: "Conversation with id '" + id + "' not found."}
	}
	if _, set := js.Present(fs, "prompt"); set {
		// Prompt templates are not simulated (ADR 0055; text not conferred).
		return oaiErr("Unsupported parameter: 'prompt'.", "prompt", "unsupported_parameter")
	}
	return nil
}

// respDecimal checks a number within [lo, hi], the bound printed as a
// Python float (range_temperature: "Expected a value <= 2.0").
func respDecimal(lo, hi float64) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		f, ok := js.NumberValue(v)
		if !ok {
			return oaiType(name, "a decimal", v)
		}
		if f > hi {
			return oaiErr("Invalid '"+name+"': decimal above maximum value. Expected a value <= "+pyBound(hi)+", but got "+oaiNumber(v)+" instead.", name, "decimal_above_max_value")
		}
		if f < lo {
			return oaiErr("Invalid '"+name+"': decimal below minimum value. Expected a value >= "+pyBound(lo)+", but got "+oaiNumber(v)+" instead.", name, "decimal_below_min_value")
		}
		return nil
	}
}

// pyBound prints a bound as a Python float (2.0, 0.5).
func pyBound(f float64) string {
	s := oaiBound(f)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// responsesTools checks tools: an array of objects with a type, whose
// functions with strict: true follow the Structured Outputs rules
// (strict_true_no_additional; omitted strict is normalized, not refused).
func responsesTools(v []byte) *core.Error {
	if js.KindOf(v) != js.Array {
		return oaiType("tools", "an array of objects", v)
	}
	for i, t := range js.Elements(v) {
		p := "tools[" + strconv.Itoa(i) + "]"
		tf, ok := js.Members(t)
		if !ok {
			return oaiType(p, "an object", t)
		}
		typ, has := js.Lookup(tf, "type")
		if !has {
			return oaiMissing(p + ".type")
		}
		if s, _ := js.StringValue(typ); s != "function" {
			continue
		}
		name, has := js.Lookup(tf, "name")
		n, _ := js.StringValue(name)
		if !has || n == "" {
			return oaiMissing(p + ".name")
		}
		if s, _ := js.Lookup(tf, "strict"); string(js.TrimSpace(s)) != "true" {
			continue
		}
		params, _ := js.Lookup(tf, "parameters")
		if msg := structured.Violation(params); msg != "" {
			return oaiErr("Invalid schema for function '"+n+"': "+msg, p+".parameters", "invalid_function_parameters")
		}
	}
	return nil
}
