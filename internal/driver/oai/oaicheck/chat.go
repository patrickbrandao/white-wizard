// Package oaicheck validates request bodies as the OpenAI API does (spec
// 004, ADRs 0029 and 0042): Chat Completions, the legacy completions and the
// Responses API, with the real status, envelope and text. It is shared by
// the drivers that speak OpenAI's API (openai, azure); the core never
// imports it (spec 005).
package oaicheck

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	js "github.com/patrickbrandao/white-wizard/internal/driver/jsonscan"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/structured"
)

// OpenAI (driver openai). Order the real API checks a Chat Completions body
// in, as recorded on 2026-10-31 (testdata/real/openai/validation/):
//  1. the body is a JSON object and model, when set, a string;
//  2. model is present and not empty;
//  3. the model exists (404 model_not_found);
//  4. messages, element by element;
//  5. each known field, in openAIChatOrder, type then range then contents;
//  6. the cross-field rules (top_logprobs, stream_options);
//  7. unknown top-level fields, all of them, sorted.

// BadJSON is the text of the 400 for a body that is not JSON.
const BadJSON = "We could not parse the JSON body of your request. (HINT: This likely means you aren't using your HTTP library correctly. The OpenAI API expects a JSON payload, but what was sent was not valid JSON. If you have trouble figuring out how to fix this, please contact us through our help center at help.openai.com.)"

// openAIRoles are the message roles in the order the API lists them.
var openAIRoles = []string{"system", "assistant", "user", "function", "tool", "developer"}

// openAIField checks one top-level field that is set and not null.
type openAIField struct {
	name  string
	check func(name string, v []byte) *core.Error
}

// openAIChatOrder is the order the real API reports field errors in.
var openAIChatOrder = []openAIField{
	{"functions", oaiArray("an array of function definitions")},
	{"tools", oaiTools},
	{"function_call", oaiChoice("one of 'none' or 'auto'", "name")},
	{"tool_choice", oaiChoice("one of 'none', 'auto', or 'required'", "type")},
	{"parallel_tool_calls", oaiBool},
	{"max_tokens", oaiInt(nil, nil)},
	{"max_completion_tokens", oaiInt(nil, nil)},
	{"temperature", oaiDecimal(0, 2)},
	{"top_p", oaiDecimal(0, 1)},
	{"logit_bias", oaiMap("an object", "a decimal", js.IsNumber)},
	{"user", oaiString},
	{"safety_identifier", oaiString},
	{"prompt_cache_key", oaiString},
	{"prompt_cache_retention", oaiEnum("one of 'in_memory' or '24h'")},
	{"n", oaiInt(ptr(1), ptr(128))},
	{"stop", oaiStop},
	{"stream", oaiBool},
	{"presence_penalty", oaiDecimal(-2, 2)},
	{"frequency_penalty", oaiDecimal(-2, 2)},
	{"seed", oaiInt(nil, nil)},
	{"modalities", oaiArray("an array of one of 'text' or 'audio'")},
	{"response_format", oaiResponseFormat},
	{"audio", oaiObject("format")},
	{"logprobs", oaiBool},
	{"top_logprobs", oaiInt(nil, nil)},
	{"stream_options", oaiClosedObject("include_usage", "include_obfuscation")},
	{"service_tier", oaiEnum("one of 'auto', 'default', 'fast', 'flex', or 'priority'")},
	{"web_search_options", oaiClosedObject("search_context_size", "user_location")},
	{"store", oaiBool},
	{"metadata", oaiMap("a metadata object", "a string", func(k js.Kind) bool { return k == js.String })},
	{"prediction", oaiObject("type")},
}

// openAIChatFields are the top-level fields Chat Completions accepts:
// openAIChatOrder plus the fields whose acceptance depends on the model
// (reasoning_effort, verbosity), which the simulator always accepts.
var openAIChatFields = func() map[string]struct{} {
	m := js.Set("model", "messages", "reasoning_effort", "verbosity")
	for _, f := range openAIChatOrder {
		m[f.name] = struct{}{}
	}
	return m
}()

