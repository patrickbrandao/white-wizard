// Package loadgen is the core of wwz-load (spec 015): it builds OpenAI and
// Anthropic requests, sends them with fasthttp streaming the response body,
// parses SSE incrementally and measures TTFT, ITL, E2E, tokens and errors.
// Percentiles live in the hist subpackage (ADR 0024).
package loadgen

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
)

// Protocol is the dialect of the body, the auth and the SSE parser.
type Protocol string

// Protocols. OpenRouter and OpenCode Go speak OpenAI (with another path).
const (
	OpenAI    Protocol = "openai"
	Anthropic Protocol = "anthropic"
)

// AnthropicVersion is the anthropic-version header sent with Anthropic.
const AnthropicVersion = "2023-06-01"

// ParseProtocol validates a --protocol value.
func ParseProtocol(s string) (Protocol, error) {
	switch p := Protocol(strings.ToLower(s)); p {
	case OpenAI, Anthropic:
		return p, nil
	}
	return "", fmt.Errorf("protocol %q: want openai or anthropic", s)
}

// DefaultPath is the inference path of p.
func (p Protocol) DefaultPath() string {
	if p == Anthropic {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

// ToolCall is a tool call received from (or replayed to) the model.
// Arguments is the JSON text of the arguments.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one conversation message, independent of the dialect.
//
//   - system / user: Content.
//   - assistant: Content, Reasoning (OpenAI reasoning_content, Anthropic
//     thinking with Signature) and ToolCalls, replayed as received.
//   - tool: the result of ToolCallID in Content; IsError marks a failed tool
//     (Anthropic is_error: true; OpenAI "error: " prefix).
type Message struct {
	Role       string
	Content    string
	Reasoning  string
	Signature  string
	ToolCalls  []ToolCall
	ToolCallID string
	IsError    bool
}

// Tool is a tool declared to the model. Schema is a JSON Schema object; nil
// means {"type":"object","additionalProperties":true}.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Header is an extra request header.
type Header struct {
	Key, Value string
}

// Request is one inference request.
type Request struct {
	Protocol  Protocol
	URL       string // target + path
	Key       string // API key; "" sends no auth header
	Model     string
	Stream    bool
	MaxTokens int
	Messages  []Message
	Tools     []Tool
	Headers   []Header
	// Seed ≠ 0 sends x-wwz-seed.
	Seed int64
	// Capture keeps the assistant message (text, reasoning, tool calls) in
	// Result.Message, for multi-turn conversations.
	Capture bool
	// Body, when set, is sent as is (precomputed with BuildBody to avoid
	// re-encoding the same request).
	Body []byte
}

var anySchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)

type oaFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   *string         `json:"arguments,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function oaFunc `json:"function"`
}

type oaMsg struct {
	Role             string       `json:"role"`
	Content          *string      `json:"content"`
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string       `json:"tool_call_id,omitempty"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function oaFunc `json:"function"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaBody struct {
	Model         string      `json:"model"`
	Messages      []oaMsg     `json:"messages"`
	MaxTokens     int         `json:"max_tokens,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
	StreamOptions *streamOpts `json:"stream_options,omitempty"`
	Tools         []oaTool    `json:"tools,omitempty"`
}

type anBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anMsg struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anBody struct {
	Model     string   `json:"model"`
	MaxTokens int      `json:"max_tokens"`
	System    string   `json:"system,omitempty"`
	Messages  []anMsg  `json:"messages"`
	Stream    bool     `json:"stream,omitempty"`
	Tools     []anTool `json:"tools,omitempty"`
}

func schemaOf(t Tool) json.RawMessage {
	if len(t.Schema) == 0 {
		return anySchema
	}
	return t.Schema
}

// BuildBody encodes the JSON body of r in its protocol.
func (r *Request) BuildBody() ([]byte, error) {
	if r.Protocol == Anthropic {
		return json.Marshal(r.anthropicBody())
	}
	return json.Marshal(r.openAIBody())
}

