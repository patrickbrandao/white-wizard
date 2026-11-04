package sse_test

import (
	"errors"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// frameRec is a rec that also writes whole frames (sse.FrameWriter).
type frameRec struct {
	rec
	frames []string
	fail   bool
}

func (r *frameRec) WriteFrame(p []byte) error {
	if r.fail {
		return errBoom
	}
	r.frames = append(r.frames, string(p))
	return nil
}

// TestFrameWriter: a transport that writes and flushes a frame in one call
// gets each frame through WriteFrame, never Write and Flush (task 341).
func TestFrameWriter(t *testing.T) {
	r := &frameRec{}
	w := sse.New(r)
	_ = w.DataRaw([]byte(`{"a":1}`))
	_ = w.Event("e", 2)
	if len(r.frames) != 2 || r.frames[0] != "data: {\"a\":1}\n\n" || r.frames[1] != "event: e\ndata: 2\n\n" {
		t.Fatalf("frames %q", r.frames)
	}
	if r.buf.Len() != 0 || r.flushes != 0 {
		t.Fatal("no Write/Flush with a FrameWriter")
	}
	r.fail = true
	if err := w.DataRaw([]byte("x")); !errors.Is(err, errBoom) {
		t.Fatalf("WriteFrame error: %v", err)
	}
}
