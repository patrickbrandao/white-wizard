package oai

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// Part names an object the dialect writes; Hooks.Shape receives it.
type Part string

const (
	// PartChat is the top level of chat.completion and text_completion.
	PartChat Part = "chat"
	// PartChoice is one choice of a non-streaming body.
	PartChoice Part = "choice"
	// PartMessage is choices[].message.
	PartMessage Part = "message"
	// PartUsage is the usage object (body and usage chunk).
	PartUsage Part = "usage"
	// PartChunk is the top level of every stream chunk, the usage chunk
	// included.
	PartChunk Part = "chunk"
	// PartChunkChoice is one choice of a chunk.
	PartChunkChoice Part = "chunk_choice"
	// PartDelta is choices[].delta of a chunk.
	PartDelta Part = "delta"
	// PartResponse is the final response object of the Responses API: the
	// non-streaming body and the response of the closing event
	// (response.completed, response.incomplete).
	PartResponse Part = "response"
)

// Hooks customise the dialect for a specific gateway.
type Hooks struct {
	Name string
	// IDPrefix for completion ids ("chatcmpl").
	IDPrefix string
	// ReasoningField: hidden | reasoning_content | reasoning | inline_tag.
	// hidden (OpenAI's own Chat Completions, ADR 0022) generates and counts
	// the reasoning (usage, cost, TTFT) but never serialises its text.
	ReasoningField string
	// ReasoningDetails also serialises the text as reasoning_details[]
	// ({type: "reasoning.text", text, signature, format, index}) next to
	// message.reasoning and delta.reasoning (OpenRouter, spec 006).
	ReasoningDetails bool
	// UsageExtras adds dialect fields to the usage object (appended after
	// the OpenAI fields, in sorted key order).
	UsageExtras func(req *core.Request, resp *core.Response, u core.Usage) map[string]any
	// TopExtras adds top-level fields to responses and chunks (e.g. provider).
	TopExtras func(req *core.Request, resp *core.Response) map[string]any
	// Shape, when set, gets every object before it is written and returns
	// it reordered or with fields removed: the gateways' own field order and
	// omissions (spec 006 16.7). Obj.Reorder and Obj.Delete do the work.
	Shape func(part Part, o Obj) Obj
	// KeepAlive is an SSE comment sent before the first chunk ("" = none).
	KeepAlive string
	// FinalChunkWithChoice: emit the usage chunk with one empty-delta choice
	// repeating finish_reason (OpenRouter) instead of choices: [] (OpenAI).
	FinalChunkWithChoice bool
	// NativeFinishReason adds native_finish_reason (OpenRouter).
	NativeFinishReason bool
	// NativeFinish, when set, is the native_finish_reason value (the
	// upstream's vocabulary), and every chunk choice carries the field,
	// null until the finish (OpenRouter, spec 006 16.7).
	NativeFinish func(req *core.Request, fr core.FinishReason) any
	// NoRoleChunk: the stream has no leading role chunk; the first delta
	// already carries content (OpenRouter puts the role in every delta
	// through Shape).
	NoRoleChunk bool
	// FinishUsage: the closing chunk (empty delta and finish_reason)
	// carries usage itself, with or without include_usage, and no separate
	// usage chunk follows (OpenCode, spec 006 16.7).
	FinishUsage bool
	// ErrorBody renders the error envelope; returns status, body (any JSON
	// value, or []byte written verbatim), content type and extra headers.
	ErrorBody func(req *core.Request, e *core.Error) (int, any, string, map[string]string)
	// RateLimitHeaders renders limit headers when the limiter reported state.
	RateLimitHeaders func(rl *core.RateLimitState) map[string]string
	// SuccessHeaders, when set, renders every dialect header of a 200 (body
	// and stream) instead of RateLimitHeaders: OpenAI sends its account,
	// version and limit headers on every success (ADR 0049).
	SuccessHeaders func(req *core.Request, resp *core.Response, rl *core.RateLimitState) map[string]string
	// RequestIDHeader is written on errors that reach the driver without the
	// engine's request id (parse errors, /models): the request's own id, or
	// a fresh one ("" = none).
	RequestIDHeader string
	// StreamErrorBody renders the mid-stream error chunk body.
	StreamErrorBody func(e *core.Error) any
	// SystemFingerprint value ("" omits).
	SystemFingerprint string
	// ServiceTier is the service_tier of bodies and chunks ("" omits).
	ServiceTier string
	// Annotations adds annotations: [] to the message (OpenAI).
	Annotations bool
	// ChunkUsageNull: with include_usage, every chunk before the usage chunk
	// carries usage: null (OpenAI).
	ChunkUsageNull bool
	// Obfuscation adds a random obfuscation string to every chunk (OpenAI;
	// stream_options.include_obfuscation: false turns it off).
	Obfuscation bool
	// BodyFormat is the byte layout of a non-streaming success body.
	BodyFormat Format
	// ChunkFormat is the byte layout of each chunk (compact: Indent "").
	ChunkFormat Format
}

