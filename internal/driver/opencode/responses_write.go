package opencode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// The response object and the event stream of the portable Responses
// route (text, function_call, incomplete, qwen_text, text_stream,
// function_call_stream, field_*; task 240). The reasoning of the upstream
// always comes as a reasoning item with its summary text, counted in
// output_tokens. Not conferred: an
// incomplete stream (response.incomplete is assumed, as on OpenAI) and
// mid-stream errors.

// respEcho is what the object repeats from the request.
type respEcho struct {
	Instructions      json.RawMessage   `json:"instructions"`
	Metadata          json.RawMessage   `json:"metadata"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Temperature       json.RawMessage   `json:"temperature"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	Tools             []json.RawMessage `json:"tools"`
	TopP              json.RawMessage   `json:"top_p"`
	MaxOutputTokens   json.RawMessage   `json:"max_output_tokens"`
	// Only when sent, after max_output_tokens (field_reasoning,
	// field_text, field_truncation; their relative order is not
	// conferred).
	Reasoning  json.RawMessage `json:"reasoning"`
	Text       json.RawMessage `json:"text"`
	Truncation json.RawMessage `json:"truncation"`
}

type respView struct {
	req     *core.Request
	id      string
	model   string
	created int64
	echo    respEcho
}

func newRespView(req *core.Request, resp *core.Response) *respView {
	v := &respView{req: req, id: resp.ID, model: resp.Model, created: resp.Created.Unix()}
	if req != nil {
		_ = json.Unmarshal(req.Raw, &v.echo)
	}
	return v
}

func present(raw json.RawMessage) bool { return len(bytes.TrimSpace(raw)) > 0 }

func rawOr(raw json.RawMessage, def any) any {
	if !present(raw) {
		return def
	}
	return raw
}

// object is the response object in the gateway's order.
func (v *respView) object(status string, output []any, incomplete, usage any) oai.Obj {
	e := &v.echo
	parallel := true
	if e.ParallelToolCalls != nil {
		parallel = *e.ParallelToolCalls
	}
	tools := make([]any, 0, len(e.Tools))
	for _, t := range e.Tools {
		tools = append(tools, t)
	}
	o := oai.O(
		"id", v.id, "object", "response", "created_at", v.created, "status", status, "error", nil,
		"incomplete_details", incomplete, "instructions", rawOr(e.Instructions, nil), "metadata", rawOr(e.Metadata, oai.Obj{}),
		"model", v.model, "output", output, "parallel_tool_calls", parallel, "temperature", rawOr(e.Temperature, nil),
		"tool_choice", rawOr(e.ToolChoice, "auto"), "tools", tools, "top_p", rawOr(e.TopP, nil),
		"max_output_tokens", rawOr(e.MaxOutputTokens, nil),
	)
	for _, f := range []struct {
		k string
		v json.RawMessage
	}{{"reasoning", e.Reasoning}, {"text", e.Text}, {"truncation", e.Truncation}} {
		if present(f.v) {
			o = append(o, oai.Field{Key: f.k, Val: f.v})
		}
	}
	return append(o, oai.Field{Key: "usage", Val: usage})
}

// usage is the usage object: OpenAI's counts and the gateway's cost and
// opencode. reasoning_tokens (and opencode.tokens.reasoning) carry the
// reasoning, as on the Chat route: the gateway spreads the same request
// over two upstream pools (task 396, 15 calls of shape/responses on
// 2026-10-31, no header tells them apart); one reports the reasoning on
// both routes, the other reports 0 here and the reasoning with
// accepted/rejected_prediction_tokens on Chat (responses/*.json, task
// 240). The wwz copies the first one, the pool its Chat shape follows.
func (v *respView) usage(u core.Usage) oai.Obj {
	cost, oc := opencodeUsage(v.req, u, true)
	return oai.O(
		"input_tokens", u.PromptTokens, "input_tokens_details", oai.O("cached_tokens", u.CachedTokens),
		"output_tokens", u.CompletionTokens, "output_tokens_details", oai.O("reasoning_tokens", u.ReasoningTokens),
		"total_tokens", u.TotalTokens(), "cost", cost, "opencode", oc,
	)
}

func respStatus(fr core.FinishReason) (string, any) {
	if fr == core.FinishLength {
		return "incomplete", oai.O("reason", "max_output_tokens")
	}
	return "completed", nil
}

