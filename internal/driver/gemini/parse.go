package gemini

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// ParseRequest reads models/{model}:{method}. The order is the real one
// (observed, task 164): an unknown method is a 404 before anything; the
// JSON transcoder's errors win over the key (driver.ExtraBeforeAuth); the
// semantic checks come after the key, and the ones the model's backend
// makes (a request ending with a model turn, cachedContent) carry
// x-gemini-service-tier.
func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	if kind != driver.KindGenerate {
		return nil, noRoute()
	}
	r := routeOfHeaders(h)
	if r.model == "" || (r.method != methodGenerate && r.method != methodStream && r.method != methodCount) {
		return nil, noRoute()
	}
	schema := msgGenerate
	if r.method == methodCount {
		schema = msgCount
	}
	obj, serr, vs := transcode(body, schema)
	beforeAuth := credentialOf(h).Scheme != schemeBearer // the OAuth check comes first (observed)
	if serr != nil {
		e := invalid(serr.msg)
		e.Extra = map[string]any{driver.ExtraBeforeAuth: beforeAuth}
		return nil, e
	}
	if len(vs) > 0 {
		e := badRequest(vs)
		e.Extra[driver.ExtraBeforeAuth] = beforeAuth
		return nil, e
	}
	gen := obj
	if r.method == methodCount {
		if g, ok := obj["generateContentRequest"].(map[string]any); ok {
			gen = g
		} else if _, ok := obj["contents"]; !ok {
			return nil, invalid("CountTokens requires generate_content_request or contents to be set.")
		}
	}
	if e := checkGenerate(gen, r.method); e != nil {
		return nil, afterModel(e, r.model)
	}
	req := &core.Request{
		Driver: d.Name(), Endpoint: endpointGenerate, Model: r.model, Raw: body,
		Stream: r.method == methodStream, IncludeUsage: true, Received: time.Now(), N: 1,
	}
	if r.method == methodCount {
		req.Endpoint = core.EndpointCountTokens
		req.MaxTokens = 1
	}
	if mh, ok := h.(driver.MapHeaders); ok {
		req.Headers = mh // the engine stores the same map; the cache owner needs it now
	}
	if e := fill(req, gen); e != nil {
		return nil, afterModel(e, r.model)
	}
	return req, nil
}

// afterModel marks the backend's errors with the model they follow: the
// real resolves the model first, so an unknown one is its 404 instead
// (fixture candidates_model_unknown, task 235).
func afterModel(e *core.Error, model string) *core.Error {
	if e.Extra[extraBackend] == true {
		e.Extra[driver.ExtraAfterModel] = model
	}
	return e
}

// checkGenerate makes the front end's semantic checks (observed texts):
// contents present, candidateCount in [1, 8] and temperature in [0, 2],
// one line each, under CountTokensRequest.generate_content_request on
// countTokens; a stream refuses more than one candidate. Then the
// backend's: roles, parts, more than one candidate (no model takes it,
// observed on every listed model on 2026-10-31), the last turn and
// cachedContent without systemInstruction, tools or toolConfig.
func checkGenerate(g map[string]any, method string) *core.Error {
	count := method == methodCount
	contents, _ := g["contents"].([]any)
	prefix := "GenerateContentRequest."
	if count {
		prefix = "CountTokensRequest.generate_content_request."
	}
	var lines []string
	if len(contents) == 0 && !count {
		lines = append(lines, "* GenerateContentRequest.contents: contents is not specified\n")
	}
	gc, _ := g["generationConfig"].(map[string]any)
	candidates, ok := gc["candidateCount"].(int64)
	if ok && (candidates < 1 || candidates > 8) {
		lines = append(lines, "* "+prefix+"generation_config.candidate_count: candidate_count must be in the range [1, 8].\n")
	}
	if t, ok := gc["temperature"].(float64); ok && (t < 0 || t > 2) {
		lines = append(lines, "* "+prefix+"generation_config.temperature: temperature must be in the range [0.0, 2.0].\n")
	}
	if len(lines) > 0 {
		return invalid(strings.Join(lines, ""))
	}
	if method == methodStream && candidates > 1 {
		return invalid("Only one candidate can be specified in the current model")
	}
	// The model's backend (x-gemini-service-tier, observed): roles user or
	// model ("assistant" is taken as model), parts not empty, the last turn
	// not the model's.
	last := ""
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		role, _ := cm["role"].(string)
		switch strings.ToLower(role) {
		case "", "user":
			last = "user"
		case "model", "assistant":
			last = "model"
		default:
			return backend(invalid("Role '" + role + "' is not supported. Please use a valid role: MODEL, USER."))
		}
		if parts, _ := cm["parts"].([]any); len(parts) == 0 {
			return backend(invalid("Request has empty input."))
		}
	}
	if count {
		return nil
	}
	if candidates > 1 {
		return backend(invalid("Multiple candidates is not enabled for this model"))
	}
	if last == "model" {
		return backend(invalid("Requests ending with a model turn are not supported."))
	}
	if name, _ := g["cachedContent"].(string); name != "" {
		_, sys := g["systemInstruction"]
		_, tools := g["tools"]
		_, tc := g["toolConfig"]
		if sys || tools || tc {
			return backend(invalid("CachedContent can not be used with GenerateContent request setting system_instruction, tools or tool_config.\n\nProposed fix: move those values to CachedContent from GenerateContent request."))
		}
	}
	return nil
}

