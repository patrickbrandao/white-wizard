package opencode

import (
	"math"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The forms below were recorded against the real OpenCode Go on 2026-10-31
// (spec 006 16.7, task 175; testdata/real/opencode/shape/). The gateway
// routes each session to one upstream; the Chat Completions form here is
// the one of spec 006 16.7 (ids gw-…, compact usage details). Two other
// upstreams were seen for the same model (bare 32-hex ids with OpenAI's
// full usage details in alphabetical order and role in every delta; and
// chatcmpl-<16 hex> with logprobs: null); the wwz does not reproduce them.

// billingOf is the billing the engine settled for req (interface (d),
// after Extra Usage). A request without one (the driver used without the
// engine) is billed by its base: go with the default allowance on a Go
// base, balance on the others.
func billingOf(req *core.Request) driver.Billing {
	if b, ok := driver.BillingOf(req); ok {
		return b
	}
	if req != nil && strings.Contains(req.Header("x-wwz-path"), "/go/") {
		return driver.Billing{Source: driver.BillingGo, AllowanceFactor: defaultAllowance(req.Model)}
	}
	return driver.Billing{Source: driver.BillingBalance}
}

// dollars rounds a dollar amount to 1e-8, as the gateway prints it.
func dollars(v float64) float64 { return math.Round(v*1e8) / 1e8 }

// listCost is the list cost of u in dollars: the engine's pico-dollar
// split when it priced the usage (task 246; 35 tokens at $0.15/M and 13 at
// $0.60/M are 0.00001305, as the real prints), else the whole
// micro-dollars.
func listCost(u core.Usage) float64 {
	if pico := u.PromptCostPicoUSD + u.CompletionCostPicoUSD; pico > 0 {
		return float64(pico) / 1e12
	}
	return float64(u.CostMicroUSD) / 1e6
}

// opencodeUsage is usage.cost and usage.opencode: billing source (the
// request's billing: free, balance, go or go-plus; ADR 0072, item 7), list
// cost, the allowance debited from the quota (go and go-plus only, list
// cost × the allowance factor) and the token split. cost is the list cost
// on balance and 0 on the others. tokens.input leaves out the
// cache reads and writes; tokens.output is completion_tokens (reasoning
// included, conferred). reasoning is false on the Messages route, which
// reports 0 there even with thinking (conferred).
func opencodeUsage(req *core.Request, u core.Usage, reasoning bool) (float64, oai.Obj) {
	list := dollars(listCost(u))
	w1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	r := 0
	if reasoning {
		r = u.ReasoningTokens
	}
	tokens := oai.O(
		"input", max(u.PromptTokens-u.CachedTokens-u.CacheWriteTokens, 0), "output", u.CompletionTokens, "reasoning", r,
		"cache_read", u.CachedTokens, "cache_write_5m", u.CacheWriteTokens-w1h, "cache_write_1h", w1h,
	)
	b := billingOf(req)
	switch {
	case b.Subscription():
		return 0, oai.O("billing_source", b.Source, "list_cost", list, "allowance", dollars(list*b.AllowanceFactor), "tokens", tokens)
	case b.Source == driver.BillingFree:
		return 0, oai.O("billing_source", b.Source, "list_cost", list, "tokens", tokens)
	}
	return list, oai.O("billing_source", driver.BillingBalance, "list_cost", list, "tokens", tokens)
}

// chatUsageExtras adds cost and opencode after the OpenAI fields.
func chatUsageExtras(req *core.Request, _ *core.Response, u core.Usage) map[string]any {
	cost, oc := opencodeUsage(req, u, true)
	return map[string]any{"cost": cost, "opencode": oc}
}

// messagesUsageExtras adds cost and opencode to the Messages usage.
func messagesUsageExtras(req *core.Request, _ *core.Response, u core.Usage) map[string]any {
	cost, oc := opencodeUsage(req, u, false)
	return map[string]any{"cost": cost, "opencode": oc}
}

// shapeChat is the Chat Completions form (oai.Hooks.Shape): no logprobs,
// no refusal (unless there is one), the reduced usage details.
func shapeChat(p oai.Part, o oai.Obj) oai.Obj {
	switch p {
	case oai.PartChoice, oai.PartChunkChoice:
		if _, text := o.Get("text"); text {
			return o
		}
		o.Delete("logprobs")
		return o.Reorder("index", "finish_reason", "message", "delta")
	case oai.PartMessage:
		if v, ok := o.Get("refusal"); ok && v == nil {
			o.Delete("refusal")
		}
		o.Delete("annotations")
		return o.Reorder("role", "content", "reasoning_content", "tool_calls")
	case oai.PartDelta:
		if v, ok := o.Get("refusal"); ok && v == nil {
			o.Delete("refusal")
		}
		return o
	case oai.PartUsage:
		cached, _ := o.Get("prompt_tokens_details")
		reasoning, _ := o.Get("completion_tokens_details")
		if c, ok := cached.(oai.Obj); ok {
			v, _ := c.Get("cached_tokens")
			o.Set("prompt_tokens_details", oai.O("cached_tokens", v))
		}
		if r, ok := reasoning.(oai.Obj); ok {
			v, _ := r.Get("reasoning_tokens")
			o.Set("completion_tokens_details", oai.O("reasoning_tokens", v))
		}
		return o
	}
	return o
}

// shapeMessages is the Messages form of the gateway's translation
// (anth.Hooks.Shape): id, type, role, stop_reason, stop_sequence, model,
// content, usage; usage without service_tier and, in message_start,
// without cost and opencode.
func shapeMessages(p anth.Part, o oai.Obj) oai.Obj {
	switch p {
	case anth.PartMessage:
		return o.Reorder("id", "type", "role", "stop_reason", "stop_sequence", "model", "content", "usage")
	case anth.PartUsage:
		o.Delete("service_tier")
		return o.Reorder("input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "cost", "opencode")
	case anth.PartEvent:
		if v, _ := o.Get("type"); v != "message_start" {
			return o
		}
		msg, _ := o.Get("message")
		m, _ := msg.(oai.Obj)
		if u, ok := m.Get("usage"); ok {
			usage, _ := u.(oai.Obj)
			usage.Delete("cost")
			usage.Delete("opencode")
			m.Set("usage", usage)
		}
		o.Set("message", m)
	}
	return o
}

// streamContentType is the Content-Type of both stream routes (no charset).
const streamContentType = "text/event-stream"

// successHeaders are the headers of a 200, chat and Messages: the log id
// and x-zen-model (the model of the answer).
func successHeaders(req *core.Request, resp *core.Response, _ *core.RateLimitState) map[string]string {
	h := map[string]string{"x-opencode-log-id": newLogID(time.Now())}
	if resp != nil && resp.Model != "" {
		h["x-zen-model"] = resp.Model
	}
	if req != nil && req.Stream {
		h["Content-Type"] = streamContentType
	}
	return h
}