// openAICompletionsFields are the top-level fields of /v1/completions. Not
// conferred against the real API: every completions model was retired.
var openAICompletionsFields = js.Set(
	"model", "prompt", "best_of", "echo", "frequency_penalty", "logit_bias", "logprobs",
	"max_tokens", "n", "presence_penalty", "seed", "stop", "stream", "stream_options",
	"suffix", "temperature", "top_p", "user",
)

func ptr(f float64) *float64 { return &f }

// Validate is the OpenAI driver's request validation: Chat Completions,
// legacy completions and Responses; other kinds pass.
func Validate(models driver.Models, kind driver.Kind, body []byte) *core.Error {
	switch kind {
	case driver.KindChat:
		return openAIChat(models, body)
	case driver.KindCompletions:
		return openAICompletions(models, body)
	case driver.KindResponses:
		return openAIResponses(models, body)
	}
	return nil
}

// oaiErr is an OpenAI 400 invalid_request_error; param and code may be "".
func oaiErr(msg, param, code string) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg, Param: param, Code: code}
}

// openAIHead applies the checks every OpenAI body goes through first: a
// JSON object whose model is a non-empty string naming a served model. The
// real API answers an unknown model (404) before it looks at the rest of
// the body.
func openAIHead(models driver.Models, body []byte) ([]js.Field, *core.Error) {
	if !json.Valid(body) {
		return nil, oaiErr(BadJSON, "", "")
	}
	fs, ok := js.Members(body)
	if !ok {
		return nil, oaiErr(BadJSON, "", "")
	}
	model, has := js.Lookup(fs, "model")
	switch js.KindOf(model) {
	case js.String:
		if s, _ := js.StringValue(model); s != "" {
			if !models.KnownModel(driver.KindChat, s) {
				return nil, &core.Error{Kind: core.ErrModelNotFound, Message: "The model `" + s + "` does not exist or you do not have access to it."}
			}
			return fs, nil
		}
	case js.Null:
	default:
		if has {
			return nil, oaiErr(BadJSON, "", "")
		}
	}
	return nil, oaiErr("you must provide a model parameter", "", "")
}

func openAIChat(models driver.Models, body []byte) *core.Error {
	fs, err := openAIHead(models, body)
	if err != nil {
		return err
	}
	if err := oaiMessages(fs); err != nil {
		return err
	}
	for _, f := range openAIChatOrder {
		if v, ok := js.Present(fs, f.name); ok {
			if err := f.check(f.name, v); err != nil {
				return err
			}
		}
	}
	if _, ok := js.Present(fs, "top_logprobs"); ok {
		if v, _ := js.Lookup(fs, "logprobs"); string(js.TrimSpace(v)) != "true" {
			return oaiErr("The 'top_logprobs' parameter is only allowed when 'logprobs' is enabled.", "top_logprobs", "")
		}
	}
	if _, ok := js.Present(fs, "stream_options"); ok {
		if v, _ := js.Lookup(fs, "stream"); string(js.TrimSpace(v)) != "true" {
			return oaiErr("The 'stream_options' parameter is only allowed when 'stream' is enabled.", "stream_options", "")
		}
	}
	return oaiUnknown(fs, openAIChatFields)
}

// openAICompletions checks /v1/completions: the common head, then unknown
// fields. Not conferred (see openAICompletionsFields).
func openAICompletions(models driver.Models, body []byte) *core.Error {
	fs, err := openAIHead(models, body)
	if err != nil {
		return err
	}
	return oaiUnknown(fs, openAICompletionsFields)
}

// oaiUnknown reports every top-level field outside allowed, sorted the way
// the API sorts them (byte order: upper case first).
func oaiUnknown(fs []js.Field, allowed map[string]struct{}) *core.Error {
	var unknown []string
	for _, f := range fs {
		if _, ok := allowed[f.Key]; !ok {
			unknown = append(unknown, f.Key)
		}
	}
	switch len(unknown) {
	case 0:
		return nil
	case 1:
		return oaiErr("Unrecognized request argument supplied: "+unknown[0], "", "")
	}
	sort.Strings(unknown)
	return oaiErr("Unrecognized request arguments supplied: "+strings.Join(unknown, ", "), "", "")
}

// oaiGot names a value's type the way the API's "but got ... instead" does.
func oaiGot(v []byte) string {
	switch js.KindOf(v) {
	case js.Null:
		return "null"
	case js.Bool:
		return "a boolean"
	case js.Int:
		return "an integer"
	case js.Float:
		return "a decimal number"
	case js.String:
		return "a string"
	case js.Array:
		return "an array"
	}
	return "an object"
}

