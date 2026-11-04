package loadgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// sseParser splits a Server-Sent Events byte stream into events. It is fed
// arbitrary chunks and keeps its buffers between events, so a line costs no
// allocation once the buffers have grown.
type sseParser struct {
	line    []byte // partial line carried over between chunks
	event   []byte
	data    []byte
	hasData bool
	// onEvent receives every complete event; the slices are only valid
	// during the call. An error stops the parser.
	onEvent func(event, data []byte) error
}

// Feed consumes a chunk.
func (p *sseParser) Feed(b []byte) error {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.line = append(p.line, b...)
			return nil
		}
		ln := b[:i]
		if len(p.line) > 0 {
			p.line = append(p.line, ln...)
			ln = p.line
		}
		b = b[i+1:]
		err := p.processLine(ln)
		p.line = p.line[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

// Close processes a last line without a newline and a pending event.
func (p *sseParser) Close() error {
	if len(p.line) > 0 {
		err := p.processLine(p.line)
		p.line = p.line[:0]
		if err != nil {
			return err
		}
	}
	return p.dispatch()
}

func (p *sseParser) processLine(ln []byte) error {
	ln = bytes.TrimSuffix(ln, []byte{'\r'})
	if len(ln) == 0 {
		return p.dispatch()
	}
	if ln[0] == ':' { // comment / keep-alive
		return nil
	}
	field, value, _ := bytes.Cut(ln, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) { // no allocation: compiler-optimized comparison
	case "event":
		p.event = append(p.event[:0], value...)
	case "data":
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	}
	return nil
}

func (p *sseParser) dispatch() error {
	if !p.hasData {
		p.event = p.event[:0]
		return nil
	}
	err := p.onEvent(p.event, p.data)
	p.event, p.data, p.hasData = p.event[:0], p.data[:0], false
	return err
}

// errParse is a 2xx body that is not valid SSE/JSON of the protocol.
var errParse = errors.New("invalid response body")

// streamErr is an error event in the middle of a 200 stream.
type streamErr struct{ typ string }

func (e *streamErr) Error() string { return StreamError(e.typ) }

// errType of an error object: type, or code (OpenRouter).
type errObj struct {
	Type json.RawMessage `json:"type"`
	Code json.RawMessage `json:"code"`
}

func (e *errObj) kind() string {
	if t := rawString(e.Type); t != "" {
		return t
	}
	return rawString(e.Code)
}

// ---- OpenAI ----

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type oaToolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaDelta struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"`
	Reasoning        string        `json:"reasoning"`
	ToolCalls        []oaToolDelta `json:"tool_calls"`
}

type oaChoice struct {
	Delta        oaDelta `json:"delta"`
	Message      oaDelta `json:"message"` // non-stream
	FinishReason string  `json:"finish_reason"`
}

type oaChunk struct {
	Choices []oaChoice `json:"choices"`
	Usage   *oaUsage   `json:"usage"`
	Error   *errObj    `json:"error"`
}

// recycle empties c for the next json.Unmarshal while keeping the choices'
// backing array: the elements are zeroed (Unmarshal does not clear fields
// absent from the next event) and the pointers dropped.
func (c *oaChunk) recycle() *oaChunk {
	all := c.Choices[:cap(c.Choices)]
	clear(all)
	*c = oaChunk{Choices: all[:0]}
	return c
}

// ---- Anthropic ----

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *anUsage) input() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type anContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type anEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage anUsage `json:"usage"`
	} `json:"message"`
	Index        int        `json:"index"`
	ContentBlock *anContent `json:"content_block"`
	// Delta is a value, not a pointer: the hot events (content_block_delta)
	// then decode without allocating. An absent delta is the zero value, which
	// every consumer already treats as "nothing to do".
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anUsage `json:"usage"`
	Error *errObj  `json:"error"`
}