func (r *Request) openAIBody() oaBody {
	b := oaBody{Model: r.Model, MaxTokens: r.MaxTokens, Stream: r.Stream, Messages: make([]oaMsg, 0, len(r.Messages))}
	if r.Stream {
		b.StreamOptions = &streamOpts{IncludeUsage: true}
	}
	for _, t := range r.Tools {
		b.Tools = append(b.Tools, oaTool{Type: "function", Function: oaFunc{Name: t.Name, Description: t.Description, Parameters: schemaOf(t)}})
	}
	for _, m := range r.Messages {
		om := oaMsg{Role: m.Role, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		content := m.Content
		if m.Role == "tool" && m.IsError {
			content = "error: " + content
		}
		if content != "" || len(m.ToolCalls) == 0 {
			om.Content = &content
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			om.ToolCalls = append(om.ToolCalls, oaToolCall{ID: tc.ID, Type: "function", Function: oaFunc{Name: tc.Name, Arguments: &args}})
		}
		b.Messages = append(b.Messages, om)
	}
	return b
}

// toolInput turns tool call arguments into a JSON object for tool_use.input.
func toolInput(args string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) == nil && obj != nil {
		return json.RawMessage(args)
	}
	return json.RawMessage(`{}`)
}

func (r *Request) anthropicBody() anBody {
	b := anBody{Model: r.Model, MaxTokens: r.MaxTokens, Stream: r.Stream, Messages: make([]anMsg, 0, len(r.Messages))}
	for _, t := range r.Tools {
		b.Tools = append(b.Tools, anTool{Name: t.Name, Description: t.Description, InputSchema: schemaOf(t)})
	}
	var system []string
	for _, m := range r.Messages {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "tool":
			c := m.Content
			blk := anBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: &c, IsError: m.IsError}
			// Consecutive tool results go in one user message, in order.
			if n := len(b.Messages); n > 0 && b.Messages[n-1].Role == "user" && b.Messages[n-1].Content[0].Type == "tool_result" {
				b.Messages[n-1].Content = append(b.Messages[n-1].Content, blk)
				continue
			}
			b.Messages = append(b.Messages, anMsg{Role: "user", Content: []anBlock{blk}})
		case "assistant":
			var blocks []anBlock
			if m.Reasoning != "" || m.Signature != "" {
				th, sig := m.Reasoning, m.Signature
				blocks = append(blocks, anBlock{Type: "thinking", Thinking: &th, Signature: &sig})
			}
			if m.Content != "" {
				c := m.Content
				blocks = append(blocks, anBlock{Type: "text", Text: &c})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: toolInput(tc.Arguments)})
			}
			if len(blocks) == 0 {
				empty := ""
				blocks = append(blocks, anBlock{Type: "text", Text: &empty})
			}
			b.Messages = append(b.Messages, anMsg{Role: "assistant", Content: blocks})
		default:
			c := m.Content
			b.Messages = append(b.Messages, anMsg{Role: m.Role, Content: []anBlock{{Type: "text", Text: &c}}})
		}
	}
	b.System = strings.Join(system, "\n\n")
	return b
}

// fill writes method, URL, headers and body into req.
func (r *Request) fill(req *fasthttp.Request) error {
	body := r.Body
	if body == nil {
		var err error
		if body, err = r.BuildBody(); err != nil {
			return err
		}
	}
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI(r.URL)
	req.Header.SetContentType("application/json")
	if r.Protocol == Anthropic {
		if r.Key != "" {
			req.Header.Set("x-api-key", r.Key)
		}
		req.Header.Set("anthropic-version", AnthropicVersion)
	} else if r.Key != "" {
		req.Header.Set("Authorization", "Bearer "+r.Key)
	}
	if r.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if r.Seed != 0 {
		req.Header.Set("x-wwz-seed", strconv.FormatInt(r.Seed, 10))
	}
	for _, h := range r.Headers {
		req.Header.Set(h.Key, h.Value)
	}
	req.SetBodyRaw(body)
	return nil
}

// ParseHeader parses a --header value "k: v".
func ParseHeader(s string) (Header, error) {
	k, v, ok := strings.Cut(s, ":")
	k = strings.TrimSpace(k)
	if !ok || k == "" || strings.ContainsAny(k, " \t") {
		return Header{}, fmt.Errorf("header %q: want 'name: value'", s)
	}
	return Header{Key: k, Value: strings.TrimSpace(v)}, nil
}