func oaiType(name, want string, v []byte) *core.Error {
	return oaiErr("Invalid type for '"+name+"': expected "+want+", but got "+oaiGot(v)+" instead.", name, "invalid_type")
}

func oaiMissing(name string) *core.Error {
	return oaiErr("Missing required parameter: '"+name+"'.", name, "missing_required_parameter")
}

// oaiMessages checks messages: present, an array, not empty, and each
// element an object with a known role and content of the right type.
func oaiMessages(fs []js.Field) *core.Error {
	v, ok := js.Lookup(fs, "messages")
	if !ok {
		return oaiMissing("messages")
	}
	if js.KindOf(v) != js.Array {
		return oaiType("messages", "an array of objects", v)
	}
	msgs := js.Elements(v)
	if len(msgs) == 0 {
		return oaiErr("Invalid 'messages': empty array. Expected an array with minimum length 1, but got an empty array instead.", "messages", "empty_array")
	}
	for i, m := range msgs {
		p := "messages[" + strconv.Itoa(i) + "]"
		mf, ok := js.Members(m)
		if !ok {
			return oaiType(p, "an object", m)
		}
		role, has := js.Lookup(mf, "role")
		if !has {
			return oaiMissing(p + ".role")
		}
		r, isStr := js.StringValue(role)
		if !isStr {
			return oaiType(p+".role", "one of 'system', 'assistant', 'user', 'function', 'tool', or 'developer'", role)
		}
		known := false
		for _, k := range openAIRoles {
			known = known || k == r
		}
		if !known {
			return oaiErr("Invalid value: '"+r+"'. Supported values are: 'system', 'assistant', 'user', 'function', 'tool', and 'developer'.", p+".role", "invalid_value")
		}
		content, has := js.Lookup(mf, "content")
		switch js.KindOf(content) {
		case js.String, js.Array:
		case js.Null, js.Invalid:
			if r == "user" || r == "system" || r == "developer" {
				return oaiErr("Invalid value for 'content': expected a string, got null.", "messages.["+strconv.Itoa(i)+"].content", "")
			}
		default:
			if has {
				return oaiType(p+".content", "one of a string or array of objects", content)
			}
		}
	}
	return nil
}

func oaiBool(name string, v []byte) *core.Error {
	if js.KindOf(v) != js.Bool {
		return oaiType(name, "a boolean", v)
	}
	return nil
}

func oaiString(name string, v []byte) *core.Error {
	if js.KindOf(v) != js.String {
		return oaiType(name, "a string", v)
	}
	return nil
}

// oaiEnum checks the type of a string enum; the value itself is not checked.
func oaiEnum(want string) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		if js.KindOf(v) != js.String {
			return oaiType(name, want, v)
		}
		return nil
	}
}

func oaiArray(want string) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		if js.KindOf(v) != js.Array {
			return oaiType(name, want, v)
		}
		return nil
	}
}

func oaiStop(name string, v []byte) *core.Error {
	switch js.KindOf(v) {
	case js.String, js.Array:
		return nil
	}
	return oaiType(name, "one of a string or array of strings", v)
}

// oaiNumber renders a number the way the API echoes it.
func oaiNumber(v []byte) string { return string(js.TrimSpace(v)) }

func oaiBound(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// oaiDecimal checks a number within [lo, hi].
func oaiDecimal(lo, hi float64) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		f, ok := js.NumberValue(v)
		if !ok {
			return oaiType(name, "a decimal", v)
		}
		if f > hi {
			return oaiErr("Invalid '"+name+"': decimal above maximum value. Expected a value <= "+oaiBound(hi)+", but got "+oaiNumber(v)+" instead.", name, "decimal_above_max_value")
		}
		if f < lo {
			return oaiErr("Invalid '"+name+"': decimal below minimum value. Expected a value >= "+oaiBound(lo)+", but got "+oaiNumber(v)+" instead.", name, "decimal_below_min_value")
		}
		return nil
	}
}