// ReasoningHidden is the ReasoningField value that omits the reasoning text.
const ReasoningHidden = "hidden"

// reasoningDetailFormat is the format OpenRouter reports for reasoning
// text from providers without a native signed format.
const reasoningDetailFormat = "unknown"

// reasoningDetails renders the OpenRouter reasoning_details[] array.
func reasoningDetails(text string) []any {
	return []any{O("type", "reasoning.text", "text", text, "signature", nil, "format", reasoningDetailFormat, "index", 0)}
}

// hideReasoning reports whether the reasoning text stays out of the body:
// the dialect hides it or the request asked for exclude (OpenRouter).
func (h *Hooks) hideReasoning(req *core.Request) bool {
	return h.ReasoningField == ReasoningHidden || (req != nil && req.Reasoning.Exclude)
}

// native is the native_finish_reason of fr.
func (h *Hooks) native(req *core.Request, fr core.FinishReason) any {
	if h.NativeFinish != nil {
		return h.NativeFinish(req, fr)
	}
	return string(fr)
}

func (h *Hooks) shape(p Part, o Obj) Obj {
	if h.Shape != nil {
		return h.Shape(p, o)
	}
	return o
}

// openAIFingerprint is system_fingerprint: "fp_" + 10 hex, like the real.
const openAIFingerprint = "fp_3a7e128613"

// DefaultHooks returns OpenAI's own behaviour (spec 006 16.7).
func DefaultHooks() Hooks {
	return Hooks{
		Name: "openai", IDPrefix: "chatcmpl", ReasoningField: ReasoningHidden,
		ErrorBody: OpenAIErrorBody, RateLimitHeaders: OpenAIRateLimitHeaders, SuccessHeaders: OpenAISuccessHeaders,
		RequestIDHeader: "x-request-id", StreamErrorBody: OpenAIStreamError, SystemFingerprint: openAIFingerprint,
		ServiceTier: "default", Annotations: true, ChunkUsageNull: true, Obfuscation: true,
		BodyFormat: Format{Indent: "  ", Newline: true, RawHTML: true}, ChunkFormat: Format{RawHTML: true},
	}
}

// UsageJSON builds the usage object, in the real order.
func UsageJSON(h *Hooks, req *core.Request, resp *core.Response, u core.Usage) Obj {
	o := Obj{
		{"prompt_tokens", u.PromptTokens},
		{"completion_tokens", u.CompletionTokens},
		{"total_tokens", u.TotalTokens()},
		{"prompt_tokens_details", O("cached_tokens", u.CachedTokens, "audio_tokens", 0)},
		{"completion_tokens_details", O("reasoning_tokens", u.ReasoningTokens, "audio_tokens", 0, "accepted_prediction_tokens", 0, "rejected_prediction_tokens", 0)},
	}
	if h.UsageExtras != nil {
		o.Merge(h.UsageExtras(req, resp, u))
	}
	return h.shape(PartUsage, o)
}

