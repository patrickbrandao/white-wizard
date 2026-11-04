package openrouter

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The Responses API of OpenRouter (POST /api/v1/responses), as recorded
// against the real gateway on 2026-10-31 (task 240;
// testdata/real/openrouter/responses/). It is a translation, not OpenAI's
// API: the gateway checks the body with its own zod schema, refuses state
// (store: true, previous_response_id), turns the request into a Chat
// Completions call to the upstream (an upstream error names
// tools[0].function.parameters; output_tokens counts like Chat, without
// the end token) and renders the answer in its own object and event
// stream. The request is therefore parsed into a Chat Completions request
// (the engine counts, caches and keeps no record as for Chat) and only the
// writers know it came through /responses (isResponses).

// responsesPath is the route.
const responsesPath = "/api/v1/responses"

// isResponses reports whether req came through the Responses route.
func isResponses(req *core.Request) bool {
	return req != nil && strings.HasSuffix(req.Header("x-wwz-path"), "/responses")
}

// extraRawReply carries an answer of the gateway's own validation, sent as
// is by errorBody.
const extraRawReply = "openrouter_raw_reply"

// rawReply is a validation answer: status, body and whether it carries a
// generation id (the checks past the schema do).
type rawReply struct {
	status     int
	body       string
	generation bool
}

func rawError(msg string, r rawReply) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg, Extra: map[string]any{extraRawReply: r}}
}

// GatewayError is the gateway's own 400 on Chat Completions, for the early
// validation of internal/server: {"error":{"message","code"},"user_id"},
// no metadata, with a generation id and the CORS list like every other
// answer (shape/error_validation.json).
func GatewayError(msg string) *core.Error {
	body := `{"error":{"message":` + jsonString(msg) + `,"code":400},"user_id":"user_wwz"}`
	return rawError(msg, rawReply{status: 400, body: body, generation: true})
}

// member is one top-level member of the body, in order.
type member struct {
	key string
	val json.RawMessage
}