// fill converts the normalized GenerateContentRequest into the engine's
// request.
func fill(req *core.Request, g map[string]any) *core.Error {
	if name, _ := g["cachedContent"].(string); name != "" {
		cc := getCache(cacheOwner(req), name, time.Now())
		if cc == nil {
			return backend(&core.Error{Kind: core.ErrPermission, Message: msgCacheMissing})
		}
		req.System = cc.system
		req.Messages = append(req.Messages, cc.messages...)
		req.Tools = append(req.Tools, cc.tools...)
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		req.Metadata[metaCached] = cc.name
		req.Metadata[metaCachedTokens] = strconv.Itoa(cc.tokens)
	}
	if si, ok := g["systemInstruction"].(map[string]any); ok {
		req.System = contentText(si)
	}
	for _, c := range asList(g["contents"]) {
		req.Messages = append(req.Messages, toMessage(c))
	}
	for _, t := range asList(g["tools"]) {
		for _, fd := range asList(t["functionDeclarations"]) {
			td := core.ToolDef{Name: str(fd, "name"), Description: str(fd, "description")}
			if p, ok := fd["parameters"].(json.RawMessage); ok {
				td.Parameters = p
			} else if p, ok := fd["parametersJsonSchema"].(json.RawMessage); ok {
				td.Parameters = p
			}
			req.Tools = append(req.Tools, td)
		}
	}
	toolChoice(req, g)
	gc, _ := g["generationConfig"].(map[string]any)
	if n, ok := gc["maxOutputTokens"].(int64); ok && n > 0 && req.Endpoint != core.EndpointCountTokens {
		req.MaxTokens = int(n)
	}
	if t, ok := gc["temperature"].(float64); ok {
		req.Temperature = &t
	}
	if s, ok := gc["seed"].(int64); ok {
		req.Seed = &s
	}
	switch mt, _ := gc["responseMimeType"].(string); {
	case mt != "application/json":
	case gc["responseSchema"] != nil:
		req.ResponseFormat, req.ResponseSchema = "json_schema", gc["responseSchema"].(json.RawMessage)
	case gc["responseJsonSchema"] != nil:
		req.ResponseFormat, req.ResponseSchema = "json_schema", gc["responseJsonSchema"].(json.RawMessage)
	default:
		req.ResponseFormat = "json_object"
	}
	thinking(req, gc)
	return nil
}

