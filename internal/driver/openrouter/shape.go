package openrouter

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The forms below were recorded against the real OpenRouter on 2026-10-31
// (spec 006 16.7, task 174; testdata/real/openrouter/shape/).

// serviceTier is the service_tier OpenRouter reports for an OpenAI
// upstream (body and the last chunk).
const serviceTier = "default"

// bodyPrefix is the keep-alive whitespace OpenRouter writes before every
// non-streaming JSON body (observed: a newline, nine spaces, a newline).
const bodyPrefix = "\n         \n"

// keepAliveComment is the SSE comment OpenRouter sends while it waits for
// the upstream.
const keepAliveComment = "OPENROUTER PROCESSING"

// exposeHeaders is access-control-expose-headers of every answer.
const exposeHeaders = "X-Generation-Id,X-Provider-Name,request-id,cf-ray"

// nativeFinish is native_finish_reason: the finish reason in the
// upstream's vocabulary. OpenAI models answer through the Responses API
// (completed, max_output_tokens; conferred). Anthropic models use the
// Messages stop_reason (not conferred); any other upstream repeats
// finish_reason (not conferred).
func nativeFinish(req *core.Request, fr core.FinishReason) any {
	if fr == "" {
		return nil
	}
	author := ""
	if req != nil {
		author, _, _ = strings.Cut(strings.ToLower(req.Model), "/")
	}
	switch author {
	case "openai":
		switch fr {
		case core.FinishLength:
			return "max_output_tokens"
		case core.FinishStop, core.FinishToolCalls:
			return "completed"
		}
	case "anthropic":
		switch fr {
		case core.FinishStop:
			return "end_turn"
		case core.FinishLength:
			return "max_tokens"
		case core.FinishToolCalls:
			return "tool_use"
		}
	}
	return string(fr)
}

// costDetails splits the cost of u in the real usage form.
func costDetails(u core.Usage) (float64, oai.Obj) {
	cost, prompt, completion := costSplit(u)
	return cost, oai.O(
		"upstream_inference_cost", cost,
		"upstream_inference_prompt_cost", prompt,
		"upstream_inference_completions_cost", completion,
	)
}

// costSplit is the cost of u in dollars and its prompt and completion
// parts. The engine prices in pico-dollars, split (task 246): 13 tokens
// at $0.10/M are 0.0000013, as the real reports. A usage with only the
// whole micro-dollars (no pico split) is split by the token counts.
func costSplit(u core.Usage) (cost, prompt, completion float64) {
	p, c := u.PromptCostPicoUSD, u.CompletionCostPicoUSD
	if p == 0 && c == 0 && u.CostMicroUSD > 0 {
		nano := u.CostMicroUSD * 1000 // nano × tokens stays inside int64
		if total := int64(u.TotalTokens()); total > 0 {
			p = nano * int64(u.PromptTokens) / total * 1000
		}
		c = nano*1000 - p
	}
	// One division each: the float is the closest to the decimal figure,
	// which encoding/json writes like JavaScript (0.0000013, 8e-7).
	return float64(p+c) / 1e12, float64(p) / 1e12, float64(c) / 1e12
}

// chatUsageExtras completes the usage object of Chat Completions; shapeChat
// puts it in the real order.
func chatUsageExtras(_ *core.Request, _ *core.Response, u core.Usage) map[string]any {
	cost, details := costDetails(u)
	return map[string]any{
		"cost":                      cost,
		"is_byok":                   false,
		"cost_details":              details,
		"prompt_tokens_details":     oai.O("cached_tokens", u.CachedTokens, "cache_write_tokens", u.CacheWriteTokens, "audio_tokens", 0, "video_tokens", 0),
		"completion_tokens_details": oai.O("reasoning_tokens", u.ReasoningTokens, "image_tokens", 0, "audio_tokens", 0),
	}
}

// shapeChat is the Chat Completions form of OpenRouter (oai.Hooks.Shape).
func shapeChat(p oai.Part, o oai.Obj) oai.Obj {
	switch p {
	case oai.PartChat:
		if v, _ := o.Get("object"); v != "chat.completion" {
			return o.Reorder("id", "object", "created", "model", "provider")
		}
		o = o.Reorder("id", "object", "created", "model", "provider")
		return insertAfter(o, "provider", oai.Field{Key: "system_fingerprint"}, oai.Field{Key: "service_tier", Val: serviceTier})
	case oai.PartChoice:
		if _, text := o.Get("text"); text {
			return o
		}
		return o.Reorder("index", "logprobs", "finish_reason", "native_finish_reason", "message")
	case oai.PartMessage:
		return shapeMessage(o)
	case oai.PartUsage:
		return o.Reorder("prompt_tokens", "completion_tokens", "total_tokens", "cost", "is_byok", "prompt_tokens_details", "cost_details", "completion_tokens_details")
	case oai.PartChunk:
		o = o.Reorder("id", "object", "created", "model", "provider")
		if _, usage := o.Get("usage"); usage {
			o = insertAfter(o, "provider", oai.Field{Key: "service_tier", Val: serviceTier})
		}
		return o
	case oai.PartChunkChoice:
		if _, text := o.Get("text"); text {
			return o
		}
		o.Delete("logprobs")
		return o.Reorder("index", "delta", "finish_reason", "native_finish_reason")
	case oai.PartDelta:
		return shapeDelta(o)
	}
	return o
}

