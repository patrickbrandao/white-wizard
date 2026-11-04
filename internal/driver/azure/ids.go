package azure

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// RequestID is apim-request-id: a version 4 UUID, reproducible from the
// request token.
func (d *Driver) RequestID(_ driver.Kind, unique string) string {
	return uuid(ids.FromToken(unique))
}

// ResponseID is the OpenAI id the service passes through: "chatcmpl-…",
// "cmpl-…" on the legacy completions (see fixID) or "resp_…" on the
// Responses API.
func (d *Driver) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	switch kind {
	case driver.KindCompletions:
		return ids.OpenAICompletion(rng)
	case driver.KindResponses:
		return oai.ResponsesID(rng, now)
	}
	return ids.OpenAIChat(rng)
}

// ToolCallID is "call_" + 24 base62, as on OpenAI.
func (d *Driver) ToolCallID(_ driver.Kind, rng *generate.RNG) string { return ids.OpenAICall(rng) }

const hexDigits = "0123456789abcdef"

// uuid draws a version 4 UUID from rng.
func uuid(rng *generate.RNG) string {
	var u [16]byte
	for i := range u {
		u[i] = byte(rng.IntN(256)) //nolint:gosec // G115: IntN(256) fits a byte
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	b := make([]byte, 0, 36)
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hexDigits[c>>4], hexDigits[c&0xF])
	}
	return string(b)
}

// requestID is the apim-request-id of a response: the request's own id
// when the engine minted one, else a fresh UUID (parse errors, /models).
func requestID(req *core.Request) string {
	if req != nil && len(req.RequestID) == 36 {
		return req.RequestID
	}
	return oai.NewUUID()
}