// thinking reads generationConfig.thinkingConfig (spec 007): a positive or
// dynamic (-1) thinkingBudget or a thinkingLevel above MINIMAL asks for
// reasoning; the text only goes out with includeThoughts, otherwise it is
// counted in thoughtsTokenCount and not returned.
func thinking(req *core.Request, gc map[string]any) {
	tc, ok := gc["thinkingConfig"].(map[string]any)
	if !ok {
		req.Reasoning.Exclude = true
		return
	}
	include, _ := tc["includeThoughts"].(bool)
	req.Reasoning.Exclude = !include
	if b, ok := tc["thinkingBudget"].(int64); ok {
		switch {
		case b > 0:
			req.Reasoning.Requested, req.Reasoning.BudgetTokens = true, int(b)
		case b < 0:
			req.Reasoning.Requested = true
		}
	}
	switch lvl, _ := tc["thinkingLevel"].(string); strings.ToUpper(lvl) {
	case "LOW":
		req.Reasoning.Requested, req.Reasoning.Effort = true, "low"
	case "MEDIUM":
		req.Reasoning.Requested, req.Reasoning.Effort = true, "medium"
	case "HIGH":
		req.Reasoning.Requested, req.Reasoning.Effort = true, "high"
	}
}

// toolChoice maps toolConfig.functionCallingConfig (spec 007): NONE never
// calls, ANY calls at least one (the one allowed name, when there is only
// one), AUTO and VALIDATED draw; allowedFunctionNames restricts the tools.
func toolChoice(req *core.Request, g map[string]any) {
	req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
	tc, _ := g["toolConfig"].(map[string]any)
	fc, _ := tc["functionCallingConfig"].(map[string]any)
	var allowed []string
	for _, n := range asAny(fc["allowedFunctionNames"]) {
		if s, ok := n.(string); ok {
			allowed = append(allowed, s)
		}
	}
	if len(allowed) > 0 {
		keep := req.Tools[:0]
		for _, t := range req.Tools {
			for _, a := range allowed {
				if t.Name == a {
					keep = append(keep, t)
					break
				}
			}
		}
		req.Tools = keep
	}
	switch mode, _ := fc["mode"].(string); strings.ToUpper(mode) {
	case "NONE":
		req.ToolChoice.Mode = core.ToolChoiceNone
	case "ANY":
		req.ToolChoice.Mode = core.ToolChoiceRequired
		if len(allowed) == 1 {
			req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNamed, Name: allowed[0]}
		}
	}
}

// toMessage converts one Content.
func toMessage(c map[string]any) core.Message {
	m := core.Message{Role: "user"}
	if r, _ := c["role"].(string); strings.EqualFold(r, "model") || strings.EqualFold(r, "assistant") {
		m.Role = "assistant"
	}
	var text []string
	for _, p := range asList(c["parts"]) {
		if s, ok := p["text"].(string); ok {
			text = append(text, s) // thought parts too: the real counts them (task 265)
		}
		if fc, ok := p["functionCall"].(map[string]any); ok {
			args := "{}"
			if a, ok := fc["args"].(json.RawMessage); ok {
				args = string(a)
			}
			m.ToolCalls = append(m.ToolCalls, core.ToolCall{ID: str(fc, "id"), Name: str(fc, "name"), Arguments: args})
		}
		if fr, ok := p["functionResponse"].(map[string]any); ok {
			content := "{}"
			if r, ok := fr["response"].(json.RawMessage); ok {
				content = string(r)
			}
			id := str(fr, "id")
			if id == "" {
				id = str(fr, "name")
			}
			m.ToolResults = append(m.ToolResults, core.ToolResult{ToolCallID: id, Content: content})
		}
		for _, k := range []string{"inlineData", "fileData"} {
			if b, ok := p[k].(map[string]any); ok {
				if strings.HasPrefix(str(b, "mimeType"), "audio/") {
					m.AudioParts++
				} else {
					m.ImageParts++
				}
			}
		}
	}
	m.Content = strings.Join(text, "")
	return m
}

// contentText is the text of a Content (systemInstruction): the parts
// joined without a separator, as the real counts them (task 265).
func contentText(c map[string]any) string {
	var parts []string
	for _, p := range asList(c["parts"]) {
		if s, ok := p["text"].(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "")
}

func asAny(v any) []any {
	l, _ := v.([]any)
	return l
}

// asList is a repeated message field as objects.
func asList(v any) []map[string]any {
	l := asAny(v)
	out := make([]map[string]any, 0, len(l))
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
