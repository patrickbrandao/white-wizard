package loadgen

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// Result is the measure of one request (spec 015, "Medidas no cliente").
// Only successes (Err == "") enter TTFT, ITL and E2E.
type Result struct {
	// Start is t0: the instant right before the request is handed to the
	// HTTP client.
	Start  time.Time
	Status int // 0 when no response arrived
	// TTFT: t0 to the first event with content (= E2E without stream).
	TTFT time.Duration
	// ITL: every interval between consecutive content events.
	ITL []time.Duration
	// E2E: t0 to the last byte of the body.
	E2E             time.Duration
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	// TokensEstimated: no usage in the response; OutputTokens is the
	// number of content events and InputTokens is 0.
	TokensEstimated bool
	// ContentEvents counts the events with content (1 for a non-stream
	// response with content).
	ContentEvents int
	FinishReason  string
	ToolCalls     []ToolCall
	// Message is the assistant message, with Request.Capture.
	Message *Message
	// ReasoningRedacted: an Anthropic redacted_thinking block came (the
	// reasoning exists, its text does not).
	ReasoningRedacted bool
	// Scenario and Step are the x-wwz-scenario and x-wwz-step response
	// headers, with Request.Capture ("" when absent).
	Scenario, Step string
	// RetryAfterMS and RetryAfter are the raw retry-after-ms and
	// retry-after headers of an error response (status ≥ 400; "" when
	// absent), for clients that retry (wwz-load agents).
	RetryAfterMS, RetryAfter string
	// Err is the error class ("" = success): see errors.go.
	Err string
	// Cause is the underlying error, for logs.
	Cause error
}

// OK reports a success.
func (r *Result) OK() bool { return r.Err == "" }

func (r *Result) reset() {
	itl := r.ITL[:0]
	*r = Result{ITL: itl}
}

// Options configure a Client.
type Options struct {
	// Timeout is the ceiling of a whole request (send to end of body);
	// exceeded, the request is a timeout. Default 60s.
	Timeout time.Duration
	// MaxConns per host. Default 65536.
	MaxConns int
	// Dial replaces the TCP dialer (tests, in-memory listeners).
	Dial fasthttp.DialFunc
	// TLSConfig for https targets.
	TLSConfig *tls.Config
	// MaxBody caps a non-stream or error body. Default 8 MiB.
	MaxBody int
}

// Client sends Requests. It is safe for concurrent use; connections are
// kept alive and reused.
type Client struct {
	hc      *fasthttp.Client
	timeout time.Duration
	maxBody int
	sinks   sync.Pool
}

// NewClient builds a Client.
func NewClient(o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 65536
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 8 << 20
	}
	c := &Client{timeout: o.Timeout, maxBody: o.MaxBody}
	c.hc = &fasthttp.Client{
		Name:                "wwz-load",
		StreamResponseBody:  true,
		MaxConnsPerHost:     o.MaxConns,
		MaxConnWaitTimeout:  o.Timeout,
		ReadBufferSize:      16 << 10,
		TLSConfig:           o.TLSConfig,
		MaxIdleConnDuration: 30 * time.Second,
		// A load generator never retries: every attempt is a measure.
		RetryIfErr: func(*fasthttp.Request, int, error) (bool, bool) { return false, false },
	}
	if o.Dial != nil {
		dial := o.Dial
		c.hc.Dial = func(addr string) (net.Conn, error) { return track(dial(addr)) }
	} else {
		c.hc.DialTimeout = func(addr string, t time.Duration) (net.Conn, error) {
			return track(fasthttp.DialTimeout(addr, t))
		}
	}
	c.sinks.New = func() any { return &sink{} }
	return c
}

// CloseIdleConnections closes the keep-alive connections not in use.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

// Do sends r and measures it.
func (c *Client) Do(ctx context.Context, r *Request) Result {
	var res Result
	c.DoInto(ctx, r, &res)
	return res
}