// oaiInt checks an integer within the optional bounds.
func oaiInt(lo, hi *float64) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		if js.KindOf(v) != js.Int {
			return oaiType(name, "an integer", v)
		}
		f, _ := js.NumberValue(v)
		if hi != nil && f > *hi {
			return oaiErr("Invalid '"+name+"': integer above maximum value. Expected a value <= "+oaiBound(*hi)+", but got "+oaiNumber(v)+" instead.", name, "integer_above_max_value")
		}
		if lo != nil && f < *lo {
			return oaiErr("Invalid '"+name+"': integer below minimum value. Expected a value >= "+oaiBound(*lo)+", but got "+oaiNumber(v)+" instead.", name, "integer_below_min_value")
		}
		return nil
	}
}

// oaiObject checks an object that must carry the field required.
func oaiObject(required string) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		fs, ok := js.Members(v)
		if !ok {
			return oaiType(name, "an object", v)
		}
		if _, has := js.Lookup(fs, required); !has {
			return oaiMissing(name + "." + required)
		}
		return nil
	}
}

// oaiClosedObject checks an object whose keys are all in keys.
func oaiClosedObject(keys ...string) func(string, []byte) *core.Error {
	allowed := js.Set(keys...)
	return func(name string, v []byte) *core.Error {
		fs, ok := js.Members(v)
		if !ok {
			return oaiType(name, "an object", v)
		}
		for _, f := range fs {
			if _, ok := allowed[f.Key]; !ok {
				p := name + "." + f.Key
				return oaiErr("Unknown parameter: '"+p+"'.", p, "unknown_parameter")
			}
		}
		return nil
	}
}

// oaiMap checks an object whose values all satisfy valOK.
func oaiMap(want, valWant string, valOK func(js.Kind) bool) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		fs, ok := js.Members(v)
		if !ok {
			return oaiType(name, want, v)
		}
		for _, f := range fs {
			if !valOK(js.KindOf(f.Val)) {
				return oaiType(name+"."+f.Key, valWant, f.Val)
			}
		}
		return nil
	}
}

// oaiChoice checks tool_choice / function_call: a string or an object that
// carries the field required.
func oaiChoice(names, required string) func(string, []byte) *core.Error {
	return func(name string, v []byte) *core.Error {
		switch js.KindOf(v) {
		case js.String:
			return nil
		case js.Object:
			return oaiObject(required)(name, v)
		}
		return oaiType(name, "one of "+names+" or object", v)
	}
}

// oaiTools checks tools: an array whose functions with strict: true follow
// the Structured Outputs rules.
func oaiTools(name string, v []byte) *core.Error {
	if js.KindOf(v) != js.Array {
		return oaiType(name, "an array of objects", v)
	}
	for i, t := range js.Elements(v) {
		tf, _ := js.Members(t)
		fn, _ := js.Lookup(tf, "function")
		ff, _ := js.Members(fn)
		if s, _ := js.Lookup(ff, "strict"); string(js.TrimSpace(s)) != "true" {
			continue
		}
		params, _ := js.Lookup(ff, "parameters")
		if msg := structured.Violation(params); msg != "" {
			fname, _ := js.Lookup(ff, "name")
			n, _ := js.StringValue(fname)
			return oaiErr("Invalid schema for function '"+n+"': "+msg, "tools["+strconv.Itoa(i)+"].function.parameters", "invalid_function_parameters")
		}
	}
	return nil
}

// oaiResponseFormat checks response_format: an object with a type and, for
// a strict json_schema, a schema that follows the Structured Outputs rules.
// The real API sends code null here (unlike invalid_function_parameters).
func oaiResponseFormat(name string, v []byte) *core.Error {
	if err := oaiObject("type")(name, v); err != nil {
		return err
	}
	fs, _ := js.Members(v)
	schemaObj, _ := js.Lookup(fs, "json_schema")
	jf, _ := js.Members(schemaObj)
	if s, _ := js.Lookup(jf, "strict"); string(js.TrimSpace(s)) != "true" {
		return nil
	}
	schema, _ := js.Lookup(jf, "schema")
	if msg := structured.Violation(schema); msg != "" {
		n, _ := js.Lookup(jf, "name")
		sn, _ := js.StringValue(n)
		return oaiErr("Invalid schema for response_format '"+sn+"': "+msg, name, "")
	}
	return nil
}
