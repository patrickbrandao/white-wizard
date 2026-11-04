package client

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// errParse marks a response the client cannot read: a malformed status
// line, header or chunk, an unsupported Content-Encoding (br and zstd,
// which the wwz never sends) or a body that is not the expected JSON.
var errParse = errors.New("invalid response")

func parseErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errParse, fmt.Sprintf(format, a...))
}

// maxBody caps a non-stream or error body (a variable for tests).
var maxBody int64 = 8 << 20

// Response is the head of an HTTP/1.1 response and its body, without the
// transfer framing and the content encoding.
type Response struct {
	Status  int
	Headers []claudecode.Header // in order, names as received
	Body    io.Reader           // decoded body

	raw       io.Reader // framed body, before decoding
	keepAlive bool      // the connection can carry another request once raw is drained
}

// Get returns the first header named name, ignoring case.
func (r *Response) Get(name string) (string, bool) {
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// readLine reads one line and strips its CRLF (or LF).
func readLine(br *bufio.Reader) (string, error) {
	b, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", parseErr("line longer than %d bytes", br.Size())
	}
	if err != nil {
		return "", unexpected(err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

// unexpected turns io.EOF in the middle of a message into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// readResponse reads a response head from br and frames its body. head is
// true for the response to a HEAD request (no body).
func readResponse(br *bufio.Reader, head bool) (*Response, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	proto, rest, _ := strings.Cut(line, " ")
	code, _, _ := strings.Cut(rest, " ")
	status, cerr := strconv.Atoi(code)
	if !strings.HasPrefix(proto, "HTTP/1.") || len(code) != 3 || cerr != nil {
		return nil, parseErr("status line %q", line)
	}
	r := &Response{Status: status, keepAlive: proto == "HTTP/1.1"}
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.TrimRight(name, " \t") != name {
			return nil, parseErr("header line %q", line)
		}
		r.Headers = append(r.Headers, claudecode.Header{Name: name, Value: strings.Trim(value, " \t")})
	}
	if v, ok := r.Get("Connection"); ok {
		r.keepAlive = !strings.EqualFold(v, "close")
	}
	te, _ := r.Get("Transfer-Encoding")
	cl, hasCL := r.Get("Content-Length")
	switch {
	case head || status == 204 || status == 304:
		r.raw = bytes.NewReader(nil)
	case strings.Contains(strings.ToLower(te), "chunked"):
		r.raw = &chunkedReader{br: br}
	case hasCL:
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 {
			return nil, parseErr("Content-Length %q", cl)
		}
		r.raw = &lengthReader{r: br, n: n}
	default:
		r.raw, r.keepAlive = br, false
	}
	r.Body = r.raw
	enc, _ := r.Get("Content-Encoding")
	switch strings.ToLower(enc) {
	case "", "identity":
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(r.raw)
		if err != nil {
			return nil, parseErr("gzip body: %v", err)
		}
		r.Body = gz
	case "deflate":
		r.Body = deflateReader(r.raw)
	default:
		return nil, parseErr("unsupported Content-Encoding %q", enc)
	}
	return r, nil
}

// deflateReader reads an HTTP "deflate" body: zlib-wrapped, as the RFC
// says, or raw deflate, as some servers send (like Node and browsers).
func deflateReader(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if h, err := br.Peek(2); err == nil && h[0]&0x0f == 8 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
		zr, err := zlib.NewReader(br)
		if err == nil {
			return zr
		}
	}
	return flate.NewReader(br)
}

// drain reads what is left of the framed body, so the connection can be
// reused; it reports whether it can.
func (r *Response) drain() bool {
	if !r.keepAlive {
		return false
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r.raw, maxBody+1))
	return err == nil && n <= maxBody
}

// lengthReader reads a body of Content-Length n.
type lengthReader struct {
	r io.Reader
	n int64
}

func (l *lengthReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, unexpected(err)
}

