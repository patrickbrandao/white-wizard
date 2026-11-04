package loadgen

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/valyala/fasthttp"
)

// Error classes of spec 015 ("Classificação de erros"). HTTP and mid-stream
// errors carry the provider type: see HTTPError and StreamError.
const (
	ErrTimeout         = "timeout"
	ErrConnect         = "connect"
	ErrReset           = "reset"
	ErrStreamTruncated = "stream_truncated"
	ErrParse           = "parse"
	// ErrCanceled is a request interrupted by its context being canceled
	// (not by a deadline). It is not in the spec table: the runner decides
	// whether to count it (an interrupted run waits for in-flight requests).
	ErrCanceled = "canceled"
	// ErrRequest is a request that could not be encoded (caller bug); it is
	// never sent.
	ErrRequest = "request"
)

// envelope matches the error bodies of OpenAI, Anthropic and OpenRouter.
type envelope struct {
	Error *errObj `json:"error"`
}

// rawString returns a JSON string or number as text ("" for null/other).
func rawString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		return n.String()
	}
	return ""
}

// errorType extracts error.type (OpenAI, Anthropic) or, failing that,
// error.code (OpenRouter) from a JSON error body. "" if none.
func errorType(body []byte) string {
	var e envelope
	if json.Unmarshal(body, &e) != nil || e.Error == nil {
		return ""
	}
	return e.Error.kind()
}

// HTTPError classifies a response with status ≥ 400: http_<status>/<type>
// with a recognized envelope, http_<status> otherwise.
func HTTPError(status int, body []byte) string {
	s := "http_" + strconv.Itoa(status)
	if t := errorType(body); t != "" {
		return s + "/" + t
	}
	return s
}

// StreamError classifies an error event in the middle of a 200 stream.
func StreamError(typ string) string {
	if typ == "" {
		typ = "unknown"
	}
	return "stream_error/" + typ
}

// IsHTTPError reports whether class came from an HTTP status (http_*).
func IsHTTPError(class string) bool { return strings.HasPrefix(class, "http_") }

// classifyTransport maps a transport error to timeout, connect, reset or
// canceled. ctx is the request context.
func classifyTransport(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ErrCanceled
		}
		return ErrTimeout
	}
	if errors.Is(err, fasthttp.ErrDialTimeout) || errors.Is(err, fasthttp.ErrNoFreeConns) {
		// The dial gets the remaining request time; ErrNoFreeConns waits up
		// to the timeout too.
		return ErrTimeout
	}
	if isConnectError(err) {
		return ErrConnect
	}
	var ne net.Error
	if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return ErrTimeout
	}
	return ErrReset
}

// isConnectError: DNS failure, refused connection or TLS handshake.
func isConnectError(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	var rec tls.RecordHeaderError
	var alert tls.AlertError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dns), errors.As(err, &rec), errors.As(err, &alert), errors.As(err, &cert),
		errors.Is(err, syscall.ECONNREFUSED):
		return true
	case errors.As(err, &op) && op.Op == "dial":
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "x509: ")
}
