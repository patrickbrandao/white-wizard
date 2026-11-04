package oai

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// RespStream is the Responses API event stream (text_stream,
// function_call_stream, incomplete_stream): every frame is "event: <type>"
// and "data: <json>", sequence_number counts from 0, there is no [DONE].
// Each event has type first and the other keys in alphabetical order.
type RespStream struct {
	h   *Hooks
	w   *sse.Writer
	rw  core.ResponseWriter
	req *core.Request
	rl  *core.RateLimitState
	v   *respView
	obf *obfuscator
	seq int
	buf []byte

	out []any // finished items
	// open is the item being written: "" none, "message" or "reasoning".
	open   string
	itemID string
	text   strings.Builder
	tools  map[int]*respCall
}

// respCall is a function_call item being streamed.
type respCall struct {
	id    string
	tc    core.ToolCall
	index int
	args  strings.Builder
}

// NewRespStream creates the Responses event writer.
func NewRespStream(h *Hooks, w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) *RespStream {
	return &RespStream{h: h, w: sse.New(w), rw: w, req: req, rl: rl, tools: map[int]*respCall{}}
}

// event writes one frame; o starts with type.
func (s *RespStream) event(o Obj) error {
	b, err := s.h.ChunkFormat.Append(s.buf[:0], o)
	if err != nil {
		return err
	}
	typ, _ := o[0].Val.(string)
	frame := make([]byte, 0, len(b)+len(typ)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, typ...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, b...)
	frame = append(frame, '\n', '\n')
	s.buf = b
	return s.w.Raw(frame)
}

// next is the sequence number of the next event.
func (s *RespStream) next() int {
	n := s.seq
	s.seq++
	return n
}

func (s *RespStream) obfuscation() (Field, bool) {
	if s.obf == nil {
		return Field{}, false
	}
	return Field{"obfuscation", s.obf.next()}, true
}

func (s *RespStream) Start(resp *core.Response) error {
	s.v = newRespView(s.req, resp)
	if s.h.Obfuscation && includeObfuscation(s.req) {
		s.obf = newObfuscator(resp.ID)
	}
	s.rw.Status(200)
	s.rw.Header("Content-Type", "text/event-stream; charset=utf-8")
	s.rw.Header("Cache-Control", "no-cache")
	s.rw.Header("Connection", "keep-alive")
	s.rw.Header("X-Accel-Buffering", "no")
	s.h.successHeaders(s.rw, s.req, resp, s.rl)
	for k, v := range resp.Headers {
		s.rw.Header(k, v)
	}
	for _, typ := range []string{"response.created", "response.in_progress"} {
		obj := s.v.object("in_progress", []any{}, nil, nil, nil, false, s.v.requestedTier())
		if err := s.event(O("type", typ, "response", obj, "sequence_number", s.next())); err != nil {
			return err
		}
	}
	return nil
}

// itemAdded opens an output item.
func (s *RespStream) itemAdded(item Obj) error {
	return s.event(O("type", "response.output_item.added", "item", item, "output_index", len(s.out), "sequence_number", s.next()))
}

// itemDone closes the current output item.
func (s *RespStream) itemDone(item Obj) error {
	err := s.event(O("type", "response.output_item.done", "item", item, "output_index", len(s.out), "sequence_number", s.next()))
	s.out = append(s.out, item)
	return err
}

func (s *RespStream) openMessage() error {
	if s.open == "message" {
		return nil
	}
	if err := s.closeOpen(core.FinishStop); err != nil {
		return err
	}
	s.open, s.itemID = "message", itemID("msg", s.v.id, s.v.created, len(s.out))
	s.text.Reset()
	if err := s.itemAdded(messageItem(s.itemID, "in_progress", []any{})); err != nil {
		return err
	}
	return s.event(O("type", "response.content_part.added", "content_index", 0, "item_id", s.itemID, "output_index", len(s.out), "part", textPart(""), "sequence_number", s.next()))
}

func (s *RespStream) openReasoning() error {
	if s.open == "reasoning" {
		return nil
	}
	if err := s.closeOpen(core.FinishStop); err != nil {
		return err
	}
	s.open, s.itemID = "reasoning", itemID("rs", s.v.id, s.v.created, len(s.out))
	s.text.Reset()
	item := O("id", s.itemID, "type", "reasoning", "summary", []any{})
	if err := s.itemAdded(item); err != nil {
		return err
	}
	if !s.v.summary {
		return nil
	}
	return s.event(O("type", "response.reasoning_summary_part.added", "item_id", s.itemID, "output_index", len(s.out), "part", O("type", "summary_text", "text", ""), "sequence_number", s.next(), "summary_index", 0))
}

