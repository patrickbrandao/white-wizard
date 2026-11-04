package driver

import "github.com/patrickbrandao/white-wizard/internal/core"

// ClientVerifier is implemented by a driver that, per mount, refuses the
// requests of every client but the one it expects (spec 005, "Assinatura
// e verificação do cliente", interface (b); spec 020; ADR 0074): the
// anthropic driver with client_verification: claude-code. The engine asks
// it first, before the credential, the body and the limits, and a refusal
// touches nothing in the store. It is found with As, so a wrapper keeps
// it; a driver without it, or a mount that verifies nothing, costs the
// hot path one type assertion.
type ClientVerifier interface {
	// VerifiesClient reports whether this mount verifies the client of
	// the requests of kind.
	VerifiesClient(kind Kind) bool
	// VerifyClient checks one request of kind: head is its raw head (the
	// zero RawHead when the server stored none) and body the body as
	// received. nil accepts it; a refusal is an ErrForgedRequest
	// (ForgedRequest).
	VerifyClient(kind Kind, head RawHead, body []byte) *core.Error
}

// ClientVerifierOf is the verifier of d (or of a driver it wraps) when it
// verifies the client of kind's requests.
func ClientVerifierOf(d Driver, kind Kind) (ClientVerifier, bool) {
	v, ok := As[ClientVerifier](d)
	if !ok || !v.VerifiesClient(kind) {
		return nil, false
	}
	return v, true
}

// ForgedRequest is the refusal of a forged client: reason is a code of
// spec 020, detail what diverged and message the driver's error text.
func ForgedRequest(reason, detail, message string) *core.Error {
	return &core.Error{Kind: core.ErrForgedRequest, Message: message, Extra: map[string]any{
		core.ExtraVerificationReason: reason,
		core.ExtraVerificationDetail: detail,
	}}
}

// ForgedReason is the reason and detail of a ForgedRequest; false for any
// other error.
func ForgedReason(e *core.Error) (reason, detail string, ok bool) {
	if e == nil || e.Kind != core.ErrForgedRequest {
		return "", "", false
	}
	reason, _ = e.Extra[core.ExtraVerificationReason].(string)
	detail, _ = e.Extra[core.ExtraVerificationDetail].(string)
	return reason, detail, true
}
