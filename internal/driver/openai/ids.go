package openai

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// RequestID returns "req_" + 32 hex, the x-request-id OpenAI sends.
func (d *Driver) RequestID(_ driver.Kind, unique string) string {
	return ids.OpenAIRequest(ids.FromToken(unique))
}

// ResponseID returns "chatcmpl-…", "cmpl-…" on the legacy completions API
// or "resp_" + 50 hex on the Responses API.
func (d *Driver) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	switch kind {
	case driver.KindCompletions:
		return ids.OpenAICompletion(rng)
	case driver.KindResponses:
		return oai.ResponsesID(rng, now)
	}
	return ids.OpenAIChat(rng)
}

// ToolCallID returns "call_" + 24 base62.
func (d *Driver) ToolCallID(_ driver.Kind, rng *generate.RNG) string { return ids.OpenAICall(rng) }