// DoInto is Do writing into res, reusing its ITL slice.
//
// The request ends at Options.Timeout or at the context deadline, whichever
// comes first. Canceling ctx interrupts the body as soon as the response
// headers have arrived (before that, the timeout still bounds the wait).
func (c *Client) DoInto(ctx context.Context, r *Request, res *Result) {
	res.reset()
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	if err := r.fill(req); err != nil {
		res.Err, res.Cause = ErrRequest, err
		return
	}
	timeout := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(dl))
	}
	if err := ctx.Err(); err != nil || timeout <= 0 {
		res.Err, res.Cause = ErrTimeout, context.DeadlineExceeded
		if errors.Is(err, context.Canceled) {
			res.Err, res.Cause = ErrCanceled, err
		}
		return
	}
	req.SetTimeout(timeout)

	res.Start = time.Now()
	err := c.hc.Do(req, resp)
	if err != nil {
		res.E2E = time.Since(res.Start)
		res.Err, res.Cause = classifyTransport(ctx, err), err
		return
	}
	res.Status = resp.StatusCode()
	if r.Capture {
		res.Scenario = string(resp.Header.Peek("x-wwz-scenario"))
		res.Step = string(resp.Header.Peek("x-wwz-step"))
	}
	if res.Status >= 400 {
		res.RetryAfterMS = string(resp.Header.Peek("retry-after-ms"))
		res.RetryAfter = string(resp.Header.Peek("retry-after"))
	}

	// Cancellation: poison the read deadline of this connection.
	if tc := connOf(resp); tc != nil {
		seq := tc.current()
		stop := context.AfterFunc(ctx, func() { tc.abort(seq) })
		defer stop()
	}

	s := c.sinks.Get().(*sink)
	defer c.sinks.Put(s)
	ok2xx := res.Status >= 200 && res.Status < 300
	s.reset(r, res, c.maxBody, ok2xx && bytes.HasPrefix(resp.Header.ContentType(), []byte("text/event-stream")))
	err = resp.BodyWriteTo(s)
	if err == nil && s.sse {
		err = s.parser.Close()
	}
	res.E2E = time.Since(res.Start)
	c.classify(ctx, res, s, ok2xx, err)
}

func (c *Client) classify(ctx context.Context, res *Result, s *sink, ok2xx bool, err error) {
	var se *streamErr
	switch {
	case !ok2xx:
		res.Err = HTTPError(res.Status, s.body)
		res.Cause = err
		return
	case errors.As(err, &se):
		res.Err, res.Cause = se.Error(), err
		return
	case errors.Is(err, errParse):
		res.Err, res.Cause = ErrParse, err
		return
	case err != nil && !s.dec.done:
		res.Err, res.Cause = classifyTransport(ctx, err), err
		return
	case !s.sse:
		if perr := s.dec.whole(s.body); perr != nil {
			res.Err, res.Cause = ErrParse, perr
			return
		}
		if s.dec.content {
			res.ContentEvents = 1
		}
		res.TTFT = res.E2E
	case !s.dec.done:
		res.Err = ErrStreamTruncated
		return
	case res.ContentEvents == 0:
		res.TTFT = res.E2E
	}
	s.dec.finish()
}

// errBodyTooLarge: a non-stream body above Options.MaxBody.
var errBodyTooLarge = errors.New("body too large")

// sink receives the response body as it arrives.
type sink struct {
	res     *Result
	sse     bool
	body    []byte
	maxBody int
	parser  sseParser
	dec     decoder
	now     time.Time
	last    time.Time
}

func (s *sink) reset(r *Request, res *Result, maxBody int, sse bool) {
	s.res, s.sse, s.maxBody = res, sse, maxBody
	s.body = s.body[:0]
	s.parser.line, s.parser.event, s.parser.data = s.parser.line[:0], s.parser.event[:0], s.parser.data[:0]
	s.parser.hasData = false
	s.parser.onEvent = s.onEvent
	s.dec.reset(r.Protocol, res, r.Capture)
}

// Write is called by fasthttp for every chunk read from the connection.
func (s *sink) Write(p []byte) (int, error) {
	if !s.sse {
		if len(s.body)+len(p) > s.maxBody {
			return 0, errors.Join(errParse, errBodyTooLarge)
		}
		s.body = append(s.body, p...)
		return len(p), nil
	}
	s.now = time.Now()
	if err := s.parser.Feed(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *sink) onEvent(name, data []byte) error {
	if err := s.dec.event(name, data); err != nil {
		return err
	}
	if s.dec.content {
		s.res.ContentEvents++
		if s.res.ContentEvents == 1 {
			s.res.TTFT = s.now.Sub(s.res.Start)
		} else {
			s.res.ITL = append(s.res.ITL, s.now.Sub(s.last))
		}
		s.last = s.now
	}
	return nil
}

// trackedConn lets a canceled request interrupt the read of its own
// connection without touching a connection already reused by another
// request: fasthttp calls SetWriteDeadline at the start of every request,
// which advances seq and disarms stale aborts.
type trackedConn struct {
	net.Conn
	addr *connAddr
	mu   sync.Mutex
	seq  uint64
}

// connAddr is the LocalAddr of a trackedConn; fasthttp exposes it in
// Response.LocalAddr, which is how a request finds its connection.
type connAddr struct {
	net.Addr
	c *trackedConn
}

func track(c net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c}
	tc.addr = &connAddr{Addr: c.LocalAddr(), c: tc}
	return tc, nil
}

func (c *trackedConn) LocalAddr() net.Addr { return c.addr }

func (c *trackedConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.seq++
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *trackedConn) current() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// abort makes the pending read fail now if the connection still serves the
// request that observed seq.
func (c *trackedConn) abort(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seq == seq {
		_ = c.SetReadDeadline(time.Unix(1, 0))
	}
}

func connOf(resp *fasthttp.Response) *trackedConn {
	if a, ok := resp.LocalAddr().(*connAddr); ok {
		return a.c
	}
	return nil
}
