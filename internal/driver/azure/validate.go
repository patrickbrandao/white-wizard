package azure

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/oaicheck"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/structured"
)

var _ driver.RequestValidator = (*Driver)(nil)

// ValidateRequest validates the Responses API of the v1 family
// (/openai/v1/responses), which follows OpenAI's contract with model =
// deployment (spec 004 "Responses API", spec 006 16.6): the same checks in
// the same order, with the texts the service passes through from the model
// backend. Two differences: an unknown or retired deployment is Azure's own
// 404 DeploymentNotFound (the driver writes it), and an api-version the v1
// family does not accept is left to the driver, whose 404 Resource not
// found comes before the body. The Chat Completions and legacy completions
// routes are validated by the driver's parse ("Unrecognized request
// argument", below). Nothing here is conferred (no real key, ADR 0043).
func (d *Driver) ValidateRequest(kind driver.Kind, body []byte, h driver.Headers, models driver.Models) *core.Error {
	if kind != driver.KindResponses {
		return nil
	}
	switch driver.Query(h).Get("api-version") {
	case "", "v1", "preview":
	default:
		return nil
	}
	fs, name, err := oaicheck.ResponsesBody(body)
	if err != nil {
		return err
	}
	if !models.KnownModel(driver.KindResponses, name) {
		return &core.Error{Kind: core.ErrModelNotFound}
	}
	return oaicheck.ResponsesTail(fs)
}

// Request validation of the Chat Completions and legacy completions routes
// (spec 004, "Validação de requisição"): a top-level field outside the API
// is refused with "Unrecognized request argument supplied", and a strict
// function or json_schema must follow the Structured Outputs rules, as on
// OpenAI. Texts from the Microsoft Q&A report and ADR 0029; not conferred.

// chatFields are the top-level fields of Chat Completions on Azure: the
// OpenAI ones plus data_sources (On Your Data) and user_security_context.
var chatFields = set(
	"model", "messages", "functions", "tools", "function_call", "tool_choice", "parallel_tool_calls",
	"max_tokens", "max_completion_tokens", "temperature", "top_p", "logit_bias", "user", "safety_identifier",
	"prompt_cache_key", "prompt_cache_retention", "n", "stop", "stream", "presence_penalty", "frequency_penalty",
	"seed", "modalities", "response_format", "audio", "logprobs", "top_logprobs", "stream_options",
	"service_tier", "store", "metadata", "prediction", "reasoning_effort", "verbosity",
	"data_sources", "user_security_context",
)

// completionsFields are the top-level fields of the legacy completions.
var completionsFields = set(
	"model", "prompt", "best_of", "echo", "frequency_penalty", "logit_bias", "logprobs",
	"max_tokens", "n", "presence_penalty", "seed", "stop", "stream", "stream_options",
	"suffix", "temperature", "top_p", "user",
)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func invalid(msg, param, code string) *core.Error {
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg, Param: param, Code: code}
}

// validate checks body for endpoint ep; body is a JSON object.
func validate(ep core.Endpoint, body []byte) *core.Error {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil // the parser answers the malformed body
	}
	allowed := chatFields
	if ep == core.EndpointCompletions {
		allowed = completionsFields
	}
	var unknown []string
	for k := range top {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	switch len(unknown) {
	case 0:
	case 1:
		return invalid("Unrecognized request argument supplied: "+unknown[0], "", "")
	default:
		sort.Strings(unknown)
		return invalid("Unrecognized request arguments supplied: "+strings.Join(unknown, ", "), "", "")
	}
	if ep != core.EndpointChat {
		return nil
	}
	if err := strictTools(top["tools"]); err != nil {
		return err
	}
	return strictResponseFormat(top["response_format"])
}

// strictTools checks every function with strict: true.
func strictTools(raw json.RawMessage) *core.Error {
	var tools []struct {
		Function struct {
			Name       string          `json:"name"`
			Strict     bool            `json:"strict"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	for i, t := range tools {
		if !t.Function.Strict {
			continue
		}
		if msg := structured.Violation(t.Function.Parameters); msg != "" {
			return invalid("Invalid schema for function '"+t.Function.Name+"': "+msg,
				"tools["+strconv.Itoa(i)+"].function.parameters", "invalid_function_parameters")
		}
	}
	return nil
}

// strictResponseFormat checks a strict json_schema response format.
func strictResponseFormat(raw json.RawMessage) *core.Error {
	var rf struct {
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(raw, &rf) != nil || !rf.JSONSchema.Strict {
		return nil
	}
	if msg := structured.Violation(rf.JSONSchema.Schema); msg != "" {
		return invalid("Invalid schema for response_format '"+rf.JSONSchema.Name+"': "+msg, "response_format", "invalid_json_schema")
	}
	return nil
}
