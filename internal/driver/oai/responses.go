package oai

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The Responses API (POST /v1/responses), as conferred against the real
// API on 2026-10-31 (testdata/real/openai/responses/, spec 004 "Responses
// API", ADR 0055). The pipeline is the Chat Completions one: the request is
// normalized to a core.Request (instructions become the first message,
// input items become messages) and the writers below render the response
// object and its event stream. What the request asked for (instructions,
// tools, text.format, store...) is echoed from the raw body.

// responsesFormat is the layout of a Responses body: 2 spaces, no final
// newline (default_store, previous_response_id).
var responsesFormat = Format{Indent: "  ", RawHTML: true}

// responsesRequest is the wire shape the parser reads.
type responsesRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      json.RawMessage   `json:"instructions"`
	MaxOutputTokens   *int              `json:"max_output_tokens"`
	Stream            bool              `json:"stream"`
	Temperature       *float64          `json:"temperature"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Text              *struct {
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
	Reasoning *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Metadata map[string]string `json:"metadata"`
	User     string            `json:"user"`
}

// responsesItem is one element of input when it is a list.
type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesTool is one element of tools (flat function shape).
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ResponsesBadJSON is the Responses API's answer to a body that is not JSON.
const ResponsesBadJSON = "Invalid body: failed to parse JSON value. Please check the value to ensure it is valid JSON. (Common errors include trailing commas, missing closing brackets, missing quotation marks, etc.)"

// ParseResponses converts a Responses body into a core.Request. The checks
// of the real API run before it (server validation); the errors here only
// guard a body the validation did not see.
func ParseResponses(body []byte, driverName string) (*core.Request, *core.Error) {
	var wr responsesRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: ResponsesBadJSON, Code: "invalid_json"}
	}
	if wr.Model == "" {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Missing required parameter: 'model'.", Param: "model", Code: "missing_required_parameter"}
	}
	in := bytes.TrimSpace(wr.Input)
	if len(in) == 0 || string(in) == "null" {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Missing required parameter: 'input'.", Param: "input", Code: "missing_required_parameter"}
	}
	req := &core.Request{
		Driver: driverName, Endpoint: core.EndpointResponses, Model: wr.Model, Stream: wr.Stream,
		IncludeUsage: true, Raw: body, Received: time.Now(), Temperature: wr.Temperature,
		ParallelToolCalls: wr.ParallelToolCalls,
	}
	if len(wr.Metadata) > 0 || wr.User != "" {
		req.Metadata = make(map[string]string, len(wr.Metadata)+1)
		for k, v := range wr.Metadata {
			req.Metadata[k] = v
		}
		if wr.User != "" {
			req.Metadata["user"] = wr.User
		}
	}
	if wr.MaxOutputTokens != nil {
		req.MaxTokens = *wr.MaxOutputTokens
	}
	if wr.Reasoning != nil && wr.Reasoning.Effort != "" && wr.Reasoning.Effort != "none" {
		req.Reasoning = core.ReasoningRequest{Requested: true, Effort: wr.Reasoning.Effort}
	}
	if wr.Text != nil && wr.Text.Format != nil && wr.Text.Format.Type != "text" {
		req.ResponseFormat = wr.Text.Format.Type
		if wr.Text.Format.Type == "json_schema" {
			req.ResponseSchema = wr.Text.Format.Schema
		}
	}
	// instructions is the first message (developer, counted like system);
	// req.System stays empty, as on Chat Completions (task 245).
	var instr string
	if json.Unmarshal(wr.Instructions, &instr) == nil && instr != "" {
		req.Messages = append(req.Messages, core.Message{Role: "system", Content: instr})
	}
	msgs, perr := responsesInput(in)
	if perr != nil {
		return nil, perr
	}
	req.Messages = append(req.Messages, msgs...)
	for i, raw := range wr.Tools {
		var t responsesTool
		if json.Unmarshal(raw, &t) != nil {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Invalid type for 'tools[" + strconv.Itoa(i) + "]': expected an object.", Param: "tools[" + strconv.Itoa(i) + "]", Code: "invalid_type"}
		}
		if t.Type == "function" && t.Name != "" {
			// Native tools (web_search, file_search...) are echoed, never called.
			req.Tools = append(req.Tools, core.ToolDef{Name: t.Name, Description: t.Description, Parameters: strictParameters(raw, t.Parameters)})
		}
	}
	req.ToolChoice = parseToolChoice(wr.ToolChoice)
	if req.ToolChoice.Mode == core.ToolChoiceNamed && !hasTool(req.Tools, req.ToolChoice.Name) {
		req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto} // not conferred
	}
	if req.ToolChoice.Mode == "" {
		req.ToolChoice.Mode = core.ToolChoiceAuto
	}
	return req, nil
}

// strictParameters is the schema the arguments are generated from: the
// strict-normalized one when the function is strict (every property
// required, as the real model then always fills them), else the one sent.
func strictParameters(raw, params json.RawMessage) json.RawMessage {
	o, ok := echoTool(raw).(Obj)
	if !ok {
		return params
	}
	if strict, _ := o.Get("strict"); strict != true {
		return params
	}
	norm, _ := o.Get("parameters")
	p, ok := norm.(Obj)
	if !ok {
		return params
	}
	b, _ := p.MarshalJSON() // decoded JSON values always encode
	return b
}

func hasTool(tools []core.ToolDef, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// responsesInput turns input (a string or a list of items) into messages.
// Consecutive function_call items form one assistant turn, as in Chat.
func responsesInput(in json.RawMessage) ([]core.Message, *core.Error) {
	if in[0] == '"' {
		var s string
		_ = json.Unmarshal(in, &s)
		return []core.Message{{Role: "user", Content: s}}, nil
	}
	var items []responsesItem
	if json.Unmarshal(in, &items) != nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Invalid type for 'input': expected one of a string or array of input items, but got " + jsonKind(in) + " instead.", Param: "input", Code: "invalid_type"}
	}
	var out []core.Message
	for _, it := range items {
		switch it.Type {
		case "", "message":
			role := it.Role
			if role == "developer" {
				role = "system"
			}
			text, images, audio := respContent(it.Content)
			out = append(out, core.Message{Role: role, Content: text, ImageParts: images, AudioParts: audio})
		case "function_call":
			tc := core.ToolCall{ID: it.CallID, Name: it.Name, Arguments: it.Arguments}
			if n := len(out); n > 0 && out[n-1].Role == "assistant" && len(out[n-1].ToolCalls) > 0 {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, tc)
				continue
			}
			out = append(out, core.Message{Role: "assistant", ToolCalls: []core.ToolCall{tc}})
		case "function_call_output":
			text, _, _ := respContent(it.Output)
			out = append(out, core.Message{Role: "tool", ToolCallID: it.CallID, Content: text})
		}
		// reasoning and item_reference add nothing the simulator counts.
	}
	return out, nil
}

// respContent flattens the content of an input message: a string, or parts
// (input_text and output_text give text; input_image, input_file and
// input_audio are counted, never read). Unlike Chat, input_image carries
// image_url as a string.
func respContent(raw json.RawMessage) (text string, images, audio int) {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '[' || json.Unmarshal(b, &parts) != nil {
		return flattenContent(raw)
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		case "input_audio":
			audio++
		default: // input_image, input_file
			images++
		}
	}
	return sb.String(), images, audio
}

// jsonKind names the JSON type of raw the way the API's messages do.
func jsonKind(raw json.RawMessage) string {
	switch raw[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	case '"':
		return "a string"
	}
	return "a number"
}

// --- ids ---

// ResponsesID is a response id: "resp_" + 50 hex. The real ids share their
// first 16 hex with the ids of their items, then carry "0" + a digit and the
// creation time in hex (observed); the rest is random.
func ResponsesID(rng *generate.RNG, now time.Time) string {
	b := make([]byte, 0, 55)
	b = append(b, "resp_"...)
	b = appendRandHex(b, rng, 16)
	b = append(b, '0', '1')
	b = appendTimeHex(b, now)
	return string(appendRandHex(b, rng, 24))
}

func appendRandHex(b []byte, rng *generate.RNG, n int) []byte {
	for range n {
		b = append(b, hexDigits[rng.IntN(16)])
	}
	return b
}

func appendTimeHex(b []byte, t time.Time) []byte {
	s := strconv.FormatInt(t.Unix()&0xffffffff, 16)
	for range 8 - len(s) {
		b = append(b, '0')
	}
	return append(b, s...)
}

// itemID is the id of output item i of response respID ("msg", "fc", "rs"):
// the response's first 16 hex, then a stable derivation of the response id.
func itemID(prefix, respID string, created time.Time, i int) string {
	shared := strings.TrimPrefix(respID, "resp_")
	if len(shared) > 16 {
		shared = shared[:16]
	}
	sum := sha256.Sum256([]byte(respID + "\x00" + prefix + "\x00" + strconv.Itoa(i)))
	b := make([]byte, 0, len(prefix)+51)
	b = append(b, prefix...)
	b = append(b, '_')
	b = append(b, shared...)
	b = append(b, '0', '1')
	b = appendTimeHex(b, created)
	return string(append(b, hex.EncodeToString(sum[:12])...))
}

// --- the response object ---

// respEcho is what the response object repeats from the request.
type respEcho struct {
	Instructions      json.RawMessage   `json:"instructions"`
	MaxOutputTokens   json.RawMessage   `json:"max_output_tokens"`
	MaxToolCalls      json.RawMessage   `json:"max_tool_calls"`
	Temperature       json.RawMessage   `json:"temperature"`
	TopP              json.RawMessage   `json:"top_p"`
	TopLogprobs       json.RawMessage   `json:"top_logprobs"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Text              *struct {
		Format    json.RawMessage `json:"format"`
		Verbosity *string         `json:"verbosity"`
	} `json:"text"`
	Reasoning *struct {
		Effort  *string `json:"effort"`
		Summary *string `json:"summary"`
	} `json:"reasoning"`
	Store                *bool           `json:"store"`
	PreviousResponseID   *string         `json:"previous_response_id"`
	Include              []string        `json:"include"`
	Metadata             json.RawMessage `json:"metadata"`
	PromptCacheKey       *string         `json:"prompt_cache_key"`
	PromptCacheRetention *string         `json:"prompt_cache_retention"`
	SafetyIdentifier     *string         `json:"safety_identifier"`
	ServiceTier          *string         `json:"service_tier"`
	Truncation           *string         `json:"truncation"`
	User                 *string         `json:"user"`
	Background           *bool           `json:"background"`
}

