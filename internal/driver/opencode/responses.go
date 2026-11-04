package opencode

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The Responses API of the gateway (POST <base>/openai/v1/responses and
// <base>/v1/responses), as recorded against the real OpenCode Go on
// 2026-10-31 (task 240; testdata/real/opencode/responses/). The models of
// the Go plan have no native Responses upstream, so the gateway routes the
// request "portably": it translates it to the model's own API (an
// upstream error names AnthropicRequest.max_tokens), refuses what needs a
// native target (state, unknown fields) and renders the answer in its own
// object and event stream. The request is parsed into a Chat Completions
// request (the engine counts, caches and keeps no record as for Chat) and
// only the writers know it came through /responses (isResponses).

// isResponses reports whether req came through a Responses route.
func isResponses(req *core.Request) bool {
	return req != nil && strings.HasSuffix(req.Header("x-wwz-path"), "/responses")
}

// extraRawReply carries an answer of the route's own checks.
const extraRawReply = "opencode_raw_reply"

// rawReply is a check's answer: status, body ("" with no content type for
// a body that is not JSON) and the model for x-zen-model ("" for none).
type rawReply struct {
	status int
	body   string
	model  string
}

func rawError(msg string, r rawReply) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg, Extra: map[string]any{extraRawReply: r}}
}

// jsonString is s as a JSON string literal (< > & as they are).
func jsonString(s string) string {
	b, _ := jsonFormat.Marshal(s)
	return string(b)
}

// writeRawReply sends a check's answer with the log id.
func writeRawReply(w core.ResponseWriter, r rawReply) {
	w.Status(r.status)
	if r.body != "" {
		w.Header("Content-Type", "application/json")
	}
	w.Header("Content-Length", strconv.Itoa(len(r.body)))
	w.Header("x-opencode-log-id", newLogID(time.Now()))
	if r.model != "" {
		w.Header("x-zen-model", r.model)
	}
	_, _ = w.Write([]byte(r.body))
	_ = w.Flush()
}

// portableFields are the fields the portable route accepts (field_*
// fixtures: each sent alone with a valid request, answered 200).
var portableFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "max_output_tokens": true, "temperature": true, "top_p": true,
	"metadata": true, "user": true, "store": true, "previous_response_id": true, "reasoning": true, "text": true,
	"include": true, "truncation": true, "service_tier": true, "prompt_cache_key": true, "parallel_tool_calls": true,
	"background": true, "safety_identifier": true, "tool_choice": true, "tools": true, "stream": true,
}

// nativeFields are fields of OpenAI's Responses API the portable route
// refuses even before it looks at input (chat_messages_field,
// field_max_tool_calls, field_top_logprobs, field_conversation,
// field_prompt_cache_retention). A field the API does not know (zzz) is
// refused the same way only when input is there; without input the
// request has no endpoint (order_unknown_before_missing_input).
var nativeFields = map[string]bool{
	"messages": true, "max_tool_calls": true, "top_logprobs": true, "conversation": true, "prompt_cache_retention": true,
}

// Texts of the portable route (conferred).
const (
	nativeSuffix    = ": requires a native Responses target; portable routing requires the full conversation in input"
	storeNative     = "store: stored responses require a native Responses target; use store=false and send prior output items in input"
	inputRequired   = "input: at least one message is required"
	endpointMissing = "Endpoint is unavailable."
)

// gatewayInvalid is the gateway's own 400 ({type: "error", error{type,
// message}}).
func gatewayInvalid(msg string) *core.Error {
	b, _ := jsonFormat.Marshal(oai.O("type", "error", "error", oai.O("type", "invalid_request_error", "message", msg)))
	return rawError(msg, rawReply{status: 400, body: string(b)})
}

// upstreamFailure is an upstream error that carries code instead of type
// (input_empty_string, type_max_output_tokens), with x-zen-model.
func upstreamFailure(msg, model string) *core.Error {
	b, _ := jsonFormat.Marshal(oai.O("error", oai.O("code", "server_error", "message", upstreamPrefix+msg)))
	return rawError(msg, rawReply{status: 400, body: string(b), model: model})
}

// unavailable is the 503 of a request no endpoint can take (type_input,
// input_empty_array, field_stream_options,
// order_unknown_before_missing_input).
func unavailable() *core.Error {
	b, _ := jsonFormat.Marshal(oai.O("error", oai.O("type", "server_error", "message", upstreamPrefix+endpointMissing)))
	return rawError(endpointMissing, rawReply{status: 503, body: string(b)})
}

