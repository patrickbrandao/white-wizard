package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// ErrorKind is the provider-neutral error class. Drivers map a kind to the
// HTTP status, body shape and headers of the dialect they imitate.
type ErrorKind string

const (
	ErrInvalidRequest   ErrorKind = "invalid_request"
	ErrContextLength    ErrorKind = "context_length"
	ErrInvalidAPIKey    ErrorKind = "invalid_api_key"
	ErrPermission       ErrorKind = "permission"
	ErrModelNotFound    ErrorKind = "model_not_found"
	ErrRequestTooLarge  ErrorKind = "request_too_large"
	ErrRateLimit        ErrorKind = "rate_limit"
	ErrQuota            ErrorKind = "quota"       // billing / credits / spend cap
	ErrSpendLimit       ErrorKind = "spend_limit" // customer-configured monthly limit
	ErrServerError      ErrorKind = "server_error"
	ErrBadGateway       ErrorKind = "bad_gateway"
	ErrOverloaded       ErrorKind = "overloaded"
	ErrGatewayTimeout   ErrorKind = "gateway_timeout"
	ErrContentFilter    ErrorKind = "content_filter"
	ErrSlowDown         ErrorKind = "slow_down"
	ErrGeoBlocked       ErrorKind = "geo_blocked"
	ErrAccountSuspended ErrorKind = "account_suspended"
	ErrAccountBanned    ErrorKind = "account_banned"
	ErrNotFound         ErrorKind = "not_found"
	ErrMissingSession   ErrorKind = "missing_session"
	ErrBudget           ErrorKind = "budget" // OpenRouter/OpenCode budget policy
	ErrMethodNotAllowed ErrorKind = "method_not_allowed"
	// ErrForgedRequest is the refusal of a client that does not behave
	// like the one a mount verifies (client_verification: claude-code,
	// spec 020, ADR 0074): raised before the credential, it touches no
	// limit. Extra carries the reason and detail (ExtraVerification*).
	ErrForgedRequest ErrorKind = "forged_request"
)

// Error.Extra keys of an ErrForgedRequest: the reason code of spec 020
// (header_order, fingerprint, ...) and what diverged, both strings.
const (
	ExtraVerificationReason = "wwz:client_verification_reason"
	ExtraVerificationDetail = "wwz:client_verification_detail"
)

// Error is a provider-neutral error the engine raises and a driver renders.
type Error struct {
	Kind       ErrorKind
	Message    string
	Param      string
	RetryAfter time.Duration
	// Status overrides the driver's default status for this kind when > 0.
	Status int
	// Code overrides the driver's default code string when set.
	Code string
	// Extra carries dialect-specific metadata (OpenRouter metadata, etc.).
	Extra map[string]any
	// Transport, when set, asks the server to do something other than send a
	// body: reset the connection, hang, send garbage.
	Transport TransportFault
	// TransportDelay is how long the engine waits before the reply when
	// Transport is slow_headers (errors.slow_headers_delay, ADR 0058).
	TransportDelay time.Duration
}

// TransportFault is a non-HTTP failure mode.
type TransportFault string

const (
	TransportNone         TransportFault = ""
	TransportReset        TransportFault = "connection_reset"
	TransportHang         TransportFault = "hang"
	TransportEmpty        TransportFault = "empty_response"
	TransportRandomData   TransportFault = "random_data_then_close"
	TransportSlowHeaders  TransportFault = "slow_headers"
	TransportCorruptBody  TransportFault = "corrupt_body"
	TransportMalformedSSE TransportFault = "malformed_chunk"
)

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// ExtraRawBody is the Error.Extra key carrying a response body that replaces
// the driver's error envelope verbatim (POST /admin/chaos/next "body"). It
// must stay equal to chaos.ExtraRawBody.
const ExtraRawBody = "wwz:raw_body"

// Content types sent with a raw body: JSON when it parses as JSON, plain
// text otherwise.
const (
	RawBodyJSON = "application/json"
	RawBodyText = "text/plain; charset=utf-8"
)

// RawBody returns the verbatim body stored under ExtraRawBody (string or
// []byte, non-empty) and the Content-Type to send with it.
func (e *Error) RawBody() (body []byte, contentType string, ok bool) {
	if e == nil {
		return nil, "", false
	}
	switch v := e.Extra[ExtraRawBody].(type) {
	case string:
		body = []byte(v)
	case []byte:
		body = v
	}
	if len(body) == 0 {
		return nil, "", false
	}
	if json.Valid(body) {
		return body, RawBodyJSON, true
	}
	return body, RawBodyText, true
}

// RenderBody serialises a driver error envelope: a raw body from
// ExtraRawBody wins byte for byte (with its own Content-Type); otherwise
// envelope is used as is when it is []byte, or JSON-encoded. ct is the
// driver's Content-Type for the envelope.
func (e *Error) RenderBody(envelope any, ct string) ([]byte, string) {
	if raw, rawCT, ok := e.RawBody(); ok {
		return raw, rawCT
	}
	if b, ok := envelope.([]byte); ok {
		return b, ct
	}
	b, _ := json.Marshal(envelope)
	return b, ct
}

// NewError builds an Error with a message.
func NewError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Message: msg}
}

// MidStreamFault describes an error injected after the stream has started.
type MidStreamFault struct {
	Kind MidStreamKind
	// AfterChunks is the number of chunks to emit before the fault; AfterRatio
	// (0..1) is used when AfterChunks is 0.
	AfterChunks int
	AfterRatio  float64
	// HangFor is used by StreamHang.
	HangFor time.Duration
	// Resume, when true, continues the stream after the hang.
	Resume bool
	// Error is the error event body for StreamErrorEvent.
	Error *Error
}

// MidStreamKind enumerates mid-stream faults.
type MidStreamKind string

const (
	StreamErrorEvent MidStreamKind = "stream_error_event"
	StreamTruncated  MidStreamKind = "stream_truncated"
	StreamHang       MidStreamKind = "stream_hang"
	StreamMalformed  MidStreamKind = "stream_malformed"
	StreamReset      MidStreamKind = "stream_reset"
	StreamSlowTail   MidStreamKind = "stream_slow_tail"
	StreamDupFrames  MidStreamKind = "duplicate_frames"
	StreamUsageMiss  MidStreamKind = "usage_missing"
)