// respView renders one response: the echo of the request, computed once.
type respView struct {
	id      string
	created time.Time
	model   string
	echo    respEcho
	tools   []any
	summary bool // reasoning.summary asked: the reasoning item carries text
}

func newRespView(req *core.Request, resp *core.Response) *respView {
	v := &respView{id: resp.ID, created: resp.Created, model: resp.Model}
	if req != nil && len(req.Raw) > 0 {
		_ = json.Unmarshal(req.Raw, &v.echo)
	}
	v.tools = make([]any, 0, len(v.echo.Tools))
	for _, raw := range v.echo.Tools {
		v.tools = append(v.tools, echoTool(raw))
	}
	if r := v.echo.Reasoning; r != nil && r.Summary != nil && *r.Summary != "" {
		v.summary = true
	}
	return v
}

// rawOr is the ordered JSON of raw, or def when raw is absent.
func rawOr(raw json.RawMessage, def any) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return def
	}
	return decodeOrdered(raw)
}

// floatOr renders a number the way the API does: always with a fraction
// (1.0, 0.7), def when absent.
func floatOr(raw json.RawMessage, def float64) any {
	raw = bytes.TrimSpace(raw)
	f := def
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &f); err != nil {
			return decodeOrdered(raw)
		}
	}
	return pyFloat(f)
}

