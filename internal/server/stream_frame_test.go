package server

import (
	"errors"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// TestWriteFrameModes: WriteFrame is Write then Flush under one lock (task
// 341): before Stream the frame lands in the response body; in direct mode
// it is one chunk on the connection; after the client went away it fails
// as Write does.
func TestWriteFrameModes(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	w := newFastWriter(ctx, func() {}, time.Millisecond, false)
	if err := w.WriteFrame([]byte("body")); err != nil || string(ctx.Response.Body()) != "body" {
		t.Fatalf("frame before streaming: %v %q", err, ctx.Response.Body())
	}

	w, cli, _ := streamingWriter(t)
	done := make(chan error, 1)
	goDirect(t, w, done)
	waitDirect(t, w)
	go func() { _ = w.WriteFrame([]byte("data: hello\n\n")) }()
	if got := readN(t, cli, len("d\r\ndata: hello\n\n\r\n")); got != "d\r\ndata: hello\n\n\r\n" {
		t.Fatalf("direct frame %q", got)
	}
	_ = w.Close()
	if err := w.WriteFrame([]byte("late")); !errors.Is(err, errClientGone) {
		t.Fatalf("frame after close: %v", err)
	}
	if err := <-done; !errors.Is(err, errClientGone) {
		t.Fatalf("read after close: %v", err)
	}
}
