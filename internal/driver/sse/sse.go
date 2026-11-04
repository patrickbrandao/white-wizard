// Package sse writes Server-Sent Events frames with minimal allocation.
package sse

import (
	"encoding/json"
	"sync"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Writer emits SSE frames to a ResponseWriter and remembers the last frame.
// Frames are built in a buffer reused from one frame to the next (the
// ResponseWriter copies what it keeps, as io.Writer requires), so a frame
// costs no allocation once the buffer has grown.
type Writer struct {
	W     core.ResponseWriter
	mu    sync.Mutex
	last  []byte // the last frame; reused for the next one
	count int
}

// New creates a writer.
func New(w core.ResponseWriter) *Writer { return &Writer{W: w} }

// Count is the number of frames written.
func (s *Writer) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.count }

// Data writes "data: <json>\n\n" for v and flushes.
func (s *Writer) Data(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.DataRaw(b)
}

// DataRaw writes "data: <raw>\n\n" and flushes.
func (s *Writer) DataRaw(raw []byte) error {
	s.mu.Lock()
	f := append(s.last[:0], "data: "...)
	f = append(f, raw...)
	f = append(f, '\n', '\n')
	return s.send(f)
}

// Event writes "event: <name>\ndata: <json>\n\n" (Anthropic style).
func (s *Writer) Event(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	f := append(s.last[:0], "event: "...)
	f = append(f, name...)
	f = append(f, "\ndata: "...)
	f = append(f, b...)
	f = append(f, '\n', '\n')
	return s.send(f)
}

// send records f as the last frame, unlocks and writes it with a flush.
// The caller holds mu.
func (s *Writer) send(f []byte) error {
	s.last = f
	s.count++
	err := s.write(f)
	s.mu.Unlock()
	return err
}

// FrameWriter is implemented by transports that write and flush a frame in
// one call (one lock instead of two per frame; task 341). WriteFrame(p) must
// behave as Write(p) followed by Flush.
type FrameWriter interface {
	WriteFrame(p []byte) error
}

// write writes p and flushes. Under mu: the frame buffer is reused.
func (s *Writer) write(p []byte) error {
	if fw, ok := s.W.(FrameWriter); ok {
		return fw.WriteFrame(p)
	}
	if _, err := s.W.Write(p); err != nil {
		return err
	}
	return s.W.Flush()
}

// Comment writes ": text\n\n" (keep-alive).
func (s *Writer) Comment(text string) error {
	if _, err := s.W.Write([]byte(": " + text + "\n\n")); err != nil {
		return err
	}
	return s.W.Flush()
}

// Raw writes bytes verbatim and flushes.
func (s *Writer) Raw(p []byte) error {
	s.mu.Lock()
	return s.send(append(s.last[:0], p...))
}

// Last returns a copy of the last frame written.
func (s *Writer) Last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil
	}
	return append([]byte(nil), s.last...)
}

// Done writes the OpenAI terminator.
func (s *Writer) Done() error { return s.Raw([]byte("data: [DONE]\n\n")) }
