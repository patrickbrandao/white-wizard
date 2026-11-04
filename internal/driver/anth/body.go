package anth

import (
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// Values Anthropic reports in usage, observed on 2026-10-31 (ADR 0049).
const (
	inferenceGeo = "not_available"
	speed        = "standard"
)

// UsageJSON builds the usage object of a message (body and message_start),
// in the real order: input_tokens, cache_creation_input_tokens,
// cache_read_input_tokens, cache_creation{ephemeral_5m_input_tokens,
// ephemeral_1h_input_tokens}, output_tokens, output_tokens_details (only
// with thinking tokens), service_tier, inference_geo, speed. Without API
// the Anthropic-only fields stay out.
func UsageJSON(h *Hooks, req *core.Request, resp *core.Response, u core.Usage) oai.Obj {
	o := oai.O("input_tokens", InputTokens(u), "cache_creation_input_tokens", u.CacheWriteTokens, "cache_read_input_tokens", u.CachedTokens)
	if h.API {
		w1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
		o = append(o, oai.Field{Key: "cache_creation", Val: oai.O("ephemeral_5m_input_tokens", u.CacheWriteTokens-w1h, "ephemeral_1h_input_tokens", w1h)})
	}
	o = append(o, oai.Field{Key: "output_tokens", Val: u.CompletionTokens})
	if h.API && u.ReasoningTokens > 0 {
		o = append(o, oai.Field{Key: "output_tokens_details", Val: oai.O("thinking_tokens", u.ReasoningTokens)})
	}
	o = append(o, oai.Field{Key: "service_tier", Val: h.ServiceTier})
	if h.API {
		o = append(o, oai.Field{Key: "inference_geo", Val: inferenceGeo}, oai.Field{Key: "speed", Val: speed})
	}
	if h.UsageExtras != nil {
		o.Merge(h.UsageExtras(req, resp, u))
	}
	return h.shape(PartUsage, o)
}

// deltaUsage is the usage of message_delta. Anthropic sends the four
// counters (and output_tokens_details with thinking), without
// cache_creation, service_tier, inference_geo or speed; the gateways keep
// the message usage.
func deltaUsage(h *Hooks, req *core.Request, resp *core.Response, u core.Usage) oai.Obj {
	if !h.API {
		return h.shape(PartDeltaUsage, UsageJSON(h, req, resp, u))
	}
	o := oai.O("input_tokens", InputTokens(u), "cache_creation_input_tokens", u.CacheWriteTokens, "cache_read_input_tokens", u.CachedTokens, "output_tokens", u.CompletionTokens)
	if u.ReasoningTokens > 0 {
		o = append(o, oai.Field{Key: "output_tokens_details", Val: oai.O("thinking_tokens", u.ReasoningTokens)})
	}
	return h.shape(PartDeltaUsage, o)
}

// signature is the signature of a thinking block: with API a base64 blob
// of the real length (observed 476 characters), stable per response.
func (h *Hooks) signature(resp *core.Response) string {
	if !h.API || h.Signature == "" {
		return h.Signature
	}
	id := ""
	if resp != nil {
		id = resp.ID
	}
	b := derive(id, "signature", 357)
	b[0], b[1] = 0x12, 0xE9 // the real blobs start with "Euk"/"Euo"
	return base64.StdEncoding.EncodeToString(b)
}

func (h *Hooks) toolUseBlock(id, name string, input any) oai.Obj {
	o := oai.O("type", "tool_use", "id", id, "name", name, "input", input)
	if h.API {
		o = append(o, oai.Field{Key: "caller", Val: oai.O("type", "direct")})
	}
	return h.shape(PartBlock, o)
}

func contentBlocks(h *Hooks, resp *core.Response) []any {
	blocks := []any{}
	if resp.Reasoning != "" {
		b := oai.O("type", "thinking", "thinking", resp.Reasoning)
		if sig := h.signature(resp); sig != "" {
			b = append(b, oai.Field{Key: "signature", Val: sig})
		}
		blocks = append(blocks, h.shape(PartBlock, b))
	}
	if resp.Content != "" {
		blocks = append(blocks, h.shape(PartBlock, oai.O("type", "text", "text", resp.Content)))
	}
	for _, tc := range resp.ToolCalls {
		var input any
		if json.Valid([]byte(tc.Arguments)) {
			input = json.RawMessage(tc.Arguments)
		} else {
			input = oai.O("_raw", tc.Arguments)
		}
		blocks = append(blocks, h.toolUseBlock(tc.ID, tc.Name, input))
	}
	return blocks
}

// message is the message object in the real order: model, id, type, role,
// content, container, stop_reason, stop_sequence, stop_details, usage,
// diagnostics (container, stop_details and diagnostics only with API).
func (h *Hooks) message(resp *core.Response, content []any, stop any, usage oai.Obj) oai.Obj {
	o := oai.O("model", resp.Model, "id", resp.ID, "type", "message", "role", "assistant", "content", content)
	if h.API {
		o = append(o, oai.Field{Key: "container", Val: nil})
	}
	o = append(o, oai.Field{Key: "stop_reason", Val: stop}, oai.Field{Key: "stop_sequence", Val: nil})
	if h.API {
		o = append(o, oai.Field{Key: "stop_details", Val: nil})
	}
	o = append(o, oai.Field{Key: "usage", Val: usage})
	if h.API {
		o = append(o, oai.Field{Key: "diagnostics", Val: nil})
	}
	return h.shape(PartMessage, o)
}

// successHeaders writes the dialect headers of a 200.
func (h *Hooks) successHeaders(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) {
	if name := h.requestIDHeader(); name != "" {
		w.Header(name, reqID(req))
	}
	var m map[string]string
	switch {
	case h.SuccessHeaders != nil:
		m = h.SuccessHeaders(req, resp, rl)
	case rl != nil && rl.Present && h.RateLimitHeaders != nil:
		m = h.RateLimitHeaders(rl)
	}
	for k, v := range m {
		w.Header(k, v)
	}
	for k, v := range resp.Headers {
		w.Header(k, v)
	}
}

// WriteMessage writes a non-streaming message.
func WriteMessage(h *Hooks, w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	body := h.message(resp, contentBlocks(h, resp), stopReason(resp.FinishReason), UsageJSON(h, req, resp, resp.Usage))
	b, err := jsonFormat.Marshal(body)
	if err != nil {
		return err
	}
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	h.successHeaders(w, req, resp, rl)
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// WriteCountTokens writes the count_tokens answer, {"input_tokens": n},
// with the account headers and request-id (no anthropic-ratelimit-*).
func WriteCountTokens(h *Hooks, w core.ResponseWriter, req *core.Request, resp *core.Response) error {
	b, _ := jsonFormat.Marshal(oai.O("input_tokens", resp.Usage.PromptTokens))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	if name := h.requestIDHeader(); name != "" {
		w.Header(name, reqID(req))
	}
	if h.AccountHeaders != nil {
		for k, v := range h.AccountHeaders(req) {
			w.Header(k, v)
		}
	}
	for k, v := range resp.Headers {
		w.Header(k, v)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// --- streaming ---

// pingData is the ping event as the real sends it: with a space after the
// colon and no padding.
const pingData = `{"type": "ping"}`

// Stream is the Anthropic event StreamWriter.
type Stream struct {
	h       *Hooks
	w       *sse.Writer
	rw      core.ResponseWriter
	req     *core.Request
	resp    *core.Response
	rl      *core.RateLimitState
	index   int    // current block index
	open    string // type of the open block: "", thinking, text, tool_use
	toolIdx map[int]int
	pinged  bool
	pad     *rand.Rand // SSE padding (API)
	// deltas holds the pre-rendered content_block_delta of each delta type
	// (text, thinking, input_json) for the block being written.
	deltas [3]deltaTpl
	data   []byte // the data of the delta being written
	fbuf   []byte // the frame being written
}

// NewStream creates the event writer.
func NewStream(h *Hooks, w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) *Stream {
	return &Stream{h: h, w: sse.New(w), rw: w, req: req, rl: rl, index: -1, toolIdx: map[int]int{}}
}

// event writes one frame. With API the data carries 0 to 15 spaces before
// its closing brace, as the real does (observed uniformly 0–15),
// reproducible from the response id.
func (s *Stream) event(name string, o oai.Obj) error {
	b, err := jsonFormat.Marshal(s.h.shape(PartEvent, o))
	if err != nil {
		return err
	}
	return s.frame(name, b)
}

func (s *Stream) frame(name string, data []byte) error {
	pad := 0
	if s.h.API && s.pad != nil && len(data) > 0 && data[len(data)-1] == '}' {
		pad = s.pad.IntN(16)
	}
	f := append(s.fbuf[:0], "event: "...)
	f = append(f, name...)
	f = append(f, "\ndata: "...)
	if pad > 0 {
		f = append(f, data[:len(data)-1]...)
		for range pad {
			f = append(f, ' ')
		}
		f = append(f, '}')
	} else {
		f = append(f, data...)
	}
	f = append(f, '\n', '\n')
	s.fbuf = f
	return s.w.Raw(f)
}

func (s *Stream) Start(resp *core.Response) error {
	s.resp = resp
	if s.h.API {
		hs := fnv.New64a()
		_, _ = hs.Write([]byte(resp.ID))
		s.pad = rand.New(rand.NewPCG(hs.Sum64(), 0x2545f4914f6cdd1d))
	}
	s.rw.Status(200)
	s.rw.Header("Content-Type", "text/event-stream; charset=utf-8")
	s.rw.Header("Cache-Control", "no-cache")
	s.rw.Header("Connection", "keep-alive")
	s.rw.Header("X-Accel-Buffering", "no")
	s.h.successHeaders(s.rw, s.req, resp, s.rl)
	usage := UsageJSON(s.h, s.req, resp, core.Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: 1, CachedTokens: resp.Usage.CachedTokens, CacheWriteTokens: resp.Usage.CacheWriteTokens, CacheWrite1hTokens: resp.Usage.CacheWrite1hTokens})
	msg := s.h.message(resp, []any{}, nil, usage)
	return s.event("message_start", oai.O("type", "message_start", "message", msg))
}

func (s *Stream) closeBlock() error {
	if s.open == "" {
		return nil
	}
	if s.open == "thinking" {
		if sig := s.h.signature(s.resp); sig != "" {
			if err := s.event("content_block_delta", oai.O("type", "content_block_delta", "index", s.index, "delta", oai.O("type", "signature_delta", "signature", sig))); err != nil {
				return err
			}
		}
	}
	s.open = ""
	return s.event("content_block_stop", oai.O("type", "content_block_stop", "index", s.index))
}

// openBlock closes the open block and starts a new one; with API the first
// content_block_start is followed by a ping (observed).
func (s *Stream) openBlock(typ string, cb oai.Obj) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.index++
	s.open = typ
	if err := s.event("content_block_start", oai.O("type", "content_block_start", "index", s.index, "content_block", cb)); err != nil {
		return err
	}
	if s.h.API && !s.pinged {
		s.pinged = true
		return s.Ping()
	}
	return nil
}

func (s *Stream) ReasoningDelta(text string) error {
	if s.open != "thinking" {
		cb := oai.O("type", "thinking", "thinking", "")
		if s.h.API {
			cb = append(cb, oai.Field{Key: "signature", Val: ""})
		}
		if err := s.openBlock("thinking", s.h.shape(PartBlock, cb)); err != nil {
			return err
		}
	}
	return s.delta(deltaThinking, s.index, text)
}

func (s *Stream) TextDelta(text string) error {
	if s.open != "text" {
		if err := s.openBlock("text", s.h.shape(PartBlock, oai.O("type", "text", "text", ""))); err != nil {
			return err
		}
	}
	return s.delta(deltaText, s.index, text)
}

// ToolCallStart opens a tool_use block; with API its first delta is an
// empty partial_json, as observed.
func (s *Stream) ToolCallStart(index int, id, name string) error {
	if err := s.openBlock("tool_use", s.h.toolUseBlock(id, name, oai.Obj{})); err != nil {
		return err
	}
	s.toolIdx[index] = s.index
	if s.h.API {
		return s.ToolArgsDelta(index, "")
	}
	return nil
}

func (s *Stream) ToolArgsDelta(index int, text string) error {
	bi, ok := s.toolIdx[index]
	if !ok {
		bi = s.index
	}
	return s.delta(deltaInputJSON, bi, text)
}

func (s *Stream) ToolCallEnd(int) error { return s.closeBlock() }

func (s *Stream) Ping() error {
	if s.h.API {
		return s.w.Raw([]byte("event: ping\ndata: " + pingData + "\n\n"))
	}
	return s.event("ping", oai.O("type", "ping"))
}

func (s *Stream) Finish(reason core.FinishReason, usage core.Usage, _ bool) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	delta := oai.O("stop_reason", stopReason(reason), "stop_sequence", nil)
	if s.h.API {
		delta = append(delta, oai.Field{Key: "stop_details", Val: nil}, oai.Field{Key: "container", Val: nil})
	}
	if err := s.event("message_delta", oai.O("type", "message_delta", "delta", delta, "usage", deltaUsage(s.h, s.req, s.resp, usage))); err != nil {
		return err
	}
	return s.event("message_stop", oai.O("type", "message_stop"))
}

// Error writes the error event: the envelope without request_id.
func (s *Stream) Error(e *core.Error) error {
	_, body, _, _ := s.h.ErrorBody(s.req, e)
	m, ok := body.(map[string]any)
	if !ok {
		m = map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": e.Message}}
	}
	o := envelope(m)
	o.Delete("request_id")
	b, err := jsonFormat.Marshal(o)
	if err != nil {
		return err
	}
	return s.frame("error", b)
}

func (s *Stream) Raw(p []byte) error { return s.w.Raw(p) }
func (s *Stream) LastFrame() []byte  { return s.w.Last() }
func (s *Stream) Close() error       { return nil }
