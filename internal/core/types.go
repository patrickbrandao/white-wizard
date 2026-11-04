// Package core holds the provider-neutral request/response model shared by
// every driver, the engine and the scenario motor. Drivers translate their
// wire formats to and from these types; nothing here knows about JSON dialects.
package core

import (
	"encoding/json"
	"time"
)

// Endpoint identifies which logical API the request arrived on.
type Endpoint string

const (
	EndpointChat        Endpoint = "chat"        // OpenAI /v1/chat/completions
	EndpointCompletions Endpoint = "completions" // OpenAI legacy /v1/completions
	EndpointResponses   Endpoint = "responses"   // OpenAI /v1/responses
	EndpointMessages    Endpoint = "messages"    // Anthropic /v1/messages
	EndpointModels      Endpoint = "models"
	EndpointCountTokens Endpoint = "count_tokens"
)

// Message is one turn of the conversation, flattened to text plus tool data.
type Message struct {
	Role        string       // system | user | assistant | tool
	Content     string       // text content (multimodal parts are reduced to text)
	ToolCalls   []ToolCall   // assistant turn: calls it made
	ToolCallID  string       // tool turn (OpenAI): which call this result answers
	ToolResults []ToolResult // user turn (Anthropic): tool_result blocks
	Name        string
	ImageParts  int // number of image inputs (counted, never inspected)
	AudioParts  int
}

// ToolResult is the outcome a harness sends back for a tool call.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// ToolCall is an assistant's invocation of a tool. Arguments is a JSON object
// encoded as a string, exactly as OpenAI's wire format carries it.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolDef is a tool the client offers. Parameters is the raw JSON Schema.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolChoiceMode mirrors OpenAI/Anthropic semantics.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required" // Anthropic "any"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

// ToolChoice is the normalised tool_choice.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// ReasoningRequest carries what the client asked about thinking.
type ReasoningRequest struct {
	Requested    bool   // client explicitly asked (reasoning_effort / thinking.enabled)
	Effort       string // low | medium | high | minimal | ""
	BudgetTokens int    // Anthropic budget_tokens, 0 = unset
	// Exclude: the reasoning is generated and counted (usage, cost, TTFT)
	// but its text is not returned (OpenRouter reasoning.exclude, ADR 0022).
	Exclude bool
}

// Request is the normalised inbound request.
type Request struct {
	Driver   string
	Endpoint Endpoint
	Model    string
	// System is the system prompt outside the messages (Anthropic system,
	// Gemini systemInstruction). Chat Completions and Responses keep theirs
	// in Messages only; SystemPrompt reads both (task 245).
	System     string
	Messages   []Message
	Tools      []ToolDef
	ToolChoice ToolChoice
	// ParallelToolCalls nil means "unset" (defaults to true).
	ParallelToolCalls *bool
	MaxTokens         int
	Stream            bool
	IncludeUsage      bool
	Reasoning         ReasoningRequest
	N                 int
	ResponseFormat    string // "" | json_object | json_schema
	ResponseSchema    json.RawMessage
	Temperature       *float64
	Seed              *int64
	Metadata          map[string]string
	// Headers carries the request headers the engine cares about, lower-cased.
	Headers map[string]string
	// ClientIP is the resolved source address (trusted proxy aware).
	ClientIP string
	// Raw is the original body for drivers that need to look at unusual fields.
	Raw []byte
	// Received is when the request arrived.
	Received time.Time
	// RequestID is assigned by the engine.
	RequestID string
}

// LastUserMessage returns the text of the most recent user turn.
func (r *Request) LastUserMessage() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// SystemPrompt is the request's system prompt wherever the API puts it:
// System, or else the first message when it is a system one (Chat
// Completions system/developer, Responses instructions).
func (r *Request) SystemPrompt() string {
	if r.System != "" {
		return r.System
	}
	if len(r.Messages) > 0 && r.Messages[0].Role == "system" {
		return r.Messages[0].Content
	}
	return ""
}

// Header returns a header value by lower-cased name.
func (r *Request) Header(name string) string {
	if r.Headers == nil {
		return ""
	}
	return r.Headers[name]
}

// FinishReason is the provider-neutral stop reason.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishRefusal       FinishReason = "refusal"
	FinishError         FinishReason = "error"
)

// Usage is token accounting, provider-neutral.
type Usage struct {
	PromptTokens     int
	CompletionTokens int // includes reasoning tokens
	ReasoningTokens  int
	CachedTokens     int // read from the prompt cache
	CacheWriteTokens int // written to the prompt cache (all TTLs)
	// CacheWrite1hTokens is the part of CacheWriteTokens written with the
	// 1 h TTL (Anthropic cache_creation.ephemeral_1h_input_tokens); the
	// rest is the 5 min TTL.
	CacheWrite1hTokens int
	// Cost is in micro-dollars (1e-6 USD) for drivers that expose cost.
	CostMicroUSD int64
	// PromptCostPicoUSD and CompletionCostPicoUSD are the same cost in
	// pico-dollars (1e-12 USD), split as OpenRouter's cost_details reports
	// it (prompt with cache reads and writes, completion with reasoning):
	// a few tokens cost less than one micro-dollar (task 246). Both zero
	// when the engine did not price the usage.
	PromptCostPicoUSD     int64
	CompletionCostPicoUSD int64
}

// TotalTokens is prompt + completion.
func (u Usage) TotalTokens() int { return u.PromptTokens + u.CompletionTokens }

// Response is the fully materialised answer for non-streaming replies and the
// summary the engine builds while streaming.
type Response struct {
	ID           string
	Model        string
	Created      time.Time
	Reasoning    string
	Content      string
	ToolCalls    []ToolCall
	FinishReason FinishReason
	Usage        Usage
	// Extra lets scenarios or drivers attach dialect-specific fields.
	Extra map[string]any
	// Headers are additional response headers (x-wwz-*, rate limit, etc.).
	Headers map[string]string
}

// RateLimitState is what the limiter reports back for headers.
type RateLimitState struct {
	RequestsLimit     int
	RequestsRemaining int
	RequestsReset     time.Duration
	TokensLimit       int
	TokensRemaining   int
	TokensReset       time.Duration
	InputTokensLimit  int
	InputTokensRemain int
	OutputTokensLimit int
	OutputTokensRem   int
	// ProjectTokens* are the tokens unit of the project when it declares
	// its own tpm (projects[].limits.tpm): the openai driver emits them as
	// x-ratelimit-{limit,remaining,reset}-project-tokens (spec 010, task
	// 234). Zero ProjectTokensLimit means no project limit.
	ProjectTokensLimit  int
	ProjectTokensRemain int
	ProjectTokensReset  time.Duration
	Present             bool
}

// Principal is what a driver could identify about the caller.
type Principal struct {
	Key     string // API key as presented (hashed before storage)
	KeyID   string // config label or hash prefix
	Account string
	Project string
	User    string
	IP      string
	Tier    string
	Driver  string
	// AuthNone is true when the driver did not require credentials.
	AuthNone bool
}
