package gemini

import (
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Content types (observed).
const (
	contentJSON = "application/json; charset=UTF-8"
	contentSSE  = "text/event-stream"
)

// securityHeaders go out on every answer, the empty 404 included
// (observed).
func securityHeaders(w core.ResponseWriter) {
	w.Header("X-XSS-Protection", "0")
	w.Header("X-Frame-Options", "SAMEORIGIN")
	w.Header("X-Content-Type-Options", "nosniff")
}

// commonHeaders are the front end's headers on a JSON or SSE answer
// (observed): Vary, Server, the security trio and Server-Timing with the
// time spent, in milliseconds. The real sends Vary three times (X-Origin,
// Referer, Origin plus Accept-Encoding on compressible bodies); one header
// carries the three request headers the answer varies on.
func commonHeaders(w core.ResponseWriter, req *core.Request) {
	w.Header("Vary", "Origin, X-Origin, Referer")
	w.Header("Server", "scaffolding on HTTPServer2")
	securityHeaders(w)
	serverTiming(w, req)
}

// serverTiming is the front end's Server-Timing: the time spent, in ms.
func serverTiming(w core.ResponseWriter, req *core.Request) {
	dur := int64(0)
	if req != nil && !req.Received.IsZero() {
		dur = time.Since(req.Received).Milliseconds()
	}
	w.Header("Server-Timing", "gfet4t7; dur="+strconv.FormatInt(dur, 10))
}

// extraHeaders copies the engine's headers (x-wwz-*) of a response.
func extraHeaders(w core.ResponseWriter, resp *core.Response) {
	for k, v := range resp.Headers {
		w.Header(k, v)
	}
}
