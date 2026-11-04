package anth

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// toolChoiceKeys are the keys each tool_choice type accepts; any other is
// refused by Anthropic's API (conferred on 2026-10-31, task 264,
// testdata/real/anthropic/tool-choice/).
var toolChoiceKeys = map[string]string{
	"auto": " type disable_parallel_tool_use ",
	"any":  " type disable_parallel_tool_use ",
	"tool": " type name disable_parallel_tool_use ",
	"none": " type ",
}

// toolChoiceExtra is the 400 for the first key (in body order) that the
// tool_choice type does not accept, as "tool_choice.none.
// disable_parallel_tool_use: Extra inputs are not permitted"; nil when
// there is none.
func toolChoiceExtra(body []byte, typ string) *core.Error {
	allowed, ok := toolChoiceKeys[typ]
	if !ok {
		return nil
	}
	// ParseMessages decoded the body, so tool_choice is a valid object.
	var top struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	_ = json.Unmarshal(body, &top)
	dec := json.NewDecoder(bytes.NewReader(top.ToolChoice))
	_, _ = dec.Token()
	for dec.More() {
		t, _ := dec.Token()
		k, _ := t.(string)
		if !strings.Contains(allowed, " "+k+" ") {
			return &core.Error{Kind: core.ErrInvalidRequest, Message: "tool_choice." + typ + "." + k + ": Extra inputs are not permitted"}
		}
		var skip json.RawMessage
		_ = dec.Decode(&skip)
	}
	return nil
}

// forcedToolUnsupported lists the models that refuse tool_choice any and
// tool (conferred on 2026-10-31; claude-haiku-5-5 and earlier accept them).
var forcedToolUnsupported = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"}

// errForcedTool is their 400.
const errForcedTool = `tool_choice: type "tool" and "any" are not supported for this model.`

// refusesForcedTool reads the model id as Anthropic writes it or as
// OpenRouter does ("anthropic/claude-opus-5.5", conferred on 2026-10-31,
// task 301).
func refusesForcedTool(model string) bool {
	model = strings.ReplaceAll(strings.TrimPrefix(model, "anthropic/"), ".", "-")
	for _, p := range forcedToolUnsupported {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// ToolChoiceRules are how a dialect checks tool_choice; the zero value is
// the lenient gateway form (OpenRouter ignores the keys a type does not
// accept, task 301).
type ToolChoiceRules struct {
	// StrictKeys refuses the first key the type does not accept ("Extra
	// inputs are not permitted"): Anthropic's API and OpenCode, which
	// relays that refusal (task 301).
	StrictKeys bool
	// IgnoreWithoutTools drops tool_choice from a request without tools
	// (absent or empty) once its type is checked: OpenRouter answers 200 to
	// any and to a named tool there, even on the models that refuse them
	// (conferred on 2026-10-31, task 342,
	// testdata/real/openrouter/tool-choice/).
	IgnoreWithoutTools bool
	// Refuse, when set, turns a refusal into the dialect's error for req (model parsed): msg is
	// the text of Anthropic's API and unknownType reports a missing type or
	// one outside the four. OpenCode relays the refusal behind its upstream
	// prefix; OpenRouter answers an unknown type with its own validation
	// and relays the rest with the upstream attempt as metadata (task 342).
	Refuse func(req *core.Request, msg string, unknownType bool) *core.Error
}

func (r *ToolChoiceRules) refuse(req *core.Request, msg string, unknownType bool) *core.Error {
	if r.Refuse != nil {
		return r.Refuse(req, msg, unknownType)
	}
	return &core.Error{Kind: core.ErrInvalidRequest, Message: msg}
}

// toolChoiceHasType reports whether the request's tool_choice has a type
// key (ParseMessages decoded the body, so tool_choice is an object).
func toolChoiceHasType(body []byte) bool {
	var top struct {
		ToolChoice map[string]json.RawMessage `json:"tool_choice"`
	}
	_ = json.Unmarshal(body, &top)
	_, ok := top.ToolChoice["type"]
	return ok
}

// parseToolChoice sets req.ToolChoice from the request's tool_choice (type
// and name) and refuses it like the real API (tasks 301 and 342,
// testdata/real/anthropic/tool-choice/): a missing type ("tool_choice.type:
// Field required"), a key the type does not accept (h.ToolChoice.StrictKeys),
// a type outside the four ("tool_choice: Input tag 'x' found using 'type'
// does not match any of the expected tags: ..."), a named tool missing from
// tools ("Tool 'zzz' not found in provided tools", also without tools), any
// without tools and any/tool on the models that refuse them, in this order.
// h.ToolChoice adapts the checks and the refusals to a gateway.
func parseToolChoice(h *Hooks, req *core.Request, body []byte, typ, name string) *core.Error {
	rules := &h.ToolChoice
	if typ == "" && !toolChoiceHasType(body) {
		return rules.refuse(req, "tool_choice.type: Field required", true)
	}
	if rules.StrictKeys {
		if e := toolChoiceExtra(body, typ); e != nil {
			return rules.refuse(req, e.Message, false)
		}
	}
	switch typ {
	case "auto":
	case "any":
		req.ToolChoice.Mode = core.ToolChoiceRequired
	case "none":
		req.ToolChoice.Mode = core.ToolChoiceNone
	case "tool":
		req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNamed, Name: name}
	default:
		return rules.refuse(req, "tool_choice: Input tag '"+typ+"' found using 'type' does not match any of the expected tags: 'auto', 'any', 'tool', 'none'", true)
	}
	if rules.IgnoreWithoutTools && len(req.Tools) == 0 {
		req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
		return nil
	}
	switch req.ToolChoice.Mode {
	case core.ToolChoiceNamed:
		if !slices.ContainsFunc(req.Tools, func(t core.ToolDef) bool { return t.Name == name }) {
			return rules.refuse(req, "Tool '"+name+"' not found in provided tools", false)
		}
	case core.ToolChoiceRequired:
		if len(req.Tools) == 0 {
			return rules.refuse(req, "tool_choice.any may only be specified while providing tools", false)
		}
	default:
		return nil
	}
	if refusesForcedTool(req.Model) {
		return rules.refuse(req, errForcedTool, false)
	}
	return nil
}
