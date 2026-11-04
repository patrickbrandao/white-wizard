package server

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// streamingWriter is a fastWriter in streaming mode over one end of a pipe;
// the other end is returned for reading what goes straight to the client.
func streamingWriter(t *testing.T) (*fastWriter, net.Conn, *atomic.Int32) {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() { _ = srv.Close(); _ = cli.Close() })
	var cancels atomic.Int32
	w := newFastWriter(&fasthttp.RequestCtx{}, func() { cancels.Add(1) }, time.Millisecond, false)
	w.onClose = func() { cancels.Add(10) }
	w.startStreaming(srv)
	return w, cli, &cancels
}

// waitDirect waits until a Read call switched the writer to direct mode.
func waitDirect(t *testing.T, w *fastWriter) {
	t.Helper()
	waitFor(t, "direct mode", 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.direct
	})
}

// goDirect hands fasthttp a first frame (the one that carries the headers),
// then starts the Read that switches to direct mode; its error goes to done.
func goDirect(t *testing.T, w *fastWriter, done chan<- error) {
	t.Helper()
	_, _ = w.Write([]byte("h"))
	_ = w.Flush()
	if n, err := w.Read(make([]byte, 8)); n != 1 || err != nil {
		t.Fatalf("first frame: %d %v", n, err)
	}
	go func() { _, err := w.Read(make([]byte, 8)); done <- err }()
}