// respItemID is "<prefix>_" and 32 hex, derived from the response id and
// the item's place (the real ones are random: rs_f076b63a…).
func respItemID(prefix, respID string, i int) string {
	sum := sha256.Sum256([]byte(respID + "\x00" + prefix + "\x00" + strconv.Itoa(i)))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func textPart(text string) oai.Obj {
	return oai.O("type", "output_text", "text", text, "annotations", []any{}, "logprobs", []any{})
}

func messageItem(id, status string, content []any) oai.Obj {
	return oai.O("id", id, "type", "message", "role", "assistant", "status", status, "content", content)
}

func callItem(id, status, args string, tc core.ToolCall) oai.Obj {
	return oai.O("id", id, "type", "function_call", "call_id", tc.ID, "name", tc.Name, "arguments", args, "status", status)
}

func summaryPart(text string) oai.Obj { return oai.O("type", "summary_text", "text", text) }

func reasoningItem(id string, summary []any) oai.Obj {
	return oai.O("id", id, "type", "reasoning", "summary", summary)
}

// messageStatus: a message cut by max_output_tokens is incomplete
// (qwen_text).
func messageStatus(fr core.FinishReason) string {
	if fr == core.FinishLength {
		return "incomplete"
	}
	return "completed"
}

// items are the output of a finished response: reasoning, the message
// when there is text (an answer cut inside the reasoning has none:
// incomplete), the calls.
func (v *respView) items(resp *core.Response) []any {
	out := make([]any, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		out = append(out, reasoningItem(respItemID("rs", v.id, len(out)), []any{summaryPart(resp.Reasoning)}))
	}
	if resp.Content != "" || (len(resp.ToolCalls) == 0 && resp.Reasoning == "") {
		out = append(out, messageItem(respItemID("msg", v.id, len(out)), messageStatus(resp.FinishReason), []any{textPart(resp.Content)}))
	}
	for _, tc := range resp.ToolCalls {
		out = append(out, callItem(respItemID("fc", v.id, len(out)), "completed", tc.Arguments, tc))
	}
	return out
}

func writeResponses(w core.ResponseWriter, req *core.Request, resp *core.Response) error {
	v := newRespView(req, resp)
	status, incomplete := respStatus(resp.FinishReason)
	// Every value is valid JSON (the echo was decoded from the body).
	b, _ := jsonFormat.Marshal(v.object(status, v.items(resp), incomplete, v.usage(resp.Usage)))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	for k, val := range successHeaders(nil, resp, nil) {
		w.Header(k, val)
	}
	for k, val := range resp.Headers {
		w.Header(k, val)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// respItem is one output item of the stream.
type respItem struct {
	kind  string // reasoning, message, function_call
	id    string
	index int
	text  bytes.Buffer
	tc    core.ToolCall
}

// respStream is the event stream: "event: <type>" and "data: <json>" with
// the payload first, then type and sequence_number; no [DONE]. Every item
// opens as its first delta comes, but all the done events wait for the end
// and come in item order (text_stream, function_call_stream).
type respStream struct {
	d     *Driver
	w     *sse.Writer
	rw    core.ResponseWriter
	req   *core.Request
	v     *respView
	seq   int
	items []*respItem
	open  *respItem // the reasoning or message item taking deltas
	calls map[int]*respItem
}

func newRespStream(d *Driver, w core.ResponseWriter, req *core.Request) *respStream {
	return &respStream{d: d, w: sse.New(w), rw: w, req: req, calls: map[int]*respItem{}}
}

func (s *respStream) event(typ string, kv ...any) error {
	o := oai.O(append(kv, "type", typ, "sequence_number", s.seq)...)
	s.seq++
	b, _ := jsonFormat.Marshal(o) // valid JSON values only (see writeResponses)
	frame := make([]byte, 0, len(b)+len(typ)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, typ...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, b...)
	return s.w.Raw(append(frame, '\n', '\n'))
}

func (s *respStream) Start(resp *core.Response) error {
	s.v = newRespView(s.req, resp)
	s.rw.Status(200)
	for k, v := range successHeaders(s.req, resp, nil) {
		s.rw.Header(k, v)
	}
	s.rw.Header("Cache-Control", "no-cache")
	for k, v := range resp.Headers {
		s.rw.Header(k, v)
	}
	for _, typ := range []string{"response.created", "response.in_progress"} {
		if err := s.event(typ, "response", s.v.object("in_progress", []any{}, nil, nil)); err != nil {
			return err
		}
	}
	return nil
}

func (s *respStream) add(kind, prefix string) (*respItem, error) {
	it := &respItem{kind: kind, index: len(s.items), id: respItemID(prefix, s.v.id, len(s.items))}
	s.items = append(s.items, it)
	var item oai.Obj
	switch kind {
	case "reasoning":
		item = reasoningItem(it.id, []any{})
	case "message":
		item = messageItem(it.id, "in_progress", []any{})
	}
	if item == nil {
		return it, nil
	}
	if err := s.event("response.output_item.added", "output_index", it.index, "item", item); err != nil {
		return nil, err
	}
	if kind == "reasoning" {
		return it, s.event("response.reasoning_summary_part.added", "item_id", it.id, "output_index", it.index, "summary_index", 0, "part", summaryPart(""))
	}
	return it, s.event("response.content_part.added", "item_id", it.id, "output_index", it.index, "content_index", 0, "part", textPart(""))
}

func (s *respStream) openItem(kind, prefix string) (*respItem, error) {
	if s.open != nil && s.open.kind == kind {
		return s.open, nil
	}
	it, err := s.add(kind, prefix)
	s.open = it
	return it, err
}

func (s *respStream) ReasoningDelta(text string) error {
	it, err := s.openItem("reasoning", "rs")
	if err != nil {
		return err
	}
	it.text.WriteString(text)
	return s.event("response.reasoning_summary_text.delta", "item_id", it.id, "output_index", it.index, "summary_index", 0, "delta", text)
}

func (s *respStream) TextDelta(text string) error {
	it, err := s.openItem("message", "msg")
	if err != nil {
		return err
	}
	it.text.WriteString(text)
	return s.event("response.output_text.delta", "item_id", it.id, "output_index", it.index, "content_index", 0, "delta", text, "logprobs", []any{})
}

func (s *respStream) ToolCallStart(index int, id, name string) error {
	s.open = nil
	it, _ := s.add("function_call", "fc")
	it.tc = core.ToolCall{ID: id, Name: name}
	s.calls[index] = it
	return s.event("response.output_item.added", "output_index", it.index, "item", callItem(it.id, "in_progress", "", it.tc))
}

func (s *respStream) ToolArgsDelta(index int, text string) error {
	it := s.calls[index]
	if it == nil {
		return nil
	}
	it.text.WriteString(text)
	return s.event("response.function_call_arguments.delta", "item_id", it.id, "output_index", it.index, "delta", text)
}

// ToolCallEnd: the call's done events wait for Finish.
func (s *respStream) ToolCallEnd(int) error { return nil }

// Ping is an SSE comment (the real keep-alive is not conferred).
func (s *respStream) Ping() error { return s.w.Comment("ping") }

// finalItem is the item as output_item.done and the final object show it.
func finalItem(it *respItem, fr core.FinishReason) oai.Obj {
	text := it.text.String()
	switch it.kind {
	case "reasoning":
		return reasoningItem(it.id, []any{summaryPart(text)})
	case "message":
		return messageItem(it.id, messageStatus(fr), []any{textPart(text)})
	}
	tc := it.tc
	return callItem(it.id, "completed", text, tc)
}

// itemDone sends the done events of one item.
func (s *respStream) itemDone(it *respItem, final oai.Obj) error {
	text := it.text.String()
	var err error
	switch it.kind {
	case "reasoning":
		err = s.event("response.reasoning_summary_text.done", "item_id", it.id, "output_index", it.index, "summary_index", 0, "text", text)
		if err == nil {
			err = s.event("response.reasoning_summary_part.done", "item_id", it.id, "output_index", it.index, "summary_index", 0, "part", summaryPart(text))
		}
	case "message":
		err = s.event("response.output_text.done", "item_id", it.id, "output_index", it.index, "content_index", 0, "text", text, "logprobs", []any{})
		if err == nil {
			err = s.event("response.content_part.done", "item_id", it.id, "output_index", it.index, "content_index", 0, "part", textPart(text))
		}
	default:
		err = s.event("response.function_call_arguments.done", "item_id", it.id, "output_index", it.index, "arguments", text, "name", it.tc.Name)
	}
	if err != nil {
		return err
	}
	return s.event("response.output_item.done", "output_index", it.index, "item", final)
}

// Finish sends every item's done events, in item order, then
// response.completed (or response.incomplete).
func (s *respStream) Finish(reason core.FinishReason, usage core.Usage, _ bool) error {
	if len(s.items) == 0 {
		if _, err := s.openItem("message", "msg"); err != nil {
			return err
		}
	}
	out := make([]any, 0, len(s.items))
	for _, it := range s.items {
		final := finalItem(it, reason)
		if err := s.itemDone(it, final); err != nil {
			return err
		}
		out = append(out, final)
	}
	status, incomplete := respStatus(reason)
	return s.event("response."+status, "response", s.v.object(status, out, incomplete, s.v.usage(usage)))
}

// Error sends the route's error envelope as an error event (not
// conferred).
func (s *respStream) Error(e *core.Error) error {
	_, body, _, _ := s.d.errorBody(&core.Request{Endpoint: core.EndpointChat}, e)
	b, _ := body.([]byte)
	return s.w.Raw(append(append([]byte("event: error\ndata: "), b...), '\n', '\n'))
}

func (s *respStream) Raw(p []byte) error { return s.w.Raw(p) }
func (s *respStream) LastFrame() []byte  { return s.w.Last() }
func (s *respStream) Close() error       { return nil }