func finishReason(fr core.FinishReason) any {
	switch fr {
	case "":
		return nil
	case core.FinishRefusal:
		return "stop"
	}
	return string(fr)
}

func toolCallsJSON(tcs []core.ToolCall) []any {
	out := make([]any, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, O("id", tc.ID, "type", "function", "function", O("name", tc.Name, "arguments", tc.Arguments)))
	}
	return out
}

func (h *Hooks) withTop(req *core.Request, resp *core.Response, o Obj) Obj {
	if h.TopExtras != nil {
		o.Merge(h.TopExtras(req, resp))
	}
	return o
}

// message builds choices[].message: role, content, tool_calls, refusal,
// the reasoning fields of the gateways, annotations.
func (h *Hooks) message(req *core.Request, resp *core.Response) (Obj, string) {
	content := resp.Content
	var reasoning Obj
	if resp.Reasoning != "" && !h.hideReasoning(req) {
		switch h.ReasoningField {
		case "inline_tag":
			content = "<thinking>" + resp.Reasoning + "</thinking>" + content
		case "reasoning":
			reasoning = Obj{{"reasoning", resp.Reasoning}}
			if h.ReasoningDetails {
				reasoning = append(reasoning, Field{"reasoning_details", reasoningDetails(resp.Reasoning)})
			}
		default:
			reasoning = Obj{{"reasoning_content", resp.Reasoning}}
		}
	}
	var c, refusal any
	if content != "" || len(resp.ToolCalls) == 0 {
		c = content
	}
	if resp.FinishReason == core.FinishRefusal {
		refusal, c = content, nil
	}
	msg := Obj{{"role", "assistant"}, {"content", c}}
	if len(resp.ToolCalls) > 0 {
		msg = append(msg, Field{"tool_calls", toolCallsJSON(resp.ToolCalls)})
	}
	msg = append(msg, Field{"refusal", refusal})
	msg = append(msg, reasoning...)
	if h.Annotations {
		msg = append(msg, Field{"annotations", []any{}})
	}
	return h.shape(PartMessage, msg), content
}