// goTypeName is the Go type name of a JSON value in encoding/json's
// "cannot unmarshal <type>" (the upstream is written in Go).
func goTypeName(v []byte) string {
	switch v[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case '[':
		return "array"
	case '{':
		return "object"
	}
	return "number"
}

// checkResponses runs the route's checks in the order observed:
//  1. a body that is not JSON: 400 with no body (invalid_json);
//  2. model not a non-empty string: the Chat route's envelope
//     (missing_model, type_model, empty_object);
//  3. input missing: a field of nativeFields (chat_messages_field), else
//     503 when the body has a field the API does not know
//     (order_unknown_before_missing_input), else 400 "input: at least one
//     message is required" (missing_input); input not a string or a list,
//     or an empty list, and stream_options: 503;
//  4. in body order, the first field the portable route does not take
//     (unknown_param, field_max_tool_calls...), store: true and a
//     previous_response_id: 400 "requires a native Responses target"
//     (order_store_before_unknown);
//  5. the upstream's answers: input "" (input_empty_string, with the
//     upstream's trace id) and max_output_tokens not a number
//     (type_max_output_tokens; the DeepSeek upstream's text).
//
// The model itself is checked before steps 3 to 5, as the real does
// (order_unknown_model_vs_unknown_field), by the server's validator, which
// knows the configured models (validateOpenCode, task 267).
func checkResponses(body []byte) *core.Error {
	if !json.Valid(body) {
		return rawError("invalid JSON", rawReply{status: 400})
	}
	var ms map[string]json.RawMessage
	_ = json.Unmarshal(body, &ms)
	var model string
	if json.Unmarshal(ms["model"], &model) != nil || model == "" {
		const msg = "`model` must be a non-empty string"
		b := `{"error":{"message":` + jsonString(msg) + `,"type":"invalid_request_error","param":"model","code":null}}`
		return rawError(msg, rawReply{status: 400, body: b})
	}
	order := bodyOrder(body)
	// native is the first field, in body order, that needs a native
	// Responses target; unknown says whether a field the API does not
	// know counts.
	native := func(unknown bool) *core.Error {
		for _, k := range order {
			v := bytes.TrimSpace(ms[k])
			switch {
			case nativeFields[k] || (unknown && !portableFields[k] && k != "stream_options"):
				return gatewayInvalid(k + nativeSuffix)
			case k == "store" && string(v) == "true":
				return gatewayInvalid(storeNative)
			case k == "previous_response_id" && string(v) != "null":
				return gatewayInvalid(k + nativeSuffix)
			}
		}
		return nil
	}
	input := bytes.TrimSpace(ms["input"])
	switch {
	case len(input) == 0 || string(input) == "null":
		if err := native(false); err != nil {
			return err // chat_messages_field
		}
		for _, k := range order {
			if !portableFields[k] {
				return unavailable()
			}
		}
		return gatewayInvalid(inputRequired)
	case input[0] != '"' && input[0] != '[', string(input) == "[]":
		return unavailable()
	}
	if _, ok := ms["stream_options"]; ok {
		return unavailable()
	}
	if err := native(true); err != nil {
		return err
	}
	if string(input) == `""` {
		return upstreamFailure("[BadRequest] Invalid request: messages must not be empty, trace_id: "+traceID(), model)
	}
	if v := bytes.TrimSpace(ms["max_output_tokens"]); len(v) > 0 && string(v) != "null" && goTypeName(v) != "number" {
		return upstreamFailure("[400] body unmarshal json: cannot unmarshal "+goTypeName(v)+" into Go struct field AnthropicRequest.max_tokens of type int", model)
	}
	return nil
}

// bodyOrder is the keys of a JSON object in the order of the body.
func bodyOrder(body []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return keys
		}
		k, _ := t.(string)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
		keys = append(keys, k)
	}
	return keys
}

// traceID is the upstream's trace id: 32 hex.
func traceID() string {
	return strings.ReplaceAll(newLogID(time.Now()), "-", "")
}

// parseResponses checks the body and parses it into a Chat request.
func (d *Driver) parseResponses(body []byte) (*core.Request, *core.Error) {
	if err := checkResponses(body); err != nil {
		return nil, err
	}
	req, err := oai.ParseResponses(body, d.Name())
	if err != nil {
		return nil, err
	}
	req.Endpoint = core.EndpointChat // the portable translation (see above)
	reasonByDefault(req, body, "reasoning")
	return req, nil
}