// chunkedReader reads a chunked body and its trailer section.
type chunkedReader struct {
	br        *bufio.Reader
	n         int64 // bytes left in the current chunk
	afterData bool  // the CRLF after the chunk data is pending
	err       error
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	for c.n == 0 && c.err == nil {
		c.err = c.next()
	}
	if c.err != nil {
		return 0, c.err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.br.Read(p)
	c.n -= int64(n)
	if err != nil {
		c.err = unexpected(err)
	}
	return n, c.err
}

// next reads the next chunk size (and, at the last chunk, the trailers).
func (c *chunkedReader) next() error {
	if c.afterData {
		line, err := readLine(c.br)
		if err != nil {
			return err
		}
		if line != "" {
			return parseErr("chunk data longer than its size")
		}
	}
	line, err := readLine(c.br)
	if err != nil {
		return err
	}
	size, _, _ := strings.Cut(line, ";")
	n, perr := strconv.ParseUint(strings.TrimSpace(size), 16, 62)
	if perr != nil {
		return parseErr("chunk size %q", line)
	}
	if n == 0 {
		for {
			line, err := readLine(c.br)
			if err != nil {
				return err
			}
			if line == "" {
				return io.EOF
			}
		}
	}
	c.n, c.afterData = int64(n), true
	return nil
}

// readSSE reads an event stream and calls fn with each event (name and
// data, the data lines joined by "\n"). It returns io.EOF at the clean end
// of the body; an event cut by the end is dropped, as the SSE spec says.
func readSSE(r io.Reader, fn func(event string, data []byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data []byte
	has := false
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return err
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		switch {
		case len(line) == 0:
			if has {
				if err := fn(event, data); err != nil {
					return err
				}
			}
			event, data, has = "", data[:0], false
		case line[0] == ':':
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				event = string(value)
			case "data":
				if has {
					data = append(data, '\n')
				}
				data, has = append(data, value...), true
			}
		}
	}
}

// usage is the usage object of the Messages API.
type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *usage) input() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type event struct {
	Type    string `json:"type"`
	Message *struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	ContentBlock *block `json:"content_block"`
	Delta        struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
	Error *struct {
		Type string `json:"type"`
	} `json:"error"`
}

type message struct {
	Type       string  `json:"type"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      *usage  `json:"usage"`
}

// streamError is an error event in the middle of a 200 stream.
type streamError struct{ typ string }

func (e *streamError) Error() string { return loadgen.StreamError(e.typ) }

// decoder turns the events of a stream, or a whole message, into the
// fields of a Result and the assembled text.
type decoder struct {
	res      *loadgen.Result
	text     strings.Builder
	done     bool
	hasUsage bool
}

// event handles one SSE event and reports whether it carried content.
func (d *decoder) event(name string, data []byte) (bool, error) {
	var ev event
	if json.Unmarshal(data, &ev) != nil {
		return false, parseErr("event %q is not JSON", name)
	}
	if ev.Type == "" {
		ev.Type = name
	}
	switch ev.Type {
	case "error":
		typ := ""
		if ev.Error != nil {
			typ = ev.Error.Type
		}
		return false, &streamError{typ: typ}
	case "message_start":
		if ev.Message != nil {
			d.res.InputTokens = ev.Message.Usage.input()
			d.res.OutputTokens = ev.Message.Usage.OutputTokens
		}
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" {
			d.text.WriteString(ev.ContentBlock.Text)
			return true, nil
		}
		return ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use", nil
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			d.text.WriteString(ev.Delta.Text)
			return ev.Delta.Text != "", nil
		case "thinking_delta":
			return ev.Delta.Thinking != "", nil
		case "input_json_delta":
			return true, nil
		}
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
	return false, nil
}

// whole decodes a non-stream message.
func (d *decoder) whole(body []byte) error {
	var m message
	if json.Unmarshal(body, &m) != nil || m.Type != "message" {
		return parseErr("body is not a message")
	}
	if m.Usage != nil {
		d.hasUsage = true
		d.res.InputTokens, d.res.OutputTokens = m.Usage.input(), m.Usage.OutputTokens
	}
	d.res.FinishReason = m.StopReason
	for _, b := range m.Content {
		if b.Type == "text" {
			d.text.WriteString(b.Text)
			if b.Text != "" {
				d.res.ContentEvents = 1
			}
		}
	}
	d.done = true
	return nil
}

// finish estimates the tokens of a response without usage, as the run does.
func (d *decoder) finish() {
	if !d.hasUsage {
		d.res.InputTokens = 0
		d.res.OutputTokens = d.res.ContentEvents
		d.res.TokensEstimated = true
	}
}