// closeOpen finishes the open message or reasoning item.
func (s *RespStream) closeOpen(fr core.FinishReason) error {
	text := s.text.String()
	switch s.open {
	case "message":
		s.open = ""
		if err := s.event(O("type", "response.output_text.done", "content_index", 0, "item_id", s.itemID, "logprobs", []any{}, "output_index", len(s.out), "sequence_number", s.next(), "text", text)); err != nil {
			return err
		}
		part := outputPart(text, fr)
		if err := s.event(O("type", "response.content_part.done", "content_index", 0, "item_id", s.itemID, "output_index", len(s.out), "part", part, "sequence_number", s.next())); err != nil {
			return err
		}
		status, _ := respStatus(fr)
		return s.itemDone(messageItem(s.itemID, status, []any{part}))
	case "reasoning":
		s.open = ""
		if s.v.summary {
			if err := s.event(O("type", "response.reasoning_summary_text.done", "item_id", s.itemID, "output_index", len(s.out), "sequence_number", s.next(), "summary_index", 0, "text", text)); err != nil {
				return err
			}
			if err := s.event(O("type", "response.reasoning_summary_part.done", "item_id", s.itemID, "output_index", len(s.out), "part", O("type", "summary_text", "text", text), "sequence_number", s.next(), "summary_index", 0)); err != nil {
				return err
			}
		}
		return s.itemDone(s.v.reasoningItem(s.itemID, text))
	}
	return nil
}

func (s *RespStream) ReasoningDelta(text string) error {
	if err := s.openReasoning(); err != nil {
		return err
	}
	s.text.WriteString(text)
	if !s.v.summary {
		return nil
	}
	o := O("type", "response.reasoning_summary_text.delta", "delta", text, "item_id", s.itemID)
	if f, ok := s.obfuscation(); ok {
		o = append(o, f)
	}
	return s.event(append(o, Field{"output_index", len(s.out)}, Field{"sequence_number", s.next()}, Field{"summary_index", 0}))
}

func (s *RespStream) TextDelta(text string) error {
	if err := s.openMessage(); err != nil {
		return err
	}
	s.text.WriteString(text)
	o := O("type", "response.output_text.delta", "content_index", 0, "delta", text, "item_id", s.itemID, "logprobs", []any{})
	if f, ok := s.obfuscation(); ok {
		o = append(o, f)
	}
	return s.event(append(o, Field{"output_index", len(s.out)}, Field{"sequence_number", s.next()}))
}

func (s *RespStream) ToolCallStart(index int, id, name string) error {
	if err := s.closeOpen(core.FinishStop); err != nil {
		return err
	}
	c := &respCall{id: itemID("fc", s.v.id, s.v.created, len(s.out)), tc: core.ToolCall{ID: id, Name: name}, index: len(s.out)}
	s.tools[index] = c
	return s.itemAdded(callItem(c.id, "in_progress", "", c.tc))
}

func (s *RespStream) ToolArgsDelta(index int, text string) error {
	c := s.tools[index]
	if c == nil {
		return nil
	}
	c.args.WriteString(text)
	o := O("type", "response.function_call_arguments.delta", "delta", text, "item_id", c.id)
	if f, ok := s.obfuscation(); ok {
		o = append(o, f)
	}
	return s.event(append(o, Field{"output_index", c.index}, Field{"sequence_number", s.next()}))
}

func (s *RespStream) ToolCallEnd(index int) error {
	c := s.tools[index]
	if c == nil {
		return nil
	}
	delete(s.tools, index)
	args := c.args.String()
	if err := s.event(O("type", "response.function_call_arguments.done", "arguments", args, "item_id", c.id, "output_index", c.index, "sequence_number", s.next())); err != nil {
		return err
	}
	c.tc.Arguments = args
	return s.itemDone(callItem(c.id, "completed", args, c.tc))
}

// Ping is an SSE comment (the real keep-alive is not conferred).
func (s *RespStream) Ping() error { return s.w.Comment("ping") }

// Finish closes the open item and sends response.completed or
// response.incomplete with the whole response. A response with no item
// still carries its (empty) message, like the non-streaming body.
func (s *RespStream) Finish(reason core.FinishReason, usage core.Usage, includeUsage bool) error {
	if len(s.out) == 0 && s.open == "" && len(s.tools) == 0 {
		if err := s.openMessage(); err != nil {
			return err
		}
	}
	if err := s.closeOpen(reason); err != nil {
		return err
	}
	pending := make([]int, 0, len(s.tools))
	for i := range s.tools {
		pending = append(pending, i)
	}
	slices.Sort(pending)
	for _, i := range pending {
		if err := s.ToolCallEnd(i); err != nil {
			return err
		}
	}
	status, incomplete := respStatus(reason)
	var u any
	if includeUsage {
		u = ResponsesUsage(usage)
	}
	obj := s.h.shape(PartResponse, s.v.object(status, s.out, u, completedAt(status), incomplete, false, s.v.finalTier()))
	return s.event(O("type", "response."+status, "response", obj, "sequence_number", s.next()))
}

// Error sends the error event: {type, code, message, param,
// sequence_number} (spec 006; not conferred).
func (s *RespStream) Error(e *core.Error) error {
	_, body, _, _ := OpenAIErrorBody(s.req, e)
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	b, _ := body.([]byte)
	if json.Unmarshal(b, &env) != nil {
		env.Error.Message, env.Error.Type = e.Message, "server_error"
	}
	code := env.Error.Code
	if code == nil {
		code = env.Error.Type
	}
	return s.event(O("type", "error", "code", code, "message", env.Error.Message, "param", env.Error.Param, "sequence_number", s.next()))
}

func (s *RespStream) Raw(p []byte) error { return s.w.Raw(p) }
func (s *RespStream) LastFrame() []byte  { return s.w.Last() }
func (s *RespStream) Close() error       { return nil }