// pyFloat is f as the real API prints floats: Python's repr (1.0, 0.7).
func pyFloat(f float64) json.RawMessage {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return json.RawMessage(s)
}

func strOr(s *string, def any) any {
	if s == nil {
		return def
	}
	return *s
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// requestedTier is service_tier as asked ("auto" when absent): the value of
// response.created and response.in_progress.
func (v *respView) requestedTier() string {
	if t := v.echo.ServiceTier; t != nil && *t != "" {
		return *t
	}
	return "auto"
}

// finalTier is service_tier of the finished response: "default" for auto.
func (v *respView) finalTier() string {
	if t := v.requestedTier(); t != "auto" {
		return t
	}
	return "default"
}

// object is the response object in the real field order. billing is only on
// non-streaming bodies.
func (v *respView) object(status string, output []any, usage, completedAt, incomplete any, billing bool, tier string) Obj {
	e := &v.echo
	o := make(Obj, 0, 42)
	o = append(o,
		Field{"id", v.id}, Field{"object", "response"}, Field{"created_at", v.created.Unix()}, Field{"status", status},
		Field{"access_programs", nil}, Field{"background", boolOr(e.Background, false)},
	)
	if billing {
		o = append(o, Field{"billing", O("payer", "openai")})
	}
	reasoning := O("context", nil, "effort", nil, "summary", nil)
	if r := e.Reasoning; r != nil {
		reasoning = O("context", nil, "effort", strOr(r.Effort, nil), "summary", strOr(r.Summary, nil))
	}
	var format any = O("type", "text")
	verbosity := "medium"
	if t := e.Text; t != nil {
		if f := bytes.TrimSpace(t.Format); len(f) > 0 && string(f) != "null" {
			format = echoFormat(f)
		}
		if t.Verbosity != nil {
			verbosity = *t.Verbosity
		}
	}
	return append(o,
		Field{"completed_at", completedAt}, Field{"error", nil}, Field{"frequency_penalty", pyFloat(0)},
		Field{"incomplete_details", incomplete}, Field{"instructions", rawOr(e.Instructions, nil)},
		Field{"max_output_tokens", rawOr(e.MaxOutputTokens, nil)}, Field{"max_tool_calls", rawOr(e.MaxToolCalls, nil)},
		Field{"model", v.model}, Field{"moderation", nil}, Field{"output", output},
		Field{"parallel_tool_calls", boolOr(e.ParallelToolCalls, true)}, Field{"presence_penalty", pyFloat(0)},
		Field{"previous_response_id", strOr(e.PreviousResponseID, nil)}, Field{"prompt_cache_key", strOr(e.PromptCacheKey, nil)},
		Field{"prompt_cache_retention", strOr(e.PromptCacheRetention, "in_memory")}, Field{"reasoning", reasoning},
		Field{"safety_identifier", strOr(e.SafetyIdentifier, nil)}, Field{"service_tier", tier},
		Field{"store", boolOr(e.Store, true)}, Field{"temperature", floatOr(e.Temperature, 1)},
		Field{"text", O("format", format, "verbosity", verbosity)}, Field{"tool_choice", rawOr(e.ToolChoice, "auto")},
		Field{"tool_usage", toolUsage()}, Field{"tools", v.tools}, Field{"top_logprobs", rawOr(e.TopLogprobs, 0)},
		Field{"top_p", floatOr(e.TopP, 1)}, Field{"truncation", strOr(e.Truncation, "disabled")}, Field{"usage", usage},
		Field{"user", strOr(e.User, nil)}, Field{"metadata", rawOr(e.Metadata, Obj{})},
	)
}

// toolUsage is the tool_usage object of a response that used no tool.
func toolUsage() Obj {
	tokens := func() Obj { return O("image_tokens", 0, "text_tokens", 0) }
	return O(
		"image_gen", O("input_tokens", 0, "input_tokens_details", tokens(), "output_tokens", 0, "output_tokens_details", tokens(), "total_tokens", 0),
		"web_search", O("num_requests", 0),
	)
}

// ResponsesUsage is the usage object: cache_write_tokens before
// cached_tokens (cache_automatic_prefix).
func ResponsesUsage(u core.Usage) Obj {
	return O(
		"input_tokens", u.PromptTokens,
		"input_tokens_details", O("cache_write_tokens", u.CacheWriteTokens, "cached_tokens", u.CachedTokens),
		"output_tokens", u.CompletionTokens,
		"output_tokens_details", O("reasoning_tokens", u.ReasoningTokens),
		"total_tokens", u.TotalTokens(),
	)
}

// respStatus maps the finish reason to status and incomplete_details.
func respStatus(fr core.FinishReason) (string, any) {
	switch fr {
	case core.FinishLength:
		return "incomplete", O("reason", "max_output_tokens")
	case core.FinishContentFilter:
		return "incomplete", O("reason", "content_filter") // not conferred
	}
	return "completed", nil
}

func textPart(text string) Obj {
	return O("type", "output_text", "annotations", []any{}, "logprobs", []any{}, "text", text)
}

// outputPart is the message content of the finished response: output_text,
// or refusal for a refusal (not conferred).
func outputPart(text string, fr core.FinishReason) Obj {
	if fr == core.FinishRefusal {
		return O("type", "refusal", "refusal", text)
	}
	return textPart(text)
}

func messageItem(id, status string, content []any) Obj {
	return O("id", id, "type", "message", "status", status, "content", content, "role", "assistant")
}

func callItem(id, status, args string, tc core.ToolCall) Obj {
	return O("id", id, "type", "function_call", "status", status, "arguments", args, "call_id", tc.ID, "name", tc.Name)
}

// reasoningItem is the reasoning item (not conferred): summary text only
// when reasoning.summary was asked, encrypted_content only with include.
func (v *respView) reasoningItem(id, text string) Obj {
	o := O("id", id, "type", "reasoning")
	if v.includes("reasoning.encrypted_content") {
		o = append(o, Field{"encrypted_content", encryptedContent(v.id, text)})
	}
	summary := []any{}
	if v.summary {
		summary = []any{O("type", "summary_text", "text", text)}
	}
	return append(o, Field{"summary", summary})
}

func (v *respView) includes(s string) bool {
	for _, x := range v.echo.Include {
		if x == s {
			return true
		}
	}
	return false
}

// encryptedContent is an opaque blob standing for the encrypted reasoning.
func encryptedContent(id, text string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + text))
	return "gAAAAA" + hex.EncodeToString(sum[:])
}

