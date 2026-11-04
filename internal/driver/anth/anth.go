// Package anth implements the Anthropic Messages dialect shared by the
// anthropic and opencode drivers.
package anth

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Hooks customise the dialect.
type Hooks struct {
	Name string
	// ErrorBody renders the error envelope: status, body (a map in the
	// Anthropic envelope, which WriteError orders like the real, or []byte /
	// json.RawMessage written verbatim), content type and extra headers.
	ErrorBody func(req *core.Request, e *core.Error) (int, any, string, map[string]string)
	// RateLimitHeaders renders limit headers when the limiter reported state.
	RateLimitHeaders func(rl *core.RateLimitState) map[string]string
	// SuccessHeaders, when set, renders every dialect header of a Messages
	// 200 (body and stream) instead of RateLimitHeaders: Anthropic sends its
	// account and the 12 anthropic-ratelimit-* on every success (spec 006
	// 16.7, ADR 0049).
	SuccessHeaders func(req *core.Request, resp *core.Response, rl *core.RateLimitState) map[string]string
	// AccountHeaders, when set, renders the account headers of the answers
	// past the key check: errors other than 401, count_tokens and /models.
	AccountHeaders func(req *core.Request) map[string]string
	// UsageExtras adds fields to usage (appended in sorted key order).
	UsageExtras func(req *core.Request, resp *core.Response, u core.Usage) map[string]any
	// Shape, when set, gets every object before it is written and returns
	// it reordered or with fields removed (a gateway's own form).
	Shape func(part Part, o oai.Obj) oai.Obj
	// RequireVersion: anthropic-version header is required.
	RequireVersion bool
	// ServiceTier reported in usage ("standard").
	ServiceTier string
	// RedactedThinkingProbability (0..100) emits redacted_thinking blocks.
	RedactedThinkingProbability float64
	// Signature for thinking blocks ("" = none). With API set the signature
	// is a base64 blob derived from the response id, like the real one;
	// otherwise this literal.
	Signature string
	// API is the form of Anthropic's own API (spec 006 16.7): container,
	// stop_details and diagnostics in the message; cache_creation,
	// inference_geo, speed and output_tokens_details in usage; caller in
	// tool_use; a ping after the first content_block_start; padded SSE
	// data; x-should-retry on errors. The gateways (OpenRouter, OpenCode)
	// leave it off until their own forms are conferred.
	API bool
	// ToolChoice are the checks of tool_choice and the dialect's refusals
	// (APIHooks: strict keys, Anthropic's texts).
	ToolChoice ToolChoiceRules
	// ClaudeCode turns on the forms the real API accepts that Claude Code
	// 2.1.308 sends (ADR 0074, conferred on 2026-11-03, task 441): a
	// role "system" message in messages is a system message of the
	// conversation; thinking.type "adaptive" asks for reasoning, sized by
	// output_config.effort; thinking.display "omitted" and "updates" blank
	// the thinking text; context_management puts its field in the answer
	// (ForRequest, NewRequestStream). Only the anthropic driver has it: the
	// gateways keep the refusal of role "system" until their forms are
	// conferred.
	ClaudeCode bool
}

// Part names an object the dialect writes; Hooks.Shape receives it.
type Part string

const (
	// PartMessage is the message object (body and message_start).
	PartMessage Part = "message"
	// PartUsage is the usage of the message.
	PartUsage Part = "usage"
	// PartDeltaUsage is the usage of message_delta.
	PartDeltaUsage Part = "delta_usage"
	// PartBlock is one content block (body and content_block_start).
	PartBlock Part = "block"
	// PartEvent is the top level of every stream event.
	PartEvent Part = "event"
)

func (h *Hooks) shape(p Part, o oai.Obj) oai.Obj {
	if h.Shape != nil {
		return h.Shape(p, o)
	}
	return o
}

// DefaultHooks returns the Messages dialect the gateways share: the
// Anthropic field order and envelope, without the account headers and the
// fields only Anthropic's own API sends (see APIHooks).
func DefaultHooks() Hooks {
	return Hooks{Name: "anthropic", ErrorBody: AnthropicErrorBody, RateLimitHeaders: AnthropicRateLimitHeaders, RequireVersion: true, ServiceTier: "standard", Signature: "wwz-sig-" + strings.Repeat("A", 40)}
}

// APIHooks returns Anthropic's own API (spec 006 16.7): DefaultHooks plus
// the account and limit headers of every answer and the full form.
func APIHooks() Hooks {
	h := DefaultHooks()
	h.SuccessHeaders = AnthropicSuccessHeaders
	h.AccountHeaders = AnthropicAccountHeaders
	h.API = true
	h.ToolChoice.StrictKeys = true
	return h
}

