// Package oai implements the OpenAI Chat Completions dialect shared by the
// openai, openrouter and opencode drivers: parsing, JSON responses, SSE
// chunks and the error envelope. Drivers customise it through Hooks.
package oai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// chatRequest is the wire shape of POST /v1/chat/completions.
type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	Tools               []wireTool        `json:"tools"`
	Functions           []wireFunction    `json:"functions"` // legacy
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	FunctionCall        json.RawMessage   `json:"function_call"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Stream              bool              `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Reasoning       json.RawMessage `json:"reasoning"` // OpenRouter {effort,max_tokens,exclude}
	N               *int            `json:"n"`
	ResponseFormat  *struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	} `json:"response_format"`
	Temperature *float64          `json:"temperature"`
	Seed        *int64            `json:"seed"`
	Metadata    map[string]string `json:"metadata"`
	User        string            `json:"user"`
	ServiceTier string            `json:"service_tier"`
	Provider    json.RawMessage   `json:"provider"` // OpenRouter
	Models      []string          `json:"models"`   // OpenRouter fallbacks
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	FunctionCall *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function_call"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio json.RawMessage `json:"input_audio"`
}

// ParseChat converts a Chat Completions body into a core.Request.
func ParseChat(body []byte, h driver.Headers, driverName string) (*core.Request, *core.Error) {
	var wr chatRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "We could not parse the JSON body of your request. (HINT: This likely means you aren't using your HTTP library correctly. The OpenAI API expects a JSON payload, but what was sent was not valid JSON.)"}
	}
	if wr.Model == "" {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "you must provide a model parameter", Param: "model"}
	}
	if len(wr.Messages) == 0 {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "[] is too short - 'messages'", Param: "messages"}
	}
	req := &core.Request{Driver: driverName, Endpoint: core.EndpointChat, Model: wr.Model, Stream: wr.Stream, Raw: body, Received: time.Now(), Metadata: wr.Metadata, Temperature: wr.Temperature, Seed: wr.Seed}
	if wr.StreamOptions != nil {
		req.IncludeUsage = wr.StreamOptions.IncludeUsage
	}
	if wr.MaxCompletionTokens != nil {
		req.MaxTokens = *wr.MaxCompletionTokens
	} else if wr.MaxTokens != nil {
		req.MaxTokens = *wr.MaxTokens
	}
	if wr.N != nil {
		req.N = *wr.N
	}
	req.ParallelToolCalls = wr.ParallelToolCalls
	if wr.ReasoningEffort != "" {
		req.Reasoning = core.ReasoningRequest{Requested: true, Effort: wr.ReasoningEffort}
	}
	if len(wr.Reasoning) > 0 {
		var r struct {
			Effort    string `json:"effort"`
			MaxTokens int    `json:"max_tokens"`
			Enabled   *bool  `json:"enabled"`
			Exclude   bool   `json:"exclude"`
		}
		if json.Unmarshal(wr.Reasoning, &r) == nil {
			// ADR 0022: enabled: false or effort: "none" turn reasoning off;
			// exclude: true still reasons (and bills) but hides the text.
			on := (r.Enabled == nil || *r.Enabled) && r.Effort != "none"
			req.Reasoning = core.ReasoningRequest{Requested: on, Effort: r.Effort, BudgetTokens: r.MaxTokens, Exclude: on && r.Exclude}
		}
	}
	if wr.ResponseFormat != nil {
		req.ResponseFormat = wr.ResponseFormat.Type
		if wr.ResponseFormat.Type == "json_schema" && len(wr.ResponseFormat.JSONSchema) > 0 {
			var js struct {
				Schema json.RawMessage `json:"schema"`
			}
			if json.Unmarshal(wr.ResponseFormat.JSONSchema, &js) == nil {
				req.ResponseSchema = js.Schema
			}
		}
	}
	if wr.User != "" {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		req.Metadata["user"] = wr.User
	}
	for i, raw := range wr.Messages {
		var m wireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("Invalid value at 'messages[%d]'", i), Param: fmt.Sprintf("messages[%d]", i)}
		}
		msg := core.Message{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("Invalid value: '%s'. Supported values are: 'system', 'assistant', 'user', 'function', 'tool', and 'developer'.", m.Role), Param: fmt.Sprintf("messages[%d].role", i)}
		}
		if m.Role == "developer" {
			msg.Role = "system"
		}
		if m.Role == "function" {
			msg.Role = "tool"
		}
		text, images, audio := flattenContent(m.Content)
		msg.Content, msg.ImageParts, msg.AudioParts = text, images, audio
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if m.FunctionCall != nil {
			msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{ID: "call_legacy", Name: m.FunctionCall.Name, Arguments: m.FunctionCall.Arguments})
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range wr.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("Invalid value: '%s'. Supported values are: 'function'.", t.Type), Param: "tools[0].type"}
		}
		if t.Function.Name == "" {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "Missing required parameter: 'tools[0].function.name'.", Param: "tools[0].function.name"}
		}
		req.Tools = append(req.Tools, core.ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	for _, f := range wr.Functions {
		req.Tools = append(req.Tools, core.ToolDef{Name: f.Name, Description: f.Description, Parameters: f.Parameters})
	}
	req.ToolChoice = parseToolChoice(wr.ToolChoice)
	if req.ToolChoice.Mode == "" && len(wr.FunctionCall) > 0 {
		req.ToolChoice = parseToolChoice(wr.FunctionCall)
	}
	if req.ToolChoice.Mode == "" {
		req.ToolChoice.Mode = core.ToolChoiceAuto
	}
	if req.ToolChoice.Mode == core.ToolChoiceNamed {
		found := false
		for _, t := range req.Tools {
			if t.Name == req.ToolChoice.Name {
				found = true
			}
		}
		if !found {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("Invalid value for 'tool_choice': no tool named '%s' was provided in 'tools'.", req.ToolChoice.Name), Param: "tool_choice"}
		}
	}
	return req, nil
}