// items are the output items of a finished response, in order: reasoning,
// message, function calls.
func (v *respView) items(resp *core.Response, itemStatus string) []any {
	out := make([]any, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		out = append(out, v.reasoningItem(itemID("rs", v.id, v.created, len(out)), resp.Reasoning))
	}
	if resp.Content != "" || len(resp.ToolCalls) == 0 {
		out = append(out, messageItem(itemID("msg", v.id, v.created, len(out)), itemStatus, []any{outputPart(resp.Content, resp.FinishReason)}))
	}
	for _, tc := range resp.ToolCalls {
		out = append(out, callItem(itemID("fc", v.id, v.created, len(out)), "completed", tc.Arguments, tc))
	}
	return out
}

// completedAt is completed_at: now for a completed response, null otherwise.
func completedAt(status string) any {
	if status != "completed" {
		return nil
	}
	return time.Now().Unix()
}

// WriteResponses writes a non-streaming response object.
func WriteResponses(h *Hooks, w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	v := newRespView(req, resp)
	status, incomplete := respStatus(resp.FinishReason)
	body := h.shape(PartResponse, v.object(status, v.items(resp, status), ResponsesUsage(resp.Usage), completedAt(status), incomplete, true, v.finalTier()))
	b, _ := responsesFormat.Marshal(body) // decoded JSON values always encode
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	h.successHeaders(w, req, resp, rl)
	for k, val := range resp.Headers {
		w.Header(k, val)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}
