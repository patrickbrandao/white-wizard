package openrouter

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// RequestID is the generation id, "gen-<epoch>-<20 base62>", on every
// surface: it is x-generation-id of the errors past the key check and, on
// /api/v1/messages, request-id and the body's request_id as well
// (conferred on 2026-10-31, task 174).
func (d *Driver) RequestID(_ driver.Kind, unique string) string {
	return ids.OpenRouterGen(ids.FromToken(unique), time.Now())
}

// ResponseID is the body's id: the generation id on Chat Completions (also
// x-generation-id), the upstream's message id on /api/v1/messages ("msg_01…"
// of an Anthropic upstream; the real one depends on the provider, e.g.
// msg_bdrk_… from Amazon Bedrock).
func (d *Driver) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	if kind == driver.KindMessages {
		return ids.AnthropicMessage(rng)
	}
	return ids.OpenRouterGen(rng, now)
}

// ToolCallID returns "toolu_01…" on /messages and "call_…" otherwise.
func (d *Driver) ToolCallID(kind driver.Kind, rng *generate.RNG) string {
	if kind == driver.KindMessages {
		return ids.AnthropicToolUse(rng)
	}
	return ids.OpenAICall(rng)
}