// readN reads exactly n bytes from c.
func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	b := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestFastWriterPhases: frames flushed before fasthttp has sent the headers
// go through Read; then Read switches to direct mode and every flush is one
// chunk written to the connection; the end of the stream is io.EOF, and
// Close cancels the request and stops the watcher once.
func TestFastWriterPhases(t *testing.T) {
	w, cli, cancels := streamingWriter(t)
	_, _ = w.Write([]byte("first"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("-second"))
	_ = w.Flush()
	if !w.isReady() {
		t.Fatal("a streamed write makes the headers final")
	}
	p := make([]byte, 5)
	if n, err := w.Read(p); err != nil || string(p[:n]) != "first" {
		t.Fatalf("read %q %v", p[:n], err)
	}
	if n, err := w.Read(p); err != nil || string(p[:n]) != "-seco" {
		t.Fatalf("read %q %v", p[:n], err)
	}
	if n, err := w.Read(p); err != nil || string(p[:n]) != "nd" {
		t.Fatalf("read %q %v", p[:n], err)
	}
	if err := w.Flush(); err != nil { // nothing pending: no chunk
		t.Fatal(err)
	}

	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() { n, err := w.Read(p); done <- res{n, err} }()
	waitDirect(t, w)
	go func() {
		_, _ = w.Write([]byte("0123456789abcdefX"))
		_ = w.Flush()
	}()
	if got := readN(t, cli, len("11\r\n0123456789abcdefX\r\n")); got != "11\r\n0123456789abcdefX\r\n" {
		t.Fatalf("direct chunk %q", got)
	}
	go func() {
		_, _ = w.Write([]byte("tail"))
		w.endStream()
	}()
	if got := readN(t, cli, len("4\r\ntail\r\n")); got != "4\r\ntail\r\n" {
		t.Fatalf("last chunk %q", got)
	}
	if r := <-done; r.n != 0 || r.err != io.EOF {
		t.Fatalf("end of stream: %d %v", r.n, r.err)
	}
	_ = w.Close()
	_ = w.Close()
	if cancels.Load() != 11 {
		t.Fatalf("cancel and onClose must run once each: %d", cancels.Load())
	}
	if _, err := w.Write([]byte("late")); !errors.Is(err, errClientGone) {
		t.Fatalf("write after close: %v", err)
	}
	if err := w.Flush(); !errors.Is(err, errClientGone) {
		t.Fatalf("flush after close: %v", err)
	}
}

// TestFastWriterNoDirectBeforeHeaders: the headers are final at the first
// Write, before its Flush; a Read that comes then must wait for the frame
// instead of letting it go straight to the connection ahead of the headers.
func TestFastWriterNoDirectBeforeHeaders(t *testing.T) {
	w, _, _ := streamingWriter(t)
	_, _ = w.Write([]byte("frame"))
	got := make(chan string, 1)
	go func() {
		p := make([]byte, 16)
		n, _ := w.Read(p)
		got <- string(p[:n])
	}()
	time.Sleep(20 * time.Millisecond)
	w.mu.Lock()
	direct := w.direct
	w.mu.Unlock()
	if direct {
		t.Fatal("direct mode before any chunk was handed to fasthttp")
	}
	_ = w.Flush()
	if s := <-got; s != "frame" {
		t.Fatalf("first frame through Read: %q", s)
	}
}

// TestStreamsBackToBack: zero-latency streams on keep-alive connections,
// where the stream often finishes before fasthttp sends the headers; every
// response must parse and end with [DONE] (task 158 regression: a frame
// written ahead of the headers broke the next response).
func TestStreamsBackToBack(t *testing.T) {
	s, err := New(newEngine(t, fastCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	cl := &fasthttp.Client{}
	url := "http://" + ln.Addr().String() + "/v1/chat/completions"
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				req := fasthttp.AcquireRequest()
				req.Header.SetMethod("POST")
				req.SetRequestURI(url)
				req.Header.Set("Authorization", "Bearer wwz-key-01")
				req.Header.SetContentType("application/json")
				req.SetBodyString(`{"model":"wwz-fast","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
				resp := fasthttp.AcquireResponse()
				err := cl.DoTimeout(req, resp, 5*time.Second)
				if err != nil || resp.StatusCode() != 200 || !strings.HasSuffix(string(resp.Body()), "data: [DONE]\n\n") {
					t.Errorf("stream: %v %d %q", err, resp.StatusCode(), resp.Body())
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			}
		}()
	}
	wg.Wait()
}

// TestFastWriterDirectWriteFails: a failed direct write marks the stream
// broken: the engine sees the error and Read ends the body with an error.
func TestFastWriterDirectWriteFails(t *testing.T) {
	w, cli, _ := streamingWriter(t)
	done := make(chan error, 1)
	goDirect(t, w, done)
	waitDirect(t, w)
	_ = cli.Close()
	_, _ = w.Write([]byte("x"))
	if err := w.Flush(); !errors.Is(err, errClientGone) {
		t.Fatalf("flush to a closed peer: %v", err)
	}
	if err := <-done; !errors.Is(err, errClientGone) {
		t.Fatalf("read after a broken write: %v", err)
	}
	if _, err := w.Write([]byte("y")); !errors.Is(err, errClientGone) {
		t.Fatalf("write after a broken write: %v", err)
	}
}

// TestFastWriterStreamFaults: transport faults flush what was written,
// then end the body with an error and drop the connection; others only
// stop writing.
func TestFastWriterStreamFaults(t *testing.T) {
	for _, f := range []core.TransportFault{core.TransportReset, core.TransportHang, core.TransportEmpty, core.TransportRandomData} {
		w, cli, _ := streamingWriter(t)
		done := make(chan error, 1)
		goDirect(t, w, done)
		waitDirect(t, w)
		go func() {
			_, _ = w.Write([]byte("ab"))
			if !w.Fault(f) {
				t.Errorf("%s: a streaming fault is always handled", f)
			}
		}()
		if got := readN(t, cli, len("2\r\nab\r\n")); got != "2\r\nab\r\n" {
			t.Fatalf("%s: data before the fault %q", f, got)
		}
		if err := <-done; err == nil || err == io.EOF {
			t.Fatalf("%s: read must fail, got %v", f, err)
		}
		if _, err := cli.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s: connection must be closed", f)
		}
		if err := w.Flush(); !errors.Is(err, errClientGone) {
			t.Fatalf("%s: flush after the fault: %v", f, err)
		}
	}

	// A fault the stream cannot express leaves it running.
	w, _, _ := streamingWriter(t)
	if !w.Fault(core.TransportSlowHeaders) || w.fault != "" {
		t.Fatalf("slow headers mid-stream: fault %q", w.fault)
	}
}

// TestFastWriterFaultBeforeDirect: a fault raised while frames still wait
// for fasthttp hands them out first.
func TestFastWriterFaultBeforeDirect(t *testing.T) {
	w, _, _ := streamingWriter(t)
	_, _ = w.Write([]byte("abc"))
	w.Fault(core.TransportReset)
	p := make([]byte, 8)
	if n, err := w.Read(p); err != nil || string(p[:n]) != "abc" {
		t.Fatalf("pending data first: %q %v", p[:n], err)
	}
	if _, err := w.Read(p); err == nil {
		t.Fatal("then the fault")
	}
}

// TestFastWriterReadUnblocksOnClose: a Read waiting in direct mode returns
// when the writer is closed.
func TestFastWriterReadUnblocksOnClose(t *testing.T) {
	w, _, _ := streamingWriter(t)
	done := make(chan error, 1)
	goDirect(t, w, done)
	waitDirect(t, w)
	_ = w.Close()
	if err := <-done; !errors.Is(err, errClientGone) {
		t.Fatalf("read after close: %v", err)
	}
}
