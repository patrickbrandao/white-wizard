package opencode

import (
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
)

// extraUpstreamModel marks a 400 relayed from the model's provider: the
// gateway names the model in x-zen-model (task 342).
const extraUpstreamModel = "opencode_upstream_model"

// relayedPrefix is how the gateway relays a 400 of Anthropic: the upstream
// text behind it, in the Messages envelope (conferred on 2026-10-31 with
// claude-haiku-5-5, tasks 301 and 342, testdata/real/opencode/tool-choice/).
const relayedPrefix = upstreamPrefix + "[invalid_request_error] "

// toolChoiceRules are the tool_choice checks of the Messages route: the
// ones of Anthropic's API (keys a type does not accept included), every
// refusal relayed behind relayedPrefix with x-zen-model, after the model is
// resolved (an unknown model is answered first).
var toolChoiceRules = anth.ToolChoiceRules{
	StrictKeys: true,
	Refuse: func(req *core.Request, msg string, _ bool) *core.Error {
		return &core.Error{Kind: core.ErrInvalidRequest, Message: relayedPrefix + msg, Extra: map[string]any{extraUpstreamModel: req.Model, driver.ExtraAfterModel: req.Model}}
	},
}