// lenientMembers reads the top-level members of body the way the gateway
// does: a trailing comma is accepted (invalid_json) and a body cut in the
// middle keeps the members read so far, the cut one as null
// (invalid_json_truncated). A repeated key keeps its first place and its
// last value. kind is the JSON type of a body that is valid JSON but not
// an object ("" otherwise).
func lenientMembers(body []byte) (ms []member, kind string) {
	b := bytes.TrimSpace(body)
	if json.Valid(b) && (len(b) == 0 || b[0] != '{') {
		return nil, zodType(b)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, "" // not conferred: read as an empty object
	}
	put := func(k string, v json.RawMessage) {
		for i := range ms {
			if ms[i].key == k {
				ms[i].val = v
				return
			}
		}
		ms = append(ms, member{k, v})
	}
	null := json.RawMessage("null")
	i := 1
	for {
		i = skipSpace(b, i)
		if i >= len(b) || b[i] == '}' {
			return ms, ""
		}
		if b[i] == ',' {
			i++
			continue
		}
		j := stringEnd(b, i)
		if j < 0 {
			return ms, ""
		}
		var key string
		if json.Unmarshal(b[i:j], &key) != nil {
			return ms, ""
		}
		i = skipSpace(b, j)
		if i >= len(b) || b[i] != ':' {
			put(key, null)
			return ms, ""
		}
		dec := json.NewDecoder(bytes.NewReader(b[i+1:]))
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			put(key, null)
			return ms, ""
		}
		put(key, v)
		i += 1 + int(dec.InputOffset())
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// stringEnd is the index after the JSON string that starts at b[i], or -1.
func stringEnd(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

func lookup(ms []member, key string) (json.RawMessage, bool) {
	for _, m := range ms {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// zodType names the JSON type of v as zod's "received ..." does.
func zodType(v []byte) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return "undefined"
	}
	switch v[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

// zodIssue is one issue of the schema check, in zod's member order.
type zodIssue = oai.Obj

func typeIssue(expected, field string, v []byte) zodIssue {
	path := []any{}
	if field != "" {
		path = []any{field}
	}
	return oai.O("expected", expected, "code", "invalid_type", "path", path,
		"message", "Invalid input: expected "+expected+", received "+zodType(v))
}

// Custom messages of the stateless gateway (store_true,
// previous_response_id): they become the error's message.
const (
	storeMessage    = "store: true is not supported on this proxy. Each response request is independent."
	previousMessage = "previous_response_id is not supported on this proxy. Each response request is independent."
	invalidRequest  = "Invalid Responses API request"
)

// schemaIssues are the zod issues of the body (conferred: type_model,
// type_input, type_max_output_tokens, type_stream, range_temperature,
// store_true, previous_response_id, multiple_issues), in the schema's
// order. Fields the probes did not cover are not checked (not conferred).
// The message is the first custom one, else the generic text.
func schemaIssues(ms []member) ([]any, string) {
	var issues []any
	msg := ""
	custom := func(m string) {
		if msg == "" {
			msg = m
		}
	}
	number := func(field string) (float64, bool) {
		v, ok := lookup(ms, field)
		if !ok {
			return 0, false
		}
		var f float64
		if zodType(v) != "number" || json.Unmarshal(v, &f) != nil {
			issues = append(issues, typeIssue("number", field, v))
			return 0, false
		}
		return f, true
	}
	if v, ok := lookup(ms, "model"); ok && zodType(v) != "string" {
		issues = append(issues, typeIssue("string", "model", v))
	}
	if v, ok := lookup(ms, "input"); ok {
		if t := zodType(v); t != "string" && t != "array" {
			issues = append(issues, oai.O("code", "invalid_union", "errors", []any{
				[]any{typeIssue("string", "", v)}, []any{typeIssue("array", "", v)},
			}, "path", []any{"input"}, "message", "Invalid input"))
		}
	}
	number("max_output_tokens")
	if f, ok := number("temperature"); ok && f > 2 {
		issues = append(issues, oai.O("origin", "number", "code", "too_big", "maximum", 2, "inclusive", true,
			"path", []any{"temperature"}, "message", "Too big: expected number to be <=2"))
	}
	number("top_p")
	if v, ok := lookup(ms, "stream"); ok && zodType(v) != "boolean" {
		issues = append(issues, typeIssue("boolean", "stream", v))
	}
	if v, ok := lookup(ms, "store"); ok && string(bytes.TrimSpace(v)) == "true" {
		issues = append(issues, oai.O("code", "invalid_value", "values", []any{false}, "path", []any{"store"}, "message", storeMessage))
		custom(storeMessage)
	}
	if v, ok := lookup(ms, "previous_response_id"); ok && zodType(v) != "null" {
		issues = append(issues, oai.O("expected", "null", "code", "invalid_type", "path", []any{"previous_response_id"}, "message", previousMessage))
		custom(previousMessage)
	}
	if msg == "" {
		msg = invalidRequest
	}
	return issues, msg
}

// zodRaw is metadata.raw: JSON.stringify(issues, null, 2).
var zodRaw = oai.Format{Indent: "  ", RawHTML: true}

// promptError is the envelope of the Responses checks: error{code,
// message} and metadata (raw issues, or null), without user_id.
func promptError(msg string, issues []any) *core.Error {
	meta := "null"
	if len(issues) > 0 {
		raw, _ := zodRaw.Marshal(issues)
		meta = `{"raw":` + jsonString(string(raw)) + `}`
	}
	body := `{"error":{"code":"invalid_prompt","message":` + jsonString(msg) + `},"metadata":` + meta + `}`
	return rawError(msg, rawReply{status: 400, body: body})
}

// gatewayError is the gateway's Chat envelope ({error{message, code},
// user_id}), with a generation id: the checks past the schema.
func gatewayError(msg string) *core.Error {
	body := `{"error":{"message":` + jsonString(msg) + `,"code":400},"user_id":"user_wwz"}`
	return rawError(msg, rawReply{status: 400, body: body, generation: true})
}

// parseResponses checks a Responses body in the gateway's order and parses
// it into a Chat Completions request:
//  1. an empty body: "JSON parsing failed" (empty_body);
//  2. a body that is JSON but not an object: the schema's root issue
//     (non_object);
//  3. the schema (types, temperature <= 2, store, previous_response_id);
//  4. input absent or "": "No input provided" (missing_input,
//     input_empty_string, chat_messages_field); input [] is the Chat
//     check's text (input_empty_array);
//  5. model absent and no models: "No models provided" (missing_model;
//     models[0] stands for model, models_field).
//
// Unknown fields are ignored (unknown_param) and max_output_tokens has no
// minimum (max_output_tokens_below_min). The model itself is checked by
// the engine afterwards (order_type_before_unknown_model).
func (d *Driver) parseResponses(body []byte) (*core.Request, *core.Error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, rawError("JSON parsing failed", rawReply{status: 400, body: `{"error":{"message":"JSON parsing failed","code":400}}`})
	}
	ms, kind := lenientMembers(body)
	if kind != "" {
		issue := oai.O("expected", "object", "code", "invalid_type", "path", []any{}, "message", "Invalid input: expected object, received "+kind)
		return nil, promptError(invalidRequest, []any{issue})
	}
	if issues, msg := schemaIssues(ms); len(issues) > 0 {
		return nil, promptError(msg, issues)
	}
	input, ok := lookup(ms, "input")
	switch t := bytes.TrimSpace(input); {
	case !ok || string(t) == `""`:
		return nil, promptError("No input provided", nil)
	case string(t) == "[]":
		return nil, gatewayError(`Input required: specify "prompt" or "messages"`)
	}
	var model string
	if v, ok := lookup(ms, "model"); ok {
		_ = json.Unmarshal(v, &model)
	}
	if model == "" {
		var models []string
		if v, ok := lookup(ms, "models"); ok {
			_ = json.Unmarshal(v, &models)
		}
		if len(models) == 0 || models[0] == "" {
			return nil, gatewayError("No models provided")
		}
		model = models[0]
	}
	if v, ok := lookup(ms, "tools"); ok {
		if err := strictTools(v); err != nil {
			return nil, err
		}
	}
	norm := membersJSON(ms, model)
	req, err := oai.ParseResponses(norm, d.Name())
	if err != nil {
		return nil, err
	}
	req.Endpoint = core.EndpointChat // the gateway's translation (see above)
	return req, nil
}

// upstreamIndent is the layout of an upstream body quoted in
// metadata.raw: two spaces (strict_true_no_additional).
var upstreamIndent = oai.Format{Indent: "  ", RawHTML: true}

// strictTools is the upstream's refusal of a function with strict: true
// whose parameters do not set additionalProperties: false, passed through
// as "Provider returned error" (strict_true_no_additional; the upstream
// checks the Chat form, tools[i].function.parameters). Deeper schema
// rules of strict mode are not conferred.
func strictTools(raw json.RawMessage) *core.Error {
	var tools []struct {
		Type       string `json:"type"`
		Name       string `json:"name"`
		Strict     bool   `json:"strict"`
		Parameters struct {
			AdditionalProperties *bool `json:"additionalProperties"`
		} `json:"parameters"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	for i, t := range tools {
		if t.Type != "function" || !t.Strict || (t.Parameters.AdditionalProperties != nil && !*t.Parameters.AdditionalProperties) {
			continue
		}
		const code = "invalid_function_parameters"
		msg := "Invalid schema for function '" + t.Name + "': In context=(), 'additionalProperties' is required to be supplied and to be false."
		up, _ := upstreamIndent.Marshal(oai.O("error", oai.O("message", msg, "type", "invalid_request_error",
			"param", "tools["+strconv.Itoa(i)+"].function.parameters", "code", code)))
		meta, _ := jsonFormat.Marshal(oai.O("raw", string(up), "provider_name", providerName, "is_byok", false, "provider_error_code", code))
		body := `{"error":{"message":"Provider returned error","code":400,"metadata":` + string(meta) + `},"user_id":"user_wwz"}`
		return rawError(msg, rawReply{status: 400, body: body, generation: true})
	}
	return nil
}

// membersJSON is the normalized body: the members in order, model set to
// the one resolved (models[0] when model was missing).
func membersJSON(ms []member, model string) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	b.WriteString(`"model":`)
	b.WriteString(jsonString(model))
	for _, m := range ms {
		if m.key == "model" {
			continue
		}
		b.WriteByte(',')
		b.WriteString(jsonString(m.key))
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}
