package openrouter

import (
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// extraRelayed marks a 400 of the model's provider relayed on the Messages
// route: the gateway adds the upstream answer as metadata (task 342).
const extraRelayed = "openrouter_relayed"

// toolChoiceRules are the tool_choice checks of /api/v1/messages
// (conferred on 2026-10-31, tasks 301 and 342,
// testdata/real/openrouter/tool-choice/): keys a type does not accept are
// ignored; without tools tool_choice is dropped (200 to any and to a named
// tool, even on the models that refuse them); a missing or unknown type is
// the gateway's own validation; the other refusals are Anthropic's,
// relayed with metadata.
var toolChoiceRules = anth.ToolChoiceRules{
	IgnoreWithoutTools: true,
	Refuse: func(req *core.Request, msg string, unknownType bool) *core.Error {
		if unknownType {
			return rawError(msg, rawReply{status: 400, body: toolChoiceTypeBody})
		}
		// Relayed: an unknown model is answered first.
		return &core.Error{Kind: core.ErrInvalidRequest, Message: msg, Extra: map[string]any{extraRelayed: true, driver.ExtraAfterModel: req.Model}}
	},
}

// toolChoiceTypeBody is the gateway's 400 for a tool_choice without one of
// the four types (missing or unknown, with or without tools): its schema
// issue in metadata.raw, request_id null, no generation id.
var toolChoiceTypeBody = func() string {
	raw, _ := upstreamIndent.Marshal([]any{oai.O(
		"code", "invalid_union", "errors", []any{}, "note", "No matching discriminator", "discriminator", "type",
		"options", []any{"auto", "any", "none", "tool"}, "path", []any{"tool_choice", "type"},
		"message", "Invalid discriminator value. Expected 'auto' | 'any' | 'none' | 'tool'",
	)})
	return `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid Anthropic Messages API request","error_type":"invalid_request"},"request_id":null,"metadata":{"raw":` + jsonString(string(raw)) + `}}`
}()

// relayedBody is the Messages 400 of a refusal relayed from Anthropic: the
// gateway's envelope (error_type, the generation id as request_id) with the
// upstream answer as metadata.raw, the provider that gave it and is_byok.
// The real also lists the providers tried before (previous_errors) when it
// fell back; the fake has one provider, so it never does.
func relayedBody(req *core.Request, msg string) string {
	raw := `{"type":"error","error":{"type":"invalid_request_error","message":` + jsonString(msg) + `},"request_id":` + jsonString(anth.NewRequestID()) + `}`
	return `{"type":"error","error":{"type":"invalid_request_error","message":` + jsonString(msg) + `,"error_type":"invalid_request"},"request_id":` + jsonString(req.RequestID) +
		`,"metadata":{"raw":` + jsonString(raw) + `,"provider_name":` + jsonString(providerName) + `,"is_byok":false}}`
}
