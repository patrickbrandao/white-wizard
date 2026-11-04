package openrouter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// The response object and the event stream of OpenRouter's Responses API
// (text, text_stream, function_call, function_call_stream, incomplete,
// incomplete_stream, instructions; task 240). Not conferred: reasoning
// models (the reasoning item below follows OpenAI's form), keep-alive
// comments on a slow stream, mid-stream errors.

// respEcho is what the object repeats from the request.
type respEcho struct {
	Instructions      json.RawMessage   `json:"instructions"`
	MaxOutputTokens   json.RawMessage   `json:"max_output_tokens"`
	Temperature       json.RawMessage   `json:"temperature"`
	TopP              json.RawMessage   `json:"top_p"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Text              json.RawMessage   `json:"text"`
	Metadata          json.RawMessage   `json:"metadata"`
	Reasoning         json.RawMessage   `json:"reasoning"`
}

// reasoningField is the reasoning member. The real (probed 2026-10-31)
// answers null while in progress and, in the final object, the object with
// null fields for openai/gpt-4.1* (effort first when the request sent
// reasoning); null for gpt-4o*, llama, gemini and deepseek. Not conferred:
// the gpt-5 family (context/effort/mode/summary filled in) and o-series.
func reasoningField(model string, e *respEcho, final bool) any {
	if !final || !strings.HasPrefix(model, "openai/gpt-4.1") {
		return nil
	}
	if len(e.Reasoning) > 0 && string(e.Reasoning) != "null" {
		return oai.O("effort", nil, "context", nil, "summary", nil)
	}
	return oai.O("context", nil, "effort", nil, "summary", nil)
}

// respView renders the object of one response.
type respView struct {
	id      string
	model   string
	created int64
	echo    respEcho
	tools   []any
}

func newRespView(req *core.Request, resp *core.Response) *respView {
	v := &respView{id: resp.ID, model: resp.Model, created: resp.Created.Unix()}
	if req != nil {
		_ = json.Unmarshal(req.Raw, &v.echo)
	}
	v.tools = make([]any, 0, len(v.echo.Tools))
	for _, raw := range v.echo.Tools {
		v.tools = append(v.tools, echoTool(raw))
	}
	return v
}

// echoTool is a tool as the object repeats it: a function in the order
// type, name, description, parameters, strict (null when not sent); any
// other tool as sent (not conferred).
func echoTool(raw json.RawMessage) any {
	var t struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	}
	if json.Unmarshal(raw, &t) != nil || t.Type != "function" {
		return raw
	}
	o := oai.O("type", "function", "name", t.Name)
	if t.Description != nil {
		o = append(o, oai.Field{Key: "description", Val: *t.Description})
	}
	o = append(o, oai.Field{Key: "parameters", Val: rawOrNil(t.Parameters)})
	var strict any
	if t.Strict != nil {
		strict = *t.Strict
	}
	return append(o, oai.Field{Key: "strict", Val: strict})
}

// rawOr is raw (compacted) when present, def otherwise.
func rawOr(raw json.RawMessage, def any) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return def
	}
	return raw
}

func rawOrNil(raw json.RawMessage) any { return rawOr(raw, nil) }

// object is the response object in the gateway's order. completed is
// completed_at (nil while in progress), usage nil while in progress.
func (v *respView) object(status string, output []any, completed, incomplete, usage any, tier string) oai.Obj {
	e := &v.echo
	parallel := true
	if e.ParallelToolCalls != nil {
		parallel = *e.ParallelToolCalls
	}
	return oai.O(
		"id", v.id, "object", "response", "created_at", v.created, "model", v.model, "status", status,
		"completed_at", completed, "output", output, "error", nil, "incomplete_details", incomplete,
		"access_programs", nil, "tools", v.tools, "tool_choice", rawOr(e.ToolChoice, "auto"),
		"parallel_tool_calls", parallel, "max_output_tokens", rawOrNil(e.MaxOutputTokens),
		"temperature", rawOr(e.Temperature, 1), "top_p", rawOr(e.TopP, 1), "presence_penalty", 0,
		"frequency_penalty", 0, "top_logprobs", 0, "max_tool_calls", nil, "metadata", rawOr(e.Metadata, oai.Obj{}),
		"background", false, "previous_response_id", nil, "service_tier", tier, "truncation", "disabled",
		"store", false, "instructions", rawOrNil(e.Instructions), "text", rawOr(e.Text, oai.O("format", oai.O("type", "text"))),
		"reasoning", reasoningField(v.model, e, usage != nil), "safety_identifier", nil, "prompt_cache_key", nil, "prompt_cache_options", nil, "usage", usage,
	)
}

// respUsage is the usage object: OpenAI's counts and OpenRouter's cost.
func respUsage(u core.Usage) oai.Obj {
	cost, prompt, completion := costSplit(u)
	return oai.O(
		"input_tokens", u.PromptTokens, "input_tokens_details", oai.O("cached_tokens", u.CachedTokens),
		"output_tokens", u.CompletionTokens, "output_tokens_details", oai.O("reasoning_tokens", u.ReasoningTokens),
		"total_tokens", u.TotalTokens(), "cost", cost, "is_byok", false,
		"cost_details", oai.O("upstream_inference_cost", cost, "upstream_inference_input_cost", prompt, "upstream_inference_output_cost", completion),
	)
}

// respStatus is status and incomplete_details of a finish reason.
func respStatus(fr core.FinishReason) (string, any) {
	if fr == core.FinishLength {
		return "incomplete", oai.O("reason", "max_output_tokens")
	}
	return "completed", nil
}

// itemID is "<prefix>_tmp_" and 11 base-36 characters, derived from the
// response id and the item's place (msg_tmp_hg7debf5vt, fc_tmp_q4uhoyg9gla:
// the real ones are random, 10 or 11 characters).
func itemID(prefix, respID string, i int) string {
	sum := sha256.Sum256([]byte(respID + "\x00" + prefix + "\x00" + strconv.Itoa(i)))
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 0, len(prefix)+16)
	b = append(b, prefix...)
	b = append(b, "_tmp_"...)
	for _, c := range sum[:11] {
		b = append(b, digits[int(c)%len(digits)])
	}
	return string(b)
}

func outputText(text string) oai.Obj {
	return oai.O("type", "output_text", "text", text, "annotations", []any{}, "logprobs", []any{})
}

// messageItem is a message in the object's output (role before status);
// the stream's items put status first (messageEvent). The status of a
// message cut by max_output_tokens is still "completed" (incomplete).
func messageItem(id, text string) oai.Obj {
	return oai.O("id", id, "type", "message", "role", "assistant", "status", "completed", "content", []any{outputText(text)})
}

func messageEvent(id, status string, content []any) oai.Obj {
	return oai.O("id", id, "type", "message", "status", status, "role", "assistant", "content", content)
}

func callItem(id, status, args string, tc core.ToolCall) oai.Obj {
	return oai.O("id", id, "type", "function_call", "status", status, "call_id", tc.ID, "name", tc.Name, "arguments", args)
}

// reasoningItem: not conferred (OpenAI's form with the summary text).
func reasoningItem(id, text string) oai.Obj {
	summary := []any{}
	if text != "" {
		summary = []any{oai.O("type", "summary_text", "text", text)}
	}
	return oai.O("id", id, "type", "reasoning", "summary", summary)
}

// items are the output of a finished response: reasoning, message, calls.
func (v *respView) items(resp *core.Response) []any {
	out := make([]any, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		out = append(out, reasoningItem(itemID("rs", v.id, len(out)), resp.Reasoning))
	}
	if resp.Content != "" || len(resp.ToolCalls) == 0 {
		out = append(out, messageItem(itemID("msg", v.id, len(out)), resp.Content))
	}
	for _, tc := range resp.ToolCalls {
		out = append(out, callItem(itemID("fc", v.id, len(out)), "completed", tc.Arguments, tc))
	}
	return out
}

// writeResponses writes the non-streaming object, after the keep-alive
// whitespace (one block; the real sends one per second of wait).
func writeResponses(w core.ResponseWriter, req *core.Request, resp *core.Response) error {
	v := newRespView(req, resp)
	status, incomplete := respStatus(resp.FinishReason)
	// Every value is valid JSON (the echo was decoded from the body).
	b, _ := oai.Format{RawHTML: true, Prefix: bodyPrefix}.Marshal(v.object(status, v.items(resp), time.Now().Unix(), incomplete, respUsage(resp.Usage), serviceTier))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	w.Header("x-generation-id", resp.ID)
	w.Header("Access-Control-Expose-Headers", exposeHeaders)
	w.Header("Access-Control-Allow-Origin", allowOrigin)
	for k, val := range resp.Headers {
		w.Header(k, val)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// respStream is the event stream: "data: <json>" frames with type first
// and sequence_number last, closed by data: [DONE] (no event: lines).
type respStream struct {
	d   *Driver
	w   *sse.Writer
	rw  core.ResponseWriter
	req *core.Request
	v   *respView
	seq int
	out []any
	// open is the item being written: "", "message" or "reasoning".
	open   string
	itemID string
	text   bytes.Buffer
	calls  map[int]*respCall
}

type respCall struct {
	id    string
	tc    core.ToolCall
	index int
	args  bytes.Buffer
}

func newRespStream(d *Driver, w core.ResponseWriter, req *core.Request) *respStream {
	return &respStream{d: d, w: sse.New(w), rw: w, req: req, calls: map[int]*respCall{}}
}

func (s *respStream) event(typ string, kv ...any) error {
	o := oai.O(append(append([]any{"type", typ}, kv...), "sequence_number", s.seq)...)
	s.seq++
	b, _ := jsonFormat.Marshal(o) // valid JSON values only (see writeResponses)
	return s.w.Raw(append(append([]byte("data: "), b...), '\n', '\n'))
}

func (s *respStream) Start(resp *core.Response) error {
	s.v = newRespView(s.req, resp)
	s.rw.Status(200)
	s.rw.Header("Content-Type", "text/event-stream")
	s.rw.Header("Cache-Control", "no-cache")
	s.rw.Header("x-generation-id", resp.ID)
	s.rw.Header("Access-Control-Expose-Headers", exposeHeaders)
	s.rw.Header("Access-Control-Allow-Origin", allowOrigin)
	for k, v := range resp.Headers {
		s.rw.Header(k, v)
	}
	for _, typ := range []string{"response.created", "response.in_progress"} {
		if err := s.event(typ, "response", s.v.object("in_progress", []any{}, nil, nil, nil, "auto")); err != nil {
			return err
		}
	}
	return nil
}

func (s *respStream) added(item oai.Obj) error {
	return s.event("response.output_item.added", "output_index", len(s.out), "item", item)
}

func (s *respStream) done(item, final oai.Obj) error {
	err := s.event("response.output_item.done", "output_index", len(s.out), "item", item)
	s.out = append(s.out, final)
	return err
}

func (s *respStream) openItem(kind string) error {
	if s.open == kind {
		return nil
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	s.open = kind
	s.text.Reset()
	if kind == "reasoning" {
		s.itemID = itemID("rs", s.v.id, len(s.out))
		return s.added(reasoningItem(s.itemID, ""))
	}
	s.itemID = itemID("msg", s.v.id, len(s.out))
	if err := s.added(messageEvent(s.itemID, "in_progress", []any{})); err != nil {
		return err
	}
	return s.event("response.content_part.added", "output_index", len(s.out), "item_id", s.itemID, "content_index", 0, "part", outputText(""))
}

func (s *respStream) closeOpen() error {
	text := s.text.String()
	switch s.open {
	case "message":
		s.open = ""
		if err := s.event("response.output_text.done", "output_index", len(s.out), "item_id", s.itemID, "content_index", 0, "text", text, "logprobs", []any{}); err != nil {
			return err
		}
		if err := s.event("response.content_part.done", "output_index", len(s.out), "item_id", s.itemID, "content_index", 0, "part", outputText(text)); err != nil {
			return err
		}
		return s.done(messageEvent(s.itemID, "completed", []any{outputText(text)}), messageItem(s.itemID, text))
	case "reasoning":
		s.open = ""
		item := reasoningItem(s.itemID, text)
		return s.done(item, item)
	}
	return nil
}

func (s *respStream) ReasoningDelta(text string) error {
	if err := s.openItem("reasoning"); err != nil {
		return err
	}
	s.text.WriteString(text)
	return nil
}

func (s *respStream) TextDelta(text string) error {
	if err := s.openItem("message"); err != nil {
		return err
	}
	s.text.WriteString(text)
	return s.event("response.output_text.delta", "output_index", len(s.out), "item_id", s.itemID, "content_index", 0, "delta", text, "logprobs", []any{})
}

func (s *respStream) ToolCallStart(index int, id, name string) error {
	if err := s.closeOpen(); err != nil {
		return err
	}
	c := &respCall{id: itemID("fc", s.v.id, len(s.out)), tc: core.ToolCall{ID: id, Name: name}, index: len(s.out)}
	s.calls[index] = c
	return s.added(callItem(c.id, "in_progress", "", c.tc))
}

func (s *respStream) ToolArgsDelta(index int, text string) error {
	c := s.calls[index]
	if c == nil {
		return nil
	}
	c.args.WriteString(text)
	return s.event("response.function_call_arguments.delta", "output_index", c.index, "item_id", c.id, "delta", text)
}

func (s *respStream) ToolCallEnd(index int) error {
	c := s.calls[index]
	if c == nil {
		return nil
	}
	delete(s.calls, index)
	args := c.args.String()
	if err := s.event("response.function_call_arguments.done", "output_index", c.index, "item_id", c.id, "name", c.tc.Name, "arguments", args); err != nil {
		return err
	}
	item := callItem(c.id, "completed", args, c.tc)
	return s.done(item, item)
}

// Ping: not conferred on this route (the Chat stream's keep-alive comment).
func (s *respStream) Ping() error { return s.w.Comment(keepAliveComment) }

// Finish closes the open item and sends response.completed or
// response.incomplete, then [DONE].
func (s *respStream) Finish(reason core.FinishReason, usage core.Usage, _ bool) error {
	if len(s.out) == 0 && s.open == "" && len(s.calls) == 0 {
		if err := s.openItem("message"); err != nil {
			return err
		}
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	pending := make([]int, 0, len(s.calls))
	for i := range s.calls {
		pending = append(pending, i)
	}
	slices.Sort(pending)
	for _, i := range pending {
		if err := s.ToolCallEnd(i); err != nil {
			return err
		}
	}
	status, incomplete := respStatus(reason)
	obj := s.v.object(status, s.out, time.Now().Unix(), incomplete, respUsage(usage), serviceTier)
	if err := s.event("response."+status, "response", obj); err != nil {
		return err
	}
	return s.w.Done()
}

// Error sends the error as an event and closes the stream (not conferred).
func (s *respStream) Error(e *core.Error) error {
	status, body, _, _ := s.d.errorBody(s.req, e)
	msg := e.Message
	if b, ok := body.(oai.Obj); ok {
		if inner, ok := b.Get("error"); ok {
			if o, ok := inner.(oai.Obj); ok {
				if m, ok := o.Get("message"); ok {
					msg, _ = m.(string)
				}
			}
		}
	}
	if err := s.event("error", "code", status, "message", msg, "param", nil); err != nil {
		return err
	}
	return s.w.Done()
}

func (s *respStream) Raw(p []byte) error { return s.w.Raw(p) }
func (s *respStream) LastFrame() []byte  { return s.w.Last() }
func (s *respStream) Close() error       { return nil }
