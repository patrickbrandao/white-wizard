package core

import (
	"bufio"
	"io"
	"sync"
)

// ResponseWriter is the transport abstraction drivers write to. The fasthttp
// data plane and net/http control plane both implement it, as does the
// in-memory writer used by tests and by `wwz simulate`.
type ResponseWriter interface {
	// Status sets the HTTP status; must be called before the first Write.
	Status(code int)
	// Header sets a response header; must be called before the first Write.
	Header(key, value string)
	// Write appends body bytes.
	Write(p []byte) (int, error)
	// Flush pushes buffered bytes to the client (SSE needs it per event).
	Flush() error
	// Fault asks the transport to perform a non-HTTP failure. Implementations
	// that cannot (e.g. tests) return false.
	Fault(f TransportFault) bool
}

// StreamWriter receives the engine's abstract events and emits the dialect's
// wire format. Every driver provides one. The engine calls the methods in this
// order: Start, then any number of ReasoningDelta / TextDelta /
// ToolCallStart / ToolArgsDelta / ToolCallEnd / Ping, then exactly one of
// Finish or Error, then Close.
type StreamWriter interface {
	Start(resp *Response) error
	ReasoningDelta(text string) error
	TextDelta(text string) error
	ToolCallStart(index int, id, name string) error
	ToolArgsDelta(index int, text string) error
	ToolCallEnd(index int) error
	Ping() error
	Finish(reason FinishReason, usage Usage, includeUsage bool) error
	// Error emits the dialect's mid-stream error event (after Start).
	Error(e *Error) error
	// Raw writes bytes verbatim (used for malformed-chunk and duplicate-frame
	// faults). Drivers return the last frame they wrote via LastFrame.
	Raw(p []byte) error
	LastFrame() []byte
	Close() error
}

// BufferedWriter adapts a bufio.Writer (fasthttp stream body) to ResponseWriter
// for the streaming phase, where headers are already sent.
type BufferedWriter struct {
	W *bufio.Writer
	// OnFault is invoked for transport faults the stream can honour.
	OnFault func(TransportFault) bool
}

func (b *BufferedWriter) Status(int)                  {}
func (b *BufferedWriter) Header(string, string)       {}
func (b *BufferedWriter) Write(p []byte) (int, error) { return b.W.Write(p) }
func (b *BufferedWriter) Flush() error                { return b.W.Flush() }
func (b *BufferedWriter) Fault(f TransportFault) bool {
	if b.OnFault != nil {
		return b.OnFault(f)
	}
	return false
}

// MemWriter is an in-memory ResponseWriter for tests and simulate mode.
type MemWriter struct {
	mu      sync.Mutex
	status  int
	headers map[string]string
	body    []byte
	faults  []TransportFault
	// OnFlush, when set, is called with the bytes written since the last flush.
	OnFlush func([]byte)
	pending []byte
}

func NewMemWriter() *MemWriter { return &MemWriter{status: 200, headers: map[string]string{}} }

func (m *MemWriter) Status(code int) { m.mu.Lock(); m.status = code; m.mu.Unlock() }
func (m *MemWriter) Header(k, v string) {
	m.mu.Lock()
	m.headers[k] = v
	m.mu.Unlock()
}

func (m *MemWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	m.body = append(m.body, p...)
	m.pending = append(m.pending, p...)
	m.mu.Unlock()
	return len(p), nil
}

func (m *MemWriter) Flush() error {
	m.mu.Lock()
	p := m.pending
	m.pending = nil
	cb := m.OnFlush
	m.mu.Unlock()
	if cb != nil && len(p) > 0 {
		cb(p)
	}
	return nil
}

func (m *MemWriter) Fault(f TransportFault) bool {
	m.mu.Lock()
	m.faults = append(m.faults, f)
	m.mu.Unlock()
	return true
}
func (m *MemWriter) StatusCode() int { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *MemWriter) Headers() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.headers))
	for k, v := range m.headers {
		out[k] = v
	}
	return out
}

func (m *MemWriter) Body() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.body...)
}

func (m *MemWriter) Faults() []TransportFault {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]TransportFault(nil), m.faults...)
}

var _ io.Writer = (*MemWriter)(nil)

// Stream runs fn immediately; MemWriter has no separate body phase.
func (m *MemWriter) Stream(fn func(ResponseWriter)) { fn(m) }
