package server

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// fastWriter adapts a fasthttp RequestCtx to core.ResponseWriter and
// engine.Streamer. Before Stream is called, writes land in the response
// body. Stream runs the engine's stream function in a goroutine; header calls
// made before the first Write are applied to the response, then the handler
// returns and fasthttp sends the headers and the body as chunked encoding.
//
// The streamed body has two phases (see Read). Until fasthttp has flushed
// the headers with the first frame, flushed frames are handed to fasthttp
// through Read. From then on the stream goroutine writes each flushed frame,
// already chunk-encoded, straight to the connection in one write call,
// while fasthttp's goroutine waits in Read for the end of the stream: no
// pipe, no goroutine handoff and no copy per frame.
type fastWriter struct {
	ctx     *fasthttp.RequestCtx
	cancel  context.CancelFunc
	hangFor time.Duration
	// anthropic is the dialect of the matched route (see anthropicDialect):
	// the transport faults that still write a body imitate its shape.
	anthropic bool

	mu        sync.Mutex
	streaming bool
	ready     chan struct{} // closed when headers are final
	readyOnce sync.Once
	gone      chan struct{} // closed when the client went away
	goneOnce  sync.Once
	faulted   bool
	transport core.TransportFault
	// onClose runs once when the streamed body ends (normally or because the
	// client went away); the server uses it to stop the disconnect watcher.
	onClose func()

	// Streaming state, guarded by mu.
	conn   net.Conn
	pend   []byte              // written, not flushed yet; starts with chunkHead reserved bytes
	out    []byte              // flushed before direct mode, for fasthttp to Read
	handed bool                // Read returned data: fasthttp has sent the headers
	direct bool                // fasthttp flushed everything handed so far: write to conn
	ended  bool                // the stream function returned
	fault  core.TransportFault // transport fault that ends the stream
	broken bool                // a direct write failed
	wake   chan struct{}       // signals Read: out has data or the stream ended
}

// chunkHead is the room reserved in front of pend for the chunk-size line
// ("<hex>\r\n"); 8 hex digits cover any frame.
const chunkHead = 10

func newFastWriter(ctx *fasthttp.RequestCtx, cancel context.CancelFunc, hang time.Duration, anthropic bool) *fastWriter {
	return &fastWriter{ctx: ctx, cancel: cancel, hangFor: hang, anthropic: anthropic, ready: make(chan struct{}), gone: make(chan struct{})}
}

func (w *fastWriter) Status(code int) {
	w.mu.Lock()
	if !w.streaming || !w.isReady() {
		w.ctx.SetStatusCode(code)
	}
	w.mu.Unlock()
}

func (w *fastWriter) Header(k, v string) {
	w.mu.Lock()
	if !w.streaming || !w.isReady() {
		w.ctx.Response.Header.Set(k, v)
	}
	w.mu.Unlock()
}

func (w *fastWriter) isReady() bool {
	select {
	case <-w.ready:
		return true
	default:
		return false
	}
}

// isStreaming reports whether Stream was called.
func (w *fastWriter) isStreaming() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streaming
}

func (w *fastWriter) markReady() { w.readyOnce.Do(func() { close(w.ready) }) }

func (w *fastWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.streaming {
		w.mu.Unlock()
		w.ctx.Response.AppendBody(p)
		return len(p), nil
	}
	if w.broken || w.isGone() {
		w.mu.Unlock()
		return 0, errClientGone
	}
	w.pend = append(w.pend, p...)
	w.mu.Unlock()
	w.markReady()
	return len(p), nil
}

var errClientGone = errors.New("client gone")

// WriteFrame is Write(p) followed by Flush under one lock (sse.FrameWriter):
// in direct mode the frame goes out as one chunk in one write call.
func (w *fastWriter) WriteFrame(p []byte) error {
	w.mu.Lock()
	if !w.streaming {
		w.mu.Unlock()
		w.ctx.Response.AppendBody(p)
		return nil
	}
	if w.broken || w.isGone() {
		w.mu.Unlock()
		return errClientGone
	}
	w.pend = append(w.pend, p...)
	err := w.flushLocked()
	w.mu.Unlock()
	w.markReady()
	return err
}

func (w *fastWriter) isGone() bool {
	select {
	case <-w.gone:
		return true
	default:
		return false
	}
}

func (w *fastWriter) Flush() error {
	w.mu.Lock()
	if !w.streaming {
		w.mu.Unlock()
		return nil
	}
	err := w.flushLocked()
	w.mu.Unlock()
	w.markReady()
	return err
}

// flushLocked sends pend: to fasthttp (out) before direct mode, else as one
// chunk written to the connection. Only the stream goroutine writes, so
// holding mu during the write blocks nobody but a Read waiting for the end.
func (w *fastWriter) flushLocked() error {
	if w.broken || w.fault != "" || w.isGone() {
		return errClientGone
	}
	n := len(w.pend) - chunkHead
	if n <= 0 {
		return nil
	}
	if !w.direct {
		w.out = append(w.out, w.pend[chunkHead:]...)
		w.pend = w.pend[:chunkHead]
		w.signal()
		return nil
	}
	start := chunkHead - 2
	w.pend[start], w.pend[start+1] = '\r', '\n'
	for v := n; ; v >>= 4 {
		start--
		w.pend[start] = hexDigits[v&0xf]
		if v < 16 {
			break
		}
	}
	w.pend = append(w.pend, '\r', '\n')
	_, err := w.conn.Write(w.pend[start:])
	w.pend = w.pend[:chunkHead]
	if err != nil {
		w.broken = true
		w.signal()
		return errClientGone
	}
	return nil
}

const hexDigits = "0123456789abcdef"