// WriteChat writes a non-streaming chat.completion (or text_completion).
func WriteChat(h *Hooks, w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	msg, content := h.message(req, resp)
	fr := finishReason(resp.FinishReason)
	body := Obj{{"id", resp.ID}, {"object", "chat.completion"}, {"created", resp.Created.Unix()}, {"model", resp.Model}}
	var choice Obj
	if req.Endpoint == core.EndpointCompletions {
		body[1].Val = "text_completion"
		choice = Obj{{"text", content}, {"index", 0}, {"logprobs", nil}, {"finish_reason", fr}}
	} else {
		choice = Obj{{"index", 0}, {"message", msg}, {"logprobs", nil}, {"finish_reason", fr}}
	}
	if h.NativeFinishReason {
		choice = append(choice, Field{"native_finish_reason", h.native(req, resp.FinishReason)})
	}
	body = append(body, Field{"choices", []any{h.shape(PartChoice, choice)}}, Field{"usage", UsageJSON(h, req, resp, resp.Usage)})
	if h.ServiceTier != "" && req.Endpoint != core.EndpointCompletions {
		body = append(body, Field{"service_tier", h.ServiceTier})
	}
	if h.SystemFingerprint != "" {
		body = append(body, Field{"system_fingerprint", h.SystemFingerprint})
	}
	body = h.shape(PartChat, h.withTop(req, resp, body))
	b, err := h.BodyFormat.Marshal(body)
	if err != nil {
		return err
	}
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	h.successHeaders(w, req, resp, rl)
	for k, v := range resp.Headers {
		w.Header(k, v)
	}
	if _, err = w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// successHeaders writes the dialect headers of a 200.
func (h *Hooks) successHeaders(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) {
	if h.SuccessHeaders != nil {
		for k, v := range h.SuccessHeaders(req, resp, rl) {
			w.Header(k, v)
		}
		return
	}
	if rl != nil && rl.Present && h.RateLimitHeaders != nil {
		for k, v := range h.RateLimitHeaders(rl) {
			w.Header(k, v)
		}
	}
}

// WriteError renders an error through the hooks.
func WriteError(h *Hooks, w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if req == nil {
		req = &core.Request{}
	}
	status, body, ct, hdr := h.ErrorBody(req, e)
	// A chaos/next raw body replaces the envelope; status and headers stay.
	b, ct := e.RenderBody(body, ct)
	w.Status(status)
	w.Header("Content-Type", ct)
	w.Header("Content-Length", strconv.Itoa(len(b)))
	if rl != nil && rl.Present && h.RateLimitHeaders != nil {
		for k, v := range h.RateLimitHeaders(rl) {
			w.Header(k, v)
		}
	}
	for k, v := range hdr {
		w.Header(k, v)
	}
	if h.RequestIDHeader != "" {
		if _, ok := e.Extra["header:"+h.RequestIDHeader]; !ok {
			id := req.RequestID
			if id == "" {
				id = NewRequestID()
			} else if !strings.HasPrefix(id, "req_") {
				id = "req_" + id
			}
			w.Header(h.RequestIDHeader, id)
		}
	}
	for k, v := range e.Extra {
		if strings.HasPrefix(k, "header:") {
			if s, ok := v.(string); ok {
				w.Header(strings.TrimPrefix(k, "header:"), s)
			}
		}
	}
	_, _ = w.Write(b)
	_ = w.Flush()
}

// --- streaming ---

// Stream is the OpenAI chunk StreamWriter.
type Stream struct {
	h          *Hooks
	w          *sse.Writer
	rw         core.ResponseWriter
	req        *core.Request
	resp       *core.Response
	started    bool
	closed     bool
	rl         *core.RateLimitState
	inline     bool
	inlineOpen bool
	hidden     bool
	// roleDue: the role chunk waits for the first visible delta, so the
	// silence of hidden reasoning lands before the first chunk (TTFT).
	roleDue bool
	// usageNull: chunks carry usage: null (include_usage on OpenAI).
	usageNull bool
	// obf draws the obfuscation strings (nil = none).
	obf *obfuscator
	buf []byte
	// reason is the finish reason once Finish runs (native_finish_reason).
	reason core.FinishReason
	// obfFixed replaces the obfuscation draw while a template is rendered.
	obfFixed string
	// text is the pre-rendered content-delta chunk (nil: not built yet;
	// textOff: the hooks make it unsafe, render each chunk).
	text    *FrameTemplate
	textOff bool
	// headFields is head() built once at Start, its values already boxed;
	// scratch is the Obj every chunk is assembled in (task 270).
	headFields Obj
	scratch    Obj
	// choice and choices hold the single choice of the chunk being built.
	choice  Obj
	choices [1]any
}

// NewStream creates the chunk writer.
func NewStream(h *Hooks, w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) *Stream {
	return &Stream{
		h: h, w: sse.New(w), rw: w, req: req, rl: rl, buf: make([]byte, 0, 1024),
		inline: h.ReasoningField == "inline_tag", hidden: h.hideReasoning(req),
		usageNull: h.ChunkUsageNull && req.IncludeUsage,
	}
}

// head is the start of every chunk: id, object, created, model,
// service_tier, system_fingerprint.
func (s *Stream) head() Obj {
	if s.headFields == nil {
		s.headFields = s.buildHead()
		s.scratch = make(Obj, 0, len(s.headFields)+8)
	}
	// Each chunk is rendered and written before the next one is built, and
	// nothing keeps the Obj, so they all share scratch.
	s.scratch = append(s.scratch[:0], s.headFields...)
	return s.scratch
}

// buildHead is the head of every chunk of this stream.
func (s *Stream) buildHead() Obj {
	object := "chat.completion.chunk"
	if s.req.Endpoint == core.EndpointCompletions {
		object = "text_completion"
	}
	o := make(Obj, 0, 10)
	o = append(o, Field{"id", s.resp.ID}, Field{"object", object}, Field{"created", s.resp.Created.Unix()}, Field{"model", s.resp.Model})
	if s.h.ServiceTier != "" && s.req.Endpoint != core.EndpointCompletions {
		o = append(o, Field{"service_tier", s.h.ServiceTier})
	}
	if s.h.SystemFingerprint != "" {
		o = append(o, Field{"system_fingerprint", s.h.SystemFingerprint})
	}
	return o
}

// send writes one chunk: the dialect tail (usage, obfuscation), the top
// extras and the gateway's shape.
func (s *Stream) send(o Obj, usage any) error {
	b, err := s.render(o, usage)
	if err != nil {
		return err
	}
	return s.w.DataRaw(b)
}

// render serializes one chunk into s.buf.
func (s *Stream) render(o Obj, usage any) ([]byte, error) {
	if usage != nil {
		o = append(o, Field{"usage", usage})
	} else if s.usageNull {
		o = append(o, Field{"usage", nil})
	}
	if s.obf != nil {
		v := s.obfFixed
		if v == "" {
			v = s.obf.next()
		}
		o = append(o, Field{"obfuscation", v})
	}
	o = s.h.shape(PartChunk, s.h.withTop(s.req, s.resp, o))
	b, err := s.h.ChunkFormat.Append(s.buf[:0], o)
	if err != nil {
		return nil, err
	}
	s.buf = b
	return b, nil
}

func (s *Stream) chunk(delta Obj, finish any) error { return s.chunkWith(delta, finish, nil) }

// chunkWith writes one chunk of a single choice; usage, when not nil,
// rides on it (FinishUsage).
func (s *Stream) chunkWith(delta Obj, finish, usage any) error {
	return s.send(s.chunkObj(delta, finish), usage)
}

// chunkObj is the chunk object of one delta, before send's tail.
func (s *Stream) chunkObj(delta Obj, finish any) Obj {
	var choice Obj
	if s.req.Endpoint == core.EndpointCompletions {
		text, _ := delta.Get("content")
		if text == nil {
			text = ""
		}
		choice = append(s.choice[:0], Field{"text", text}, Field{"index", 0}, Field{"logprobs", nil}, Field{"finish_reason", finish})
	} else {
		choice = append(s.choice[:0], Field{"index", 0}, Field{"delta", s.h.shape(PartDelta, delta)}, Field{"logprobs", nil}, Field{"finish_reason", finish})
	}
	switch {
	case s.h.NativeFinishReason && finish != nil:
		choice = append(choice, Field{"native_finish_reason", s.h.native(s.req, s.reason)})
	case s.h.NativeFinishReason && s.h.NativeFinish != nil:
		choice = append(choice, Field{"native_finish_reason", nil})
	}
	s.choice = choice
	head := s.head()
	s.choices[0] = s.h.shape(PartChunkChoice, choice)
	return append(head, Field{"choices", s.choices[:]})
}

func (s *Stream) Start(resp *core.Response) error {
	s.resp = resp
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
	s.started = true
	if s.h.KeepAlive != "" {
		if err := s.w.Comment(s.h.KeepAlive); err != nil {
			return err
		}
	}
	if s.h.NoRoleChunk {
		return nil
	}
	if s.hidden && resp.Reasoning != "" {
		s.roleDue = true
		return nil
	}
	return s.roleChunk()
}

func (s *Stream) roleChunk() error {
	return s.chunk(Obj{{"role", "assistant"}, {"content", ""}, {"refusal", nil}}, nil)
}

// visible sends the deferred role chunk before the first visible frame.
func (s *Stream) visible() error {
	if !s.roleDue {
		return nil
	}
	s.roleDue = false
	return s.roleChunk()
}

func (s *Stream) ReasoningDelta(text string) error {
	if s.hidden {
		return nil
	}
	if s.inline {
		if !s.inlineOpen {
			s.inlineOpen = true
			text = "<thinking>" + text
		}
		return s.chunk(Obj{{"content", text}}, nil)
	}
	field := "reasoning_content"
	if s.h.ReasoningField == "reasoning" {
		field = "reasoning"
	}
	delta := Obj{{field, text}}
	if s.h.ReasoningDetails && field == "reasoning" {
		delta = append(delta, Field{"reasoning_details", reasoningDetails(text)})
	}
	return s.chunk(delta, nil)
}

func (s *Stream) closeInline() error {
	if s.inline && s.inlineOpen {
		s.inlineOpen = false
		return s.chunk(Obj{{"content", "</thinking>"}}, nil)
	}
	return nil
}

func (s *Stream) TextDelta(text string) error {
	if err := s.visible(); err != nil {
		return err
	}
	if err := s.closeInline(); err != nil {
		return err
	}
	return s.contentChunk(text)
}

func (s *Stream) ToolCallStart(index int, id, name string) error {
	if err := s.visible(); err != nil {
		return err
	}
	if err := s.closeInline(); err != nil {
		return err
	}
	return s.chunk(Obj{{"tool_calls", []any{O("index", index, "id", id, "type", "function", "function", O("name", name, "arguments", ""))}}}, nil)
}

func (s *Stream) ToolArgsDelta(index int, text string) error {
	return s.chunk(Obj{{"tool_calls", []any{O("index", index, "function", O("arguments", text))}}}, nil)
}

func (s *Stream) ToolCallEnd(int) error { return nil }

func (s *Stream) Ping() error { return s.w.Comment("ping") }

func (s *Stream) Finish(reason core.FinishReason, usage core.Usage, includeUsage bool) error {
	if err := s.visible(); err != nil {
		return err
	}
	if err := s.closeInline(); err != nil {
		return err
	}
	fr := finishReason(reason)
	s.reason = reason
	if s.h.FinishUsage {
		if err := s.chunkWith(Obj{}, fr, UsageJSON(s.h, s.req, s.resp, usage)); err != nil {
			return err
		}
		s.closed = true
		return s.w.Done()
	}
	if err := s.chunk(Obj{}, fr); err != nil {
		return err
	}
	if includeUsage {
		choices := []any{}
		if s.h.FinalChunkWithChoice {
			native := s.h.native(s.req, reason)
			if s.req.Endpoint == core.EndpointCompletions {
				choices = []any{s.h.shape(PartChunkChoice, O("index", 0, "text", "", "logprobs", nil, "finish_reason", fr, "native_finish_reason", native))}
			} else {
				choices = []any{s.h.shape(PartChunkChoice, O("index", 0, "delta", s.h.shape(PartDelta, Obj{}), "finish_reason", fr, "native_finish_reason", native))}
			}
		}
		o := append(s.head(), Field{"choices", choices})
		if err := s.send(o, UsageJSON(s.h, s.req, s.resp, usage)); err != nil {
			return err
		}
	}
	s.closed = true
	return s.w.Done()
}

func (s *Stream) Error(e *core.Error) error {
	if err := s.visible(); err != nil {
		return err
	}
	body := s.h.StreamErrorBody(e)
	var err error
	if raw, ok := body.(json.RawMessage); ok {
		err = s.w.DataRaw(raw) // already compact, as the dialect writes it
	} else {
		err = s.w.Data(body)
	}
	if err != nil {
		return err
	}
	s.closed = true
	return s.w.Done()
}

func (s *Stream) Raw(p []byte) error { return s.w.Raw(p) }
func (s *Stream) LastFrame() []byte  { return s.w.Last() }
func (s *Stream) Close() error       { return nil }
