package anthropic

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// drivers[].options.client_verification (spec 020, ADR 0074, item 4): off
// is the real API; claude-code refuses, with a 403 forged_request_error,
// every inference request that is not exactly what a Claude Code version
// of the profile sends.
const (
	verifyOff        = "off"
	verifyClaudeCode = "claude-code"
)

var (
	_ driver.RawHeadWanter  = (*Driver)(nil)
	_ driver.ClientVerifier = (*Driver)(nil)
)

// WantsRawHead: the strict verification compares the request line and the
// headers as they arrived; without it nothing is copied (spec 004).
func (d *Driver) WantsRawHead() bool { return d.verify }

// VerifiesClient: with client_verification: claude-code, the two
// inference routes; /v1/models, the OAuth and account routes and
// /api/hello are never verified (ADR 0074, item 7).
func (d *Driver) VerifiesClient(kind driver.Kind) bool {
	return d.verify && (kind == driver.KindMessages || kind == driver.KindCountTokens)
}

// VerifyClient is claudecode.Verify over the raw head and body. Without a
// raw head (the request did not come through the data plane) there is no
// request line to check: the refusal is the line's.
func (d *Driver) VerifyClient(_ driver.Kind, head driver.RawHead, body []byte) *core.Error {
	reason, detail, ok := claudecode.ReasonQuery, "no request line received", false
	if head.Line != "" {
		reason, detail, ok = claudecode.Verify(claudeHead(head), body)
	}
	if ok {
		return nil
	}
	msg := "Client verification failed: this request does not match Claude Code " + strings.Join(claudecode.Versions(), ", ") +
		"; the client is forging Claude Code requests (" + reason + ": " + detail + ")."
	return driver.ForgedRequest(reason, detail, msg)
}

// claudeHead is the raw head of the data plane (task 444) as
// claudecode.Head: the request line split at its spaces, the headers in
// arrival order with repeats. A line without three parts keeps what it
// has, and Verify refuses it (query).
func claudeHead(r driver.RawHead) claudecode.Head {
	var h claudecode.Head
	h.Method, h.Target, _ = strings.Cut(r.Line, " ")
	h.Target, h.Proto, _ = strings.Cut(h.Target, " ")
	raw := r.Headers()
	h.Headers = make([]claudecode.Header, len(raw))
	for i, hd := range raw {
		h.Headers[i] = claudecode.Header{Name: hd.Name, Value: hd.Value}
	}
	return h
}