// signal wakes Read (mu held).
func (w *fastWriter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Read feeds fasthttp's chunked body writer, which writes and flushes each
// returned slice as one chunk before calling Read again. The first frames
// flushed go out this way, together with the headers; once everything handed
// out has been flushed, the next Read switches to direct mode and waits for
// the end of the stream: io.EOF ends the body normally, an error makes
// fasthttp drop the connection.
func (w *fastWriter) Read(p []byte) (int, error) {
	for {
		w.mu.Lock()
		if len(w.out) > 0 {
			n := copy(p, w.out)
			w.out = append(w.out[:0], w.out[n:]...)
			w.handed = true
			w.mu.Unlock()
			return n, nil
		}
		if w.broken {
			w.mu.Unlock()
			return 0, errClientGone
		}
		if w.fault != "" {
			f := w.fault
			w.mu.Unlock()
			switch f {
			case core.TransportHang:
				time.Sleep(w.hangFor)
			}
			w.closeConn()
			return 0, errors.New("transport fault " + string(f))
		}
		if w.ended {
			w.mu.Unlock()
			return 0, io.EOF
		}
		// The headers go out with the first chunk fasthttp writes: until one
		// was handed out, frames keep coming through Read.
		w.direct = w.handed
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-w.gone:
			return 0, errClientGone
		}
	}
}

// Close is called by fasthttp once the body is written (or failed): the
// request is over for the engine and the disconnect watcher.
func (w *fastWriter) Close() error {
	w.end()
	return nil
}

// end marks the client gone (once): cancels the request context, stops the
// watcher.
func (w *fastWriter) end() {
	w.goneOnce.Do(func() {
		close(w.gone)
		w.cancel()
		if w.onClose != nil {
			w.onClose()
		}
	})
}

// defaultHangFor is how long a hang fault lasts when none is configured
// (a variable so tests can shorten it).
var defaultHangFor = 60 * time.Second

// Fault performs a transport-level failure.
func (w *fastWriter) Fault(f core.TransportFault) bool {
	w.mu.Lock()
	streaming := w.streaming
	w.faulted, w.transport = true, f
	if streaming {
		// Data written before the fault still goes out, then the fault ends
		// the stream. Faults that only stop writing (truncation) end it with
		// the normal terminator.
		_ = w.flushLocked()
		switch f {
		case core.TransportReset, core.TransportEmpty, core.TransportRandomData, core.TransportHang:
			w.fault = f
			w.signal()
		}
		w.mu.Unlock()
		w.markReady()
		return true
	}
	w.mu.Unlock()
	switch f {
	case core.TransportReset:
		w.closeConn()
		w.ctx.HijackSetNoResponse(true)
		w.ctx.Hijack(func(net.Conn) {})
	case core.TransportHang:
		d := w.hangFor
		if d <= 0 {
			d = defaultHangFor
		}
		time.Sleep(d)
		w.closeConn()
		w.ctx.HijackSetNoResponse(true)
		w.ctx.Hijack(func(net.Conn) {})
	case core.TransportEmpty:
		w.ctx.SetStatusCode(200)
		w.ctx.Response.Header.Set("Content-Type", "application/json")
		w.ctx.Response.SetBody(nil)
	case core.TransportRandomData:
		buf := make([]byte, 512)
		_, _ = rand.Read(buf)
		w.ctx.HijackSetNoResponse(true)
		w.ctx.Hijack(func(c net.Conn) {
			_, _ = c.Write(buf)
			_ = c.Close()
		})
	case core.TransportSlowHeaders:
		// The engine already waited errors.slow_headers_delay (ADR 0058).
		return false // then write the normal error
	case core.TransportCorruptBody:
		w.ctx.SetStatusCode(200)
		w.ctx.Response.Header.Set("Content-Type", "application/json")
		if w.anthropic {
			w.ctx.Response.SetBodyString(`{"id":"msg_wwz","type":"message","role":"assistant","content":[{"type":"text","text":"trunc`)
		} else {
			w.ctx.Response.SetBodyString(`{"id":"chatcmpl-wwz","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"trunc`)
		}
	case core.TransportMalformedSSE:
		w.ctx.SetStatusCode(200)
		w.ctx.Response.Header.Set("Content-Type", "text/event-stream")
		if w.anthropic {
			w.ctx.Response.SetBodyString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			w.ctx.Response.SetBodyString("data: {\"id\":\"chatcmpl-wwz\",\"choices\":[{\"delta\":{\"content\":\"\n\ndata: [DONE]\n\n")
		}
	default:
		return false
	}
	return true
}

// closeConn drops the connection: the streamed one, else the request's.
func (w *fastWriter) closeConn() {
	c := w.conn
	if c == nil {
		c = w.ctx.Conn()
	}
	if c != nil {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // RST instead of FIN
		}
		_ = c.Close()
	}
}

// Stream switches to streaming mode: fn runs in its own goroutine, the
// handler returns once headers are final and fasthttp pulls the body from
// Read (see fastWriter).
func (w *fastWriter) Stream(fn func(core.ResponseWriter)) {
	w.startStreaming(w.ctx.Conn())
	go func() {
		defer w.markReady()
		fn(w)
		w.endStream()
	}()
	<-w.ready
	// Headers are final now (set by Start before the first Write).
	w.ctx.SetBodyStream(w, -1)
}

// startStreaming switches the writer to streaming over conn.
func (w *fastWriter) startStreaming(conn net.Conn) {
	w.mu.Lock()
	w.streaming = true
	w.conn = conn
	w.pend = make([]byte, chunkHead, 1024)
	w.wake = make(chan struct{}, 1)
	w.mu.Unlock()
}

// endStream sends what is left and tells Read the stream is over.
func (w *fastWriter) endStream() {
	w.mu.Lock()
	_ = w.flushLocked()
	w.ended = true
	w.signal()
	w.mu.Unlock()
}