func parseToolChoice(raw json.RawMessage) core.ToolChoice {
	if len(raw) == 0 {
		return core.ToolChoice{}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			return core.ToolChoice{Mode: core.ToolChoiceNone}
		case "required", "any":
			return core.ToolChoice{Mode: core.ToolChoiceRequired}
		default:
			return core.ToolChoice{Mode: core.ToolChoiceAuto}
		}
	}
	var o struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &o) == nil {
		name := o.Function.Name
		if name == "" {
			name = o.Name
		}
		if name != "" {
			return core.ToolChoice{Mode: core.ToolChoiceNamed, Name: name}
		}
	}
	return core.ToolChoice{Mode: core.ToolChoiceAuto}
}

func flattenContent(raw json.RawMessage) (text string, images, audio int) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", 0, 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0, 0
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text":
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			case "image_url", "input_image", "image":
				images++
			case "input_audio", "audio":
				audio++
			}
		}
		return b.String(), images, audio
	}
	return string(raw), 0, 0
}

// completionsRequest is the legacy /v1/completions body.
type completionsRequest struct {
	Model         string          `json:"model"`
	Prompt        json.RawMessage `json:"prompt"`
	MaxTokens     *int            `json:"max_tokens"`
	Stream        bool            `json:"stream"`
	Echo          bool            `json:"echo"`
	N             *int            `json:"n"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// ParseCompletions converts a legacy completions body.
func ParseCompletions(body []byte, driverName string) (*core.Request, *core.Error) {
	var wr completionsRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "We could not parse the JSON body of your request."}
	}
	if wr.Model == "" {
		return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "you must provide a model parameter", Param: "model"}
	}
	req := &core.Request{Driver: driverName, Endpoint: core.EndpointCompletions, Model: wr.Model, Stream: wr.Stream, Raw: body, Received: time.Now()}
	if wr.MaxTokens != nil {
		req.MaxTokens = *wr.MaxTokens
	}
	if wr.StreamOptions != nil {
		req.IncludeUsage = wr.StreamOptions.IncludeUsage
	}
	var p string
	if json.Unmarshal(wr.Prompt, &p) != nil {
		var ps []string
		if json.Unmarshal(wr.Prompt, &ps) == nil {
			p = strings.Join(ps, "\n")
		} else {
			p = string(wr.Prompt)
		}
	}
	req.Messages = []core.Message{{Role: "user", Content: p}}
	req.ToolChoice.Mode = core.ToolChoiceNone
	if wr.Echo {
		req.Metadata = map[string]string{"echo": "true"}
	}
	return req, nil
}