type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens *int              `json:"max_tokens"`
	System    json.RawMessage   `json:"system"`
	Messages  []json.RawMessage `json:"messages"`
	Tools     []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Type        string          `json:"type"`
	} `json:"tools"`
	ToolChoice *struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel *bool  `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Stream   bool `json:"stream"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Thinking *struct {
		Type         string          `json:"type"`
		BudgetTokens int             `json:"budget_tokens"`
		Display      json.RawMessage `json:"display"`
	} `json:"thinking"`
	// OutputConfig and ContextManagement are read only with
	// Hooks.ClaudeCode; raw, so a value of another type never fails the
	// decoding (the validator refuses it).
	OutputConfig      json.RawMessage `json:"output_config"`
	ContextManagement json.RawMessage `json:"context_management"`
	Temperature       *float64        `json:"temperature"`
	StopSequences     []string        `json:"stop_sequences"`
	ServiceTier       string          `json:"service_tier"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Thinking  string          `json:"thinking"`
	Source    json.RawMessage `json:"source"`
}

// ParseMessages converts a Messages API body into a core.Request.
func ParseMessages(h *Hooks, body []byte, hdr driver.Headers, driverName string) (*core.Request, *core.Error) {
	var wr messagesRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Invalid JSON: " + err.Error()}
	}
	if wr.Model == "" {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "model: Field required"}
	}
	if wr.MaxTokens == nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "max_tokens: Field required"}
	}
	if *wr.MaxTokens < 0 {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "max_tokens: Input should be greater than or equal to 0"}
	}
	if len(wr.Messages) == 0 {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "messages: at least one message is required"}
	}
	req := &core.Request{Driver: driverName, Endpoint: core.EndpointMessages, Model: wr.Model, MaxTokens: *wr.MaxTokens, Stream: wr.Stream, Raw: body, Received: time.Now(), Temperature: wr.Temperature, IncludeUsage: true}
	if wr.Metadata != nil && wr.Metadata.UserID != "" {
		req.Metadata = map[string]string{"user_id": wr.Metadata.UserID, "user": wr.Metadata.UserID}
	}
	req.System = flattenSystem(wr.System)
	if h.ClaudeCode {
		claudeCodeForm(req, &wr)
	}
	if wr.Thinking != nil && wr.Thinking.Type == "enabled" {
		req.Reasoning = core.ReasoningRequest{Requested: true, BudgetTokens: wr.Thinking.BudgetTokens}
		if wr.Thinking.BudgetTokens < 1024 {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "thinking.budget_tokens: Input should be greater than or equal to 1024"}
		}
		if wr.Thinking.BudgetTokens >= *wr.MaxTokens {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "max_tokens must be greater than thinking.budget_tokens"}
		}
	}
	for i, raw := range wr.Messages {
		var m wireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("messages.%d: invalid message", i)}
		}
		if m.Role != "user" && m.Role != "assistant" && (m.Role != "system" || !h.ClaudeCode) {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("messages.%d.role: Input should be 'user' or 'assistant'", i)}
		}
		// The real API accepts a conversation that starts with assistant and
		// consecutive turns of the same role (conferred on 2026-10-31,
		// testdata/real/anthropic/shape/assistant_first.json, user_user.json).
		msg := core.Message{Role: m.Role}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			msg.Content = s
		} else {
			var blocks []block
			if err := json.Unmarshal(m.Content, &blocks); err != nil {
				return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("messages.%d.content: Input should be a valid string or list of content blocks", i)}
			}
			var text strings.Builder
			for _, b := range blocks {
				switch b.Type {
				case "text":
					// Blocks join with nothing between them, as the real
					// counts them (task 301: [x, x] costs what "xx" does).
					text.WriteString(b.Text)
				case "image", "document":
					msg.ImageParts++
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
				case "tool_result":
					tr := core.ToolResult{ToolCallID: b.ToolUseID, IsError: b.IsError}
					var cs string
					if json.Unmarshal(b.Content, &cs) == nil {
						tr.Content = cs
					} else {
						var inner []block
						if json.Unmarshal(b.Content, &inner) == nil {
							var tb strings.Builder
							for _, ib := range inner {
								if ib.Type == "text" {
									tb.WriteString(ib.Text)
								}
							}
							tr.Content = tb.String()
						}
					}
					msg.ToolResults = append(msg.ToolResults, tr)
				case "thinking", "redacted_thinking":
				}
			}
			msg.Content = text.String()
			if m.Role == "system" {
				// Each text block of a role "system" message costs as a
				// system message of its own (task 453: [s, t] counts what
				// two messages "s" and "t" do, not "st").
				req.Messages = appendSystemBlocks(req.Messages, blocks)
				continue
			}
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range wr.Tools {
		if t.Name == "" {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "tools.0.name: Field required"}
		}
		req.Tools = append(req.Tools, core.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
	if wr.ToolChoice != nil {
		if e := parseToolChoice(h, req, body, wr.ToolChoice.Type, wr.ToolChoice.Name); e != nil {
			return nil, e
		}
		if wr.ToolChoice.DisableParallel != nil && *wr.ToolChoice.DisableParallel {
			f := false
			req.ParallelToolCalls = &f
		}
	}
	return req, nil
}

// appendSystemBlocks appends a role "system" message per text block, or one
// without text when there is none (the directive-only form, tool_addition
// and tool_removal).
func appendSystemBlocks(ms []core.Message, blocks []block) []core.Message {
	n := len(ms)
	for _, b := range blocks {
		if b.Type == "text" {
			ms = append(ms, core.Message{Role: "system", Content: b.Text})
		}
	}
	if len(ms) == n {
		ms = append(ms, core.Message{Role: "system"})
	}
	return ms
}

// Request markers ParseMessages leaves in req.Metadata with
// Hooks.ClaudeCode (the NUL prefix keeps them apart from metadata.user_id).
const (
	// MetaThinkingDisplay holds thinking.display when it blanks the
	// thinking text ("omitted" or "updates").
	MetaThinkingDisplay = "\x00anth-thinking-display"
	// MetaContextManagement is set when the request has context_management.
	MetaContextManagement = "\x00anth-context-management"
)

// claudeCodeForm reads the fields of the Claude Code form (Hooks.ClaudeCode).
// thinking.type "adaptive" asks for reasoning like "enabled", without a
// budget: output_config.effort sizes it (spec 007).
func claudeCodeForm(req *core.Request, wr *messagesRequest) {
	if t := wr.Thinking; t != nil && t.Type == "adaptive" {
		req.Reasoning = core.ReasoningRequest{Requested: true, Effort: effortOf(wr.OutputConfig)}
		var display string
		if json.Unmarshal(t.Display, &display) == nil && (display == "omitted" || display == "updates") {
			setMeta(req, MetaThinkingDisplay, display)
		}
	}
	if len(wr.ContextManagement) > 0 && string(wr.ContextManagement) != "null" {
		setMeta(req, MetaContextManagement, "1")
	}
}

// effortOf is output_config.effort ("" when absent).
func effortOf(raw json.RawMessage) string {
	var oc struct {
		Effort string `json:"effort"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &oc) != nil {
		return ""
	}
	return oc.Effort
}

