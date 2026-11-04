package sse_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/sse"
)

// rec records writes and flushes; failWrite/failFlush inject errors.
type rec struct {
	buf       bytes.Buffer
	flushes   int
	failWrite bool
	failFlush bool
}

var errBoom = errors.New("boom")

func (r *rec) Status(int)            {}
func (r *rec) Header(string, string) {}
func (r *rec) Write(p []byte) (int, error) {
	if r.failWrite {
		return 0, errBoom
	}
	return r.buf.Write(p)
}

func (r *rec) Flush() error {
	if r.failFlush {
		return errBoom
	}
	r.flushes++
	return nil
}
func (r *rec) Fault(core.TransportFault) bool { return false }

func TestFrames(t *testing.T) {
	r := &rec{}
	w := sse.New(r)
	if w.Count() != 0 || w.Last() != nil {
		t.Fatalf("fresh writer: count=%d last=%q", w.Count(), w.Last())
	}
	steps := []struct {
		do   func() error
		want string
	}{
		{func() error { return w.Data(map[string]int{"a": 1}) }, "data: {\"a\":1}\n\n"},
		{func() error { return w.DataRaw([]byte(`{"b":2}`)) }, "data: {\"b\":2}\n\n"},
		{func() error { return w.Event("ping", map[string]string{"type": "ping"}) }, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{func() error { return w.Raw([]byte("raw bytes")) }, "raw bytes"},
		{w.Done, "data: [DONE]\n\n"},
	}
	for i, s := range steps {
		r.buf.Reset()
		if err := s.do(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := r.buf.String(); got != s.want {
			t.Fatalf("step %d wrote %q, want %q", i, got, s.want)
		}
		if got := string(w.Last()); got != s.want {
			t.Fatalf("step %d Last() = %q, want %q", i, got, s.want)
		}
	}
	if w.Count() != len(steps) || r.flushes != len(steps) {
		t.Fatalf("count=%d flushes=%d, want %d", w.Count(), r.flushes, len(steps))
	}
	// A comment is a keep-alive: written and flushed but not a frame.
	r.buf.Reset()
	if err := w.Comment("keep"); err != nil {
		t.Fatal(err)
	}
	if r.buf.String() != ": keep\n\n" || w.Count() != len(steps) || string(w.Last()) != "data: [DONE]\n\n" {
		t.Fatalf("comment: wrote %q count=%d last=%q", r.buf.String(), w.Count(), w.Last())
	}
}

// TestLastIsACopy: the remembered frame is not aliased to the reused buffer.
func TestLastIsACopy(t *testing.T) {
	w := sse.New(&rec{})
	_ = w.DataRaw([]byte("1"))
	first := w.Last()
	_ = w.DataRaw([]byte("2"))
	if string(first) != "data: 1\n\n" {
		t.Fatalf("first frame mutated: %q", first)
	}
}

func TestMarshalError(t *testing.T) {
	r := &rec{}
	w := sse.New(r)
	bad := make(chan int)
	if err := w.Data(bad); err == nil {
		t.Fatal("Data with unmarshalable value: want error")
	}
	if err := w.Event("x", bad); err == nil {
		t.Fatal("Event with unmarshalable value: want error")
	}
	if r.buf.Len() != 0 || w.Count() != 0 {
		t.Fatalf("nothing must be written: %q count=%d", r.buf.String(), w.Count())
	}
}

func TestWriteAndFlushErrors(t *testing.T) {
	ops := map[string]func(w *sse.Writer) error{
		"DataRaw": func(w *sse.Writer) error { return w.DataRaw([]byte("x")) },
		"Event":   func(w *sse.Writer) error { return w.Event("e", 1) },
		"Comment": func(w *sse.Writer) error { return w.Comment("c") },
		"Raw":     func(w *sse.Writer) error { return w.Raw([]byte("x")) },
		"Done":    func(w *sse.Writer) error { return w.Done() },
	}
	for name, op := range ops {
		for _, mode := range []string{"write", "flush"} {
			r := &rec{failWrite: mode == "write", failFlush: mode == "flush"}
			if err := op(sse.New(r)); !errors.Is(err, errBoom) {
				t.Errorf("%s with %s error: got %v", name, mode, err)
			}
		}
	}
}
