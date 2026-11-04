package gemini

import (
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// streamGenerateContent (spec 005, spec 006 16.5), conferred on 2026-10-31
// (task 164): every chunk is a whole GenerateContentResponse with the
// running usageMetadata, modelVersion and the same responseId; no chunk
// opens the stream and only the last has finishReason. Thoughts (with
// includeThoughts) are parts {text, thought: true}; hidden thoughts send
// nothing and only grow thoughtsTokenCount. A function call arrives whole
// in one chunk with the signature. The last chunk is an empty text part,
// with the signature when no call carried it. With alt=sse each chunk is
// an SSE event; without it the chunks are the elements of a JSON array.

type stream struct {
	w    core.ResponseWriter
	req  *core.Request
	resp *core.Response
	sse  bool
	// count measures a delta in tokens (the simulated tokenizer).
	count func(string) int

	started, closed bool
	frames          int
	text, thoughts  int // tokens sent so far
	signed          bool
	calls           map[int]*core.ToolCall
	last            []byte
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, _ *core.RateLimitState) core.StreamWriter {
	return &stream{w: w, req: req, sse: sse(reqHeaders(req)), count: generate.Simulated{}.Count, calls: map[int]*core.ToolCall{}}
}

// Start sends the headers; the first chunk carries the first delta.
func (s *stream) Start(resp *core.Response) error {
	s.resp = resp
	s.started = true
	s.w.Status(200)
	if s.sse {
		s.w.Header("Content-Type", contentSSE)
		s.w.Header("Content-Disposition", "attachment")
	} else {
		s.w.Header("Content-Type", contentJSON)
	}
	commonHeaders(s.w, s.req)
	extraHeaders(s.w, resp)
	return nil
}

// running is the usageMetadata so far, never above the final counts. Once
// the answer starts the thinking is over, so the thoughts are all counted
// (observed: the first text chunk after the thoughts carries the final
// thoughtsTokenCount).
func (s *stream) running() oai.Obj {
	u := s.resp.Usage
	cand := min(s.text, u.CompletionTokens-u.ReasoningTokens)
	th := min(s.thoughts, u.ReasoningTokens)
	if s.text > 0 {
		th = u.ReasoningTokens
	}
	return usage(u.PromptTokens, cand, th, cachedTokens(s.req, u))
}

// chunk renders one GenerateContentResponse.
func (s *stream) chunk(ps []oai.Obj, finish string, u oai.Obj) oai.Obj {
	cand := oai.O("content", oai.O("parts", ps, "role", "model"))
	if finish != "" {
		cand = append(cand, oai.Field{Key: "finishReason", Val: finish})
	}
	cand = append(cand, oai.Field{Key: "index", Val: 0})
	return oai.O("candidates", []any{cand}, "usageMetadata", u, "modelVersion", s.resp.Model, "responseId", s.resp.ID)
}

// frame writes one chunk in the stream's framing.
func (s *stream) frame(v oai.Obj) error {
	var b []byte
	if s.sse {
		b = sseEvent(v)
	} else {
		sep := ",\r\n"
		if s.frames == 0 {
			sep = "["
		}
		b = append([]byte(sep), pretty(v)...)
	}
	s.frames++
	s.last = b
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *stream) ReasoningDelta(text string) error {
	s.thoughts += s.count(text)
	if s.req != nil && s.req.Reasoning.Exclude {
		return nil
	}
	return s.frame(s.chunk([]oai.Obj{oai.O("text", text, "thought", true)}, "", s.running()))
}

func (s *stream) TextDelta(text string) error {
	s.text += s.count(text)
	return s.frame(s.chunk([]oai.Obj{oai.O("text", text)}, "", s.running()))
}

func (s *stream) ToolCallStart(index int, id, name string) error {
	s.calls[index] = &core.ToolCall{ID: id, Name: name}
	return nil
}

func (s *stream) ToolArgsDelta(index int, text string) error {
	if c := s.calls[index]; c != nil {
		c.Arguments += text
	}
	return nil
}

// ToolCallEnd sends the whole call; the first one carries the signature.
func (s *stream) ToolCallEnd(index int) error {
	c := s.calls[index]
	if c == nil {
		return nil
	}
	s.text += s.count(c.Name) + s.count(c.Arguments)
	p := callPart(*c)
	if !s.signed {
		s.signed = true
		p = append(p, oai.Field{Key: "thoughtSignature", Val: signature(s.resp.ID, s.resp.Usage.ReasoningTokens)})
	}
	return s.frame(s.chunk([]oai.Obj{p}, "", s.running()))
}

// Ping: the Gemini stream has no keep-alive.
func (s *stream) Ping() error { return nil }

// Finish sends the closing chunk: an empty text part (signed unless a call
// was), the finish reason and the final usage.
func (s *stream) Finish(reason core.FinishReason, u core.Usage, _ bool) error {
	p := oai.O("text", "")
	if !s.signed {
		s.signed = true
		p = append(p, oai.Field{Key: "thoughtSignature", Val: signature(s.resp.ID, u.ReasoningTokens)})
	}
	return s.frame(s.chunk([]oai.Obj{p}, finishReason(reason), finalUsage(s.req, u)))
}

// Error after the 200: a chunk holding the google.rpc.Status (the SDKs look
// for "error" in a chunk; não conferido).
func (s *stream) Error(e *core.Error) error {
	return s.frame(render(s.req, e).obj())
}

func (s *stream) Raw(p []byte) error {
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *stream) LastFrame() []byte { return s.last }

// Close ends the JSON array; an SSE stream just ends.
func (s *stream) Close() error {
	if s.closed || s.sse {
		s.closed = true
		return nil
	}
	s.closed = true
	b := []byte("]")
	if s.frames == 0 {
		b = []byte("[]")
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	return s.w.Flush()
}
