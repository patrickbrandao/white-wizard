package openrouter

import (
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// keepAliveEvery is how long the stream waits for its first byte before
// OpenRouter's ": OPENROUTER PROCESSING" comment, and the interval between
// comments (spec 006 16.7: 1 s in the wwz; the real interval was not
// conferred). keepAliveFor caps the comments of a stream the engine
// abandoned before Start.
var (
	keepAliveEvery = time.Second
	keepAliveFor   = 10 * time.Minute
)

// processing is the keep-alive comment frame.
var processing = []byte(": " + keepAliveComment + "\n\n")

// stream is the Chat Completions StreamWriter of OpenRouter: the oai
// chunks plus the gateway's habits. A response that takes longer than
// keepAliveEvery to begin gets the 200 headers and a PROCESSING comment
// per interval while it waits; a fast one gets none (observed). The usage
// chunk always closes the stream, with or without stream_options.
type stream struct {
	*oai.Stream
	rw core.ResponseWriter

	mu       sync.Mutex
	timer    *time.Timer
	born     time.Time
	every    time.Duration // keepAliveEvery, read once
	deadline time.Time     // born + keepAliveFor
	opened   bool          // status and headers written by a keep-alive
	halted   bool          // Start, Error or Close ran: no more keep-alives
}

func newStream(h *oai.Hooks, w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) *stream {
	s := &stream{Stream: oai.NewStream(h, w, req, rl), rw: w, born: time.Now(), every: keepAliveEvery}
	s.deadline = s.born.Add(keepAliveFor)
	s.mu.Lock()
	s.timer = time.AfterFunc(s.every, s.tick)
	s.mu.Unlock()
	return s
}

// tick sends one keep-alive and re-arms the timer.
func (s *stream) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted {
		return
	}
	if err := s.comment(); err != nil || !time.Now().Before(s.deadline) {
		s.halted = true // the client is gone, or the engine never started
		return
	}
	s.timer.Reset(s.every)
}

// comment writes the keep-alive, opening the response first if needed.
// The caller holds mu.
func (s *stream) comment() error {
	if !s.opened {
		s.opened = true
		s.rw.Status(200)
		s.rw.Header("Content-Type", "text/event-stream")
		s.rw.Header("Cache-Control", "no-cache")
		s.rw.Header("Access-Control-Expose-Headers", exposeHeaders)
		s.rw.Header("Access-Control-Allow-Origin", allowOrigin)
	}
	if err := s.Raw(processing); err != nil {
		return err
	}
	return s.rw.Flush()
}

// halt stops the keep-alives; a tick in progress finishes first, so its
// comment lands before whatever the caller writes next.
func (s *stream) halt() {
	s.mu.Lock()
	s.halted = true
	s.mu.Unlock()
	s.timer.Stop()
}

func (s *stream) Start(resp *core.Response) error {
	s.halt()
	s.mu.Lock()
	s.opened = true
	s.mu.Unlock()
	return s.Stream.Start(resp)
}

// Ping is the engine's keep-alive (server.ping_interval): OpenRouter's
// comment, never ": ping".
func (s *stream) Ping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.comment()
}

func (s *stream) Finish(reason core.FinishReason, usage core.Usage, _ bool) error {
	return s.Stream.Finish(reason, usage, true)
}

func (s *stream) Error(e *core.Error) error {
	s.halt()
	return s.Stream.Error(e)
}

func (s *stream) Close() error {
	s.halt()
	return s.Stream.Close()
}