// shapeMessage: role, content, refusal, reasoning (null without
// reasoning), then tool_calls with each call as {type, index, id,
// function}.
func shapeMessage(o oai.Obj) oai.Obj {
	_, reasoning := o.Get("reasoning")
	_, other := o.Get("reasoning_content")
	if !reasoning && !other {
		o = insertAfter(o, "refusal", oai.Field{Key: "reasoning"})
	}
	if v, ok := o.Get("tool_calls"); ok {
		calls, _ := v.([]any)
		out := make([]any, 0, len(calls))
		for i, c := range calls {
			call, _ := c.(oai.Obj)
			id, _ := call.Get("id")
			fn, _ := call.Get("function")
			out = append(out, oai.O("type", "function", "index", i, "id", id, "function", fn))
		}
		o.Delete("tool_calls")
		o = append(o, oai.Field{Key: "tool_calls", Val: out})
	}
	return o.Reorder("role", "content", "refusal", "reasoning", "reasoning_details")
}

// shapeDelta: every delta carries content and role "assistant" first;
// content is null next to tool calls and "" on the closing chunks.
func shapeDelta(o oai.Obj) oai.Obj {
	o.Delete("role")
	o.Delete("refusal")
	content, ok := o.Get("content")
	if !ok {
		content = ""
		if _, tools := o.Get("tool_calls"); tools {
			content = nil
		}
	}
	o.Delete("content")
	return append(oai.O("content", content, "role", "assistant"), o...)
}

// insertAfter puts fs right after key (at the end when key is absent).
func insertAfter(o oai.Obj, key string, fs ...oai.Field) oai.Obj {
	for i, f := range o {
		if f.Key == key {
			out := make(oai.Obj, 0, len(o)+len(fs))
			out = append(out, o[:i+1]...)
			out = append(out, fs...)
			return append(out, o[i+1:]...)
		}
	}
	return append(o, fs...)
}

// --- Messages (/api/v1/messages, Anthropic upstream) ---

// messagesUsageExtras completes the usage object of a message.
func messagesUsageExtras(_ *core.Request, _ *core.Response, u core.Usage) map[string]any {
	cost, details := costDetails(u)
	w1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	return map[string]any{
		"output_tokens_details": oai.O("thinking_tokens", u.ReasoningTokens),
		"cache_creation":        oai.O("ephemeral_5m_input_tokens", u.CacheWriteTokens-w1h, "ephemeral_1h_input_tokens", w1h),
		"inference_geo":         nil,
		"server_tool_use":       nil,
		"speed":                 "standard",
		"cost":                  cost,
		"is_byok":               false,
		"cost_details":          details,
	}
}

// shapeMessages is the Messages form of OpenRouter (anth.Hooks.Shape).
func shapeMessages(p anth.Part, o oai.Obj) oai.Obj {
	switch p {
	case anth.PartBlock:
		if v, _ := o.Get("type"); v == "text" {
			o.Set("citations", []any{})
		}
		return o
	case anth.PartMessage:
		o = append(o, oai.Field{Key: "container"}, oai.Field{Key: "stop_details"}, oai.Field{Key: "context_management"}, oai.Field{Key: "provider", Val: providerName})
		return o.Reorder("id", "type", "role", "container", "content", "model", "stop_reason", "stop_details", "stop_sequence", "usage", "context_management", "provider")
	case anth.PartUsage:
		return o.Reorder("input_tokens", "output_tokens", "output_tokens_details", "cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation",
			"inference_geo", "server_tool_use", "service_tier", "speed", "cost", "is_byok", "cost_details")
	case anth.PartDeltaUsage:
		o.Delete("inference_geo")
		return o
	case anth.PartEvent:
		switch v, _ := o.Get("type"); v {
		case "message_start":
			msg, _ := o.Get("message")
			m, _ := msg.(oai.Obj)
			m.Delete("context_management")
			if u, ok := m.Get("usage"); ok {
				usage, _ := u.(oai.Obj)
				usage.Set("output_tokens_details", nil)
				usage.Delete("cost")
				usage.Delete("is_byok")
				usage.Delete("cost_details")
				m.Set("usage", usage)
			}
			o.Set("message", m)
		case "message_delta":
			d, _ := o.Get("delta")
			delta, _ := d.(oai.Obj)
			reason, _ := delta.Get("stop_reason")
			seq, _ := delta.Get("stop_sequence")
			o.Set("delta", oai.O("container", nil, "stop_details", nil, "stop_reason", reason, "stop_sequence", seq))
			o.Set("context_management", nil)
		}
	}
	return o
}