// anResponse is a non-stream Anthropic message.
type anResponse struct {
	Type       string      `json:"type"`
	Content    []anContent `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      *anUsage    `json:"usage"`
}

// decoder turns events (stream) or a whole body (non-stream) into the
// fields of a Result.
type decoder struct {
	proto   Protocol
	res     *Result
	capture bool
	text    strings.Builder
	reason  strings.Builder
	sig     strings.Builder
	args    []strings.Builder // tool call arguments by position
	// Anthropic: block index → tool call position (-1 for non-tool blocks).
	blockTool []int
	hasUsage  bool
	done      bool
	// content is set by handle when the event carried content (TTFT/ITL).
	content bool
	// oa is reused by every OpenAI event (task 378): decoding into a fresh
	// oaChunk allocated the choices array per chunk (~200 B), and the GC of
	// that garbage delayed the reading goroutines enough to show in the ITL
	// p99 at 9 000 streams.
	oa oaChunk
	// an is reused by every Anthropic event, like oa (task 400).
	an anEvent
}

func (d *decoder) reset(proto Protocol, res *Result, capture bool) {
	d.proto, d.res, d.capture = proto, res, capture
	d.text.Reset()
	d.reason.Reset()
	d.sig.Reset()
	d.args = d.args[:0]
	d.blockTool = d.blockTool[:0]
	d.hasUsage, d.done, d.content = false, false, false
}

// toolAt returns the position of tool call i, growing the slices.
func (d *decoder) toolAt(i int) int {
	for len(d.res.ToolCalls) <= i {
		d.res.ToolCalls = append(d.res.ToolCalls, ToolCall{})
		d.args = append(d.args, strings.Builder{})
	}
	return i
}

// event handles one SSE event and sets d.content.
func (d *decoder) event(name, data []byte) error {
	d.content = false
	if d.done {
		return nil
	}
	if d.proto == Anthropic {
		return d.anthropicEvent(name, data)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		d.done = true
		return nil
	}
	c := d.oa.recycle()
	if json.Unmarshal(data, c) != nil {
		return errParse
	}
	if c.Error != nil {
		return &streamErr{typ: c.Error.kind()}
	}
	d.openAIChoices(c, false)
	return nil
}

func (d *decoder) openAIChoices(c *oaChunk, whole bool) {
	if c.Usage != nil {
		d.hasUsage = true
		d.res.InputTokens = c.Usage.PromptTokens
		d.res.OutputTokens = c.Usage.CompletionTokens
		if c.Usage.Details != nil {
			d.res.ReasoningTokens = c.Usage.Details.ReasoningTokens
		}
	}
	for _, ch := range c.Choices {
		delta := &ch.Delta
		if whole {
			delta = &ch.Message
		}
		if ch.FinishReason != "" {
			d.res.FinishReason = ch.FinishReason
		}
		reasoning := delta.ReasoningContent + delta.Reasoning
		if delta.Content != "" || reasoning != "" || len(delta.ToolCalls) > 0 {
			d.content = true
		}
		if d.capture {
			d.text.WriteString(delta.Content)
			d.reason.WriteString(reasoning)
		}
		for k, tc := range delta.ToolCalls {
			i := k
			if tc.Index != nil {
				i = *tc.Index
			}
			i = d.toolAt(i)
			if tc.ID != "" {
				d.res.ToolCalls[i].ID = tc.ID
			}
			if tc.Function.Name != "" {
				d.res.ToolCalls[i].Name = tc.Function.Name
			}
			_, _ = d.args[i].WriteString(tc.Function.Arguments)
		}
	}
}

func (d *decoder) anthropicEvent(name, data []byte) error {
	// Unmarshal leaves the fields absent from the next event untouched, so the
	// reused event is zeroed first.
	ev := &d.an
	*ev = anEvent{}
	if json.Unmarshal(data, ev) != nil {
		return errParse
	}
	if ev.Type == "" {
		ev.Type = string(name)
	}
	switch ev.Type {
	case "error":
		var typ string
		if ev.Error != nil {
			typ = ev.Error.kind()
		}
		return &streamErr{typ: typ}
	case "message_start":
		if ev.Message != nil {
			d.res.InputTokens = ev.Message.Usage.input()
			d.res.OutputTokens = ev.Message.Usage.OutputTokens
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			d.blockStart(ev.Index, ev.ContentBlock)
		}
	case "content_block_delta":
		d.blockDelta(ev.Index, ev.Delta.Type, ev.Delta.Text, ev.Delta.Thinking, ev.Delta.PartialJSON, ev.Delta.Signature)
	case "message_delta":
		if ev.Delta.StopReason != "" {
			d.res.FinishReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			d.hasUsage = true
			d.res.OutputTokens = ev.Usage.OutputTokens
			if in := ev.Usage.input(); in > 0 {
				d.res.InputTokens = in
			}
		}
	case "message_stop":
		d.done = true
	}
	return nil
}

func (d *decoder) blockStart(idx int, b *anContent) {
	for len(d.blockTool) <= idx {
		d.blockTool = append(d.blockTool, -1)
	}
	switch b.Type {
	case "tool_use":
		d.content = true
		i := d.toolAt(len(d.res.ToolCalls))
		d.blockTool[idx] = i
		d.res.ToolCalls[i].ID, d.res.ToolCalls[i].Name = b.ID, b.Name
		if in := bytes.TrimSpace(b.Input); len(in) > 0 && !bytes.Equal(in, []byte("{}")) {
			_, _ = d.args[i].Write(in)
		}
	case "text":
		d.blockDelta(idx, "text_delta", b.Text, "", "", "")
	case "thinking":
		d.blockDelta(idx, "thinking_delta", "", b.Thinking, "", b.Signature)
	case "redacted_thinking":
		d.res.ReasoningRedacted = true
	}
}

func (d *decoder) blockDelta(idx int, typ, text, thinking, partial, sig string) {
	switch typ {
	case "text_delta":
		d.content = d.content || text != ""
		if d.capture {
			d.text.WriteString(text)
		}
	case "thinking_delta":
		d.content = d.content || thinking != ""
		if d.capture {
			d.reason.WriteString(thinking)
		}
	case "input_json_delta":
		d.content = true
		if idx < len(d.blockTool) && d.blockTool[idx] >= 0 {
			_, _ = d.args[d.blockTool[idx]].WriteString(partial)
		}
	}
	if d.capture {
		d.sig.WriteString(sig)
	}
}

// whole parses a non-stream 2xx JSON body.
func (d *decoder) whole(body []byte) error {
	if d.proto == Anthropic {
		var r anResponse
		if json.Unmarshal(body, &r) != nil || r.Type != "message" {
			return errParse
		}
		if r.Usage != nil {
			d.hasUsage = true
			d.res.InputTokens, d.res.OutputTokens = r.Usage.input(), r.Usage.OutputTokens
		}
		d.res.FinishReason = r.StopReason
		for i := range r.Content {
			d.blockStart(i, &r.Content[i])
		}
	} else {
		var c oaChunk
		if json.Unmarshal(body, &c) != nil || c.Choices == nil {
			return errParse
		}
		d.openAIChoices(&c, true)
	}
	d.done = true
	return nil
}

// finish copies the accumulated tool arguments and message into the result.
func (d *decoder) finish() {
	for i := range d.res.ToolCalls {
		d.res.ToolCalls[i].Arguments = d.args[i].String()
	}
	if !d.hasUsage {
		d.res.InputTokens, d.res.ReasoningTokens = 0, 0
		d.res.OutputTokens = d.res.ContentEvents
		d.res.TokensEstimated = true
	}
	if d.capture {
		d.res.Message = &Message{
			Role: "assistant", Content: d.text.String(), Reasoning: d.reason.String(),
			Signature: d.sig.String(), ToolCalls: d.res.ToolCalls,
		}
	}
}