func setMeta(req *core.Request, k, v string) {
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[k] = v
}

// ForRequest returns the hooks that write the answer to req: h itself, or,
// when ParseMessages left a marker (Hooks.ClaudeCode), a copy whose Shape
// also writes the Claude Code form as the real does (fixtures
// testdata/real/anthropic/cc-shape/, task 441): with thinking.display
// "omitted" or "updates" the thinking block carries no text (only the
// signature); with context_management the message ends with
// context_management {"applied_edits": []} in the body, null in
// message_start and the object after the usage of message_delta. stream
// tells message_start from the body.
func (h *Hooks) ForRequest(req *core.Request, stream bool) *Hooks {
	if req == nil || len(req.Metadata) == 0 {
		return h
	}
	blank := req.Metadata[MetaThinkingDisplay] != ""
	cm := req.Metadata[MetaContextManagement] != ""
	if !blank && !cm {
		return h
	}
	c := *h
	base := h.Shape
	c.Shape = func(p Part, o oai.Obj) oai.Obj {
		if base != nil {
			o = base(p, o)
		}
		switch p {
		case PartBlock:
			if t, _ := o.Get("type"); blank && t == "thinking" {
				o.Set("thinking", "")
			}
		case PartMessage:
			if cm && stream {
				o.Set("context_management", nil)
			} else if cm {
				o.Set("context_management", appliedEdits())
			}
		case PartEvent:
			if t, _ := o.Get("type"); cm && t == "message_delta" {
				o.Set("context_management", appliedEdits())
			}
		}
		return o
	}
	return &c
}

// appliedEdits is the context_management of the answer: no edit applied
// (the simulated conversation never reaches a clearing threshold).
func appliedEdits() oai.Obj {
	return oai.O("applied_edits", []any{})
}

// NewRequestStream is NewStream with the hooks of ForRequest; with
// thinking.display "omitted" or "updates" the thinking block gets one empty
// thinking_delta before its signature, as the real sends it, whatever
// reasoning the engine generates (and counts).
func NewRequestStream(h *Hooks, w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	s := NewStream(h.ForRequest(req, true), w, req, rl)
	if req != nil && req.Metadata[MetaThinkingDisplay] != "" {
		return &blankThinking{Stream: s}
	}
	return s
}

// blankThinking writes the reasoning of a thinking block as one empty
// delta.
type blankThinking struct {
	*Stream
	sent bool
}

func (b *blankThinking) ReasoningDelta(string) error {
	if b.sent && b.open == "thinking" {
		return nil
	}
	b.sent = true
	return b.Stream.ReasoningDelta("")
}

func flattenSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" {
				b.WriteString(bl.Text) // joined like the message blocks
			}
		}
		return b.String()
	}
	return ""
}

func stopReason(fr core.FinishReason) string {
	switch fr {
	case core.FinishLength:
		return "max_tokens"
	case core.FinishToolCalls:
		return "tool_use"
	case core.FinishRefusal, core.FinishContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// InputTokens is Anthropic's usage.input_tokens: the prompt tokens neither
// read from nor written to the cache. The real API reports the three parts
// separately (input + cache_creation + cache_read = whole prompt), while
// core.Usage.PromptTokens counts the whole prompt.
func InputTokens(u core.Usage) int {
	return max(0, u.PromptTokens-u.CachedTokens-u.CacheWriteTokens)
}
