package engine

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

func decodeB64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// inputBreakdownHeader is the x-wwz-input-breakdown header name (I16).
const inputBreakdownHeader = "x-wwz-input-breakdown"

// inputBreakdown returns the x-wwz-input-breakdown value (spec 013, I16, ADR
// 0027) when usage.extended is on and the prompt was already counted; cached
// comes from the planned usage, when there is one. Body and usage never
// change. The same numbers are kept in r.sentBreakdown for the request event
// (fields.input_breakdown, ADR 0032): every caller puts the value on the
// response.
func (r *request) inputBreakdown() (string, bool) {
	if r.cfg == nil || !r.cfg.Usage.Extended || r.prompt.Total == 0 {
		return "", false
	}
	b := r.prompt
	if r.resp != nil {
		b.Cached = r.resp.Usage.CachedTokens
	}
	r.sentBreakdown = &limits.InputBreakdown{
		System: b.System, ToolSchemas: b.ToolSchemas, User: b.User, Assistant: b.Assistant, ToolCalls: b.ToolCalls,
		ToolResults: b.ToolResults, Images: b.Images, Cached: b.Cached, Total: b.Total,
	}
	return string(b.AppendJSON(make([]byte, 0, 160))), true
}

// setInputBreakdown stamps x-wwz-input-breakdown on the planned response.
// The engine calls it once usage.cached_tokens is final (after the latency
// plan decides a simulated cache hit).
func (r *request) setInputBreakdown() {
	if r.resp == nil {
		return
	}
	if v, ok := r.inputBreakdown(); ok {
		r.resp.Headers[inputBreakdownHeader] = v
	}
}

// plan decides the full content of the response before any byte is sent, so
// usage, finish reason and latency can be computed up front.
func (e *Engine) plan(r *request) {
	req, g, rng := r.req, r.gen, r.rng
	now := time.Now()
	conv := driver.ConventionsOf(r.in.Driver)
	model := r.model.ID
	if conv.ModelVersion {
		model = r.model.ResponseModel() // ADR 0049: the dated snapshot, never the alias
	}
	resp := &core.Response{ID: r.responseID(now), Model: model, Created: now, Headers: map[string]string{}}
	if conv.RequestIDHeader != "" {
		resp.Headers[conv.RequestIDHeader] = requestIDHeader(req.RequestID)
	}
	if e.podName != "" {
		resp.Headers["x-wwz-pod"] = e.podName
	}
	maxOut := req.MaxTokens
	if maxOut <= 0 || (r.model.MaxOutput > 0 && maxOut > r.model.MaxOutput) {
		maxOut = r.model.MaxOutput
		if maxOut <= 0 {
			maxOut = 4096
		}
	}

	// --- reasoning -----------------------------------------------------
	reasoningTokens := 0
	canReason := r.model.Capabilities.Reasoning == nil || *r.model.Capabilities.Reasoning
	if r.scn != nil {
		if r.scn.Reasoning != "" {
			resp.Reasoning = r.scn.Reasoning
		} else if r.scn.ReasoningTokens > 0 {
			resp.Reasoning = r.text.Reasoning(rng, r.scn.ReasoningTokens)
		}
	} else if canReason && req.Reasoning.Requested {
		// Spec 007 R7: reasoning needs a capable model AND a client that asked
		// (reasoning_effort, OpenRouter/OpenCode reasoning, Anthropic
		// thinking.type=enabled); the profile only sizes it.
		n := generate.SampleReasoningTokens(rng, g.ReasoningTokens, req.Reasoning.Effort, req.Reasoning.BudgetTokens)
		if n > 0 {
			resp.Reasoning = r.text.Reasoning(rng, n)
		}
	}
	if resp.Reasoning != "" {
		reasoningTokens = r.tk.Count(resp.Reasoning)
		if g.RedactedProbability > 0 && rng.Chance(g.RedactedProbability) {
			if resp.Extra == nil {
				resp.Extra = map[string]any{}
			}
			resp.Extra["redacted_thinking"] = true
		}
	}

	// --- tool calls ----------------------------------------------------
	var toolCalls []core.ToolCall
	switch {
	case r.overrides.hasTools:
		toolCalls = r.overrides.toolCalls
		for i := range toolCalls {
			if strings.HasPrefix(toolCalls[i].ID, "call_hdr_") {
				toolCalls[i].ID = r.toolCallID(i)
			}
		}
	case r.scn != nil:
		toolCalls = r.scn.ToolCalls
		for i := range toolCalls {
			// tool_call_id_format: native leaves the id to the driver.
			if toolCalls[i].ID == "" {
				toolCalls[i].ID = r.toolCallID(i)
			}
		}
	case len(req.Tools) > 0 && req.ToolChoice.Mode != "none":
		prob := 30.0
		if g.ToolCalls != nil {
			prob = g.ToolCalls.Probability
		}
		if r.overrides.hasToolProb {
			prob = r.overrides.toolProb
		}
		force := req.ToolChoice.Mode == "required" || req.ToolChoice.Mode == "named"
		if force || rng.Chance(prob) {
			toolCalls = e.randomToolCalls(r)
		}
	}
	// Tool misbehaviour from chaos or step.
	breakKind, breakTool := r.chaos.ToolBreak, ""
	if breakKind != "" && !e.toolBreakDriverAllowed(r) {
		breakKind, r.chaos.ToolBreak = "", ""
	}
	if tm := r.errs.ToolMisbehavior; breakKind != "" && tm != nil {
		breakTool = tm.Tool
	}
	if r.scn != nil && r.scn.ToolMisbehavior != "" {
		breakKind, breakTool = r.scn.ToolMisbehavior, ""
	}
	if c, ok := config.CanonicalToolMisbehavior(breakKind); ok {
		breakKind = c
	}
	if breakKind != "" && len(req.Tools) > 0 {
		var broke bool
		toolCalls, broke = e.breakTools(r, toolCalls, breakKind, breakTool)
		if !broke && breakTool != "" {
			// errors.tool_misbehavior.tool: no call of that tool, no breakage.
			breakKind, r.chaos.ToolBreak = "", ""
		}
	} else if breakTool != "" {
		breakKind, r.chaos.ToolBreak = "", ""
	}
	resp.ToolCalls = toolCalls

	// --- content -------------------------------------------------------
	contentTokens := 0
	switch {
	case r.overrides.response != "":
		resp.Content = r.overrides.response
	case r.scn != nil:
		resp.Content = r.scn.Content
		if resp.Content == "" && r.scn.ContentTokens > 0 {
			resp.Content = r.text.Text(rng, r.scn.ContentTokens)
		}
	case len(toolCalls) > 0:
		if g.TextBeforeToolProbability > 0 && rng.Chance(g.TextBeforeToolProbability) {
			resp.Content = r.text.Text(rng, rng.Between(8, 40))
		}
	default:
		target := generate.SampleOutputTokens(rng, g.OutputTokens, maxOut)
		resp.Content = e.content(r, target)
	}
	if resp.Content != "" {
		contentTokens = r.tk.Count(resp.Content)
	}
	// Enforce max_tokens: reasoning + content + tool args share the budget.
	argTokens := 0
	for _, tc := range toolCalls {
		argTokens += r.tk.Count(tc.Arguments)
	}
	budget := maxOut
	hitLength := false
	if reasoningTokens+contentTokens+argTokens > budget && r.scn == nil && r.overrides.response == "" {
		if reasoningTokens >= budget {
			resp.Reasoning = generate.TrimToTokens(r.tk, resp.Reasoning, budget)
			reasoningTokens = r.tk.Count(resp.Reasoning)
			resp.Content, contentTokens = "", 0
			hitLength = true
		} else if contentTokens > 0 {
			resp.Content = generate.TrimToTokens(r.tk, resp.Content, budget-reasoningTokens-argTokens)
			contentTokens = r.tk.Count(resp.Content)
			hitLength = true
		}
	}

	// Output that reaches max_tokens ends with length, as on the real
	// provider, even when nothing had to be trimmed (spec 007, ADR 0049).
	if budget > 0 && reasoningTokens+contentTokens+argTokens >= budget {
		hitLength = true
	}

	// --- finish reason ---------------------------------------------------
	switch {
	case breakKind == "length_mid_tool":
		// Arguments cut mid-JSON only make sense with length, even when the
		// scenario step names another finish_reason.
		resp.FinishReason = core.FinishLength
	case r.scn != nil && r.scn.FinishReason != "":
		resp.FinishReason = r.scn.FinishReason
	case len(toolCalls) > 0:
		resp.FinishReason = core.FinishToolCalls
	case hitLength:
		resp.FinishReason = core.FinishLength
	default:
		resp.FinishReason = core.FinishStop
	}
	// Moderation.
	modMode := ""
	if r.chaos.Moderation != nil {
		modMode = r.chaos.Moderation.Mode
	}
	if r.scn != nil && r.scn.Moderation != "" {
		modMode = r.scn.Moderation
	}
	if r.overrides.contentFilter {
		modMode = "finish_reason"
	}
	switch modMode {
	case "finish_reason":
		resp.FinishReason = core.FinishContentFilter
		resp.Content = generate.TrimToTokens(r.tk, resp.Content, contentTokens/2)
		contentTokens = r.tk.Count(resp.Content)
	case "refusal":
		resp.FinishReason = core.FinishRefusal
		resp.ToolCalls = nil
		if r.decl.RefusalEmpty {
			// A refusal before any output: content [], output_tokens 0.
			resp.Content, resp.Reasoning, contentTokens, reasoningTokens = "", "", 0, 0
			break
		}
		resp.Content = "I can't help with that request."
		contentTokens = r.tk.Count(resp.Content)
		if resp.Extra == nil {
			resp.Extra = map[string]any{}
		}
		resp.Extra["refusal"] = resp.Content
	}
	if breakKind == "empty_response" {
		resp.Content, resp.ToolCalls, contentTokens = "", nil, 0
		resp.FinishReason = core.FinishStop
	}
	if breakKind == "reasoning_only" {
		resp.Content, resp.ToolCalls, contentTokens = "", nil, 0
		if resp.Reasoning == "" {
			resp.Reasoning = r.text.Reasoning(rng, 60)
			reasoningTokens = r.tk.Count(resp.Reasoning)
		}
		resp.FinishReason = core.FinishLength
	}

	// --- usage -----------------------------------------------------------
	argTokens = 0
	for _, tc := range resp.ToolCalls {
		argTokens += r.tk.Count(tc.Arguments) + 4
	}
	resp.Usage = core.Usage{PromptTokens: r.prompt.Total, CompletionTokens: reasoningTokens + contentTokens + argTokens, ReasoningTokens: reasoningTokens}
	// usage.cost (OpenRouter, OpenCode) bills reasoning as output, hidden
	// or not (ADR 0022); the store's Finish uses the same formula.
	setCost(r.model.Pricing, &resp.Usage)

	// --- scenario extras -------------------------------------------------
	if r.scn != nil {
		resp.Headers["x-wwz-scenario"] = r.scn.Scenario
		resp.Headers["x-wwz-step"] = r.scn.Step
		resp.Headers["x-wwz-attempt"] = strconv.Itoa(r.scn.Attempt)
		for k, v := range r.scn.Headers {
			resp.Headers[k] = v
		}
		r.stall = r.scn.Stall
		r.cutAfter = r.scn.CutAfterTokens
	}
	if r.seed != 0 {
		resp.Headers["x-wwz-seed"] = strconv.FormatInt(r.seed, 10)
	}
	r.resp = resp
}

// content produces the main text for the configured generation mode.
func (e *Engine) content(r *request, target int) string {
	g, rng, req := r.gen, r.rng, r.req
	if target <= 0 {
		target = 1
	}
	switch g.Mode {
	case "echo":
		return req.LastUserMessage()
	case "lorem":
		return r.text.Lorem(rng, target)
	case "template":
		return renderTemplate(g.Template, r)
	case "json":
		return e.jsonContent(r, target)
	case "dataset":
		// The text is cut at max_tokens by plan, like random (task 381).
		if ds, err := datasetFor(r.cfg, g.DatasetPath); err == nil {
			text, _ := ds.Response(rng, req.Messages)
			return text
		}
		// Validated at boot and reload; a file gone since then answers random.
	}
	if req.ResponseFormat == "json_object" || req.ResponseFormat == "json_schema" {
		return e.jsonContent(r, target)
	}
	if len(g.Sentences) > 0 {
		gen := generate.NewGenerator(r.tk, g.Sentences)
		return gen.Text(rng, target)
	}
	return r.text.Text(rng, target)
}

// datasetSet holds the datasets of one configuration, so a request in mode
// dataset costs a pointer compare and a map lookup; a new configuration
// (reload) checks each file once more (generate.LoadDataset re-reads only a
// changed file).
type datasetSet struct {
	cfg *config.Config
	m   sync.Map // path → *generate.Dataset
}

var datasets atomic.Pointer[datasetSet]

func datasetFor(cfg *config.Config, path string) (*generate.Dataset, error) {
	s := datasets.Load()
	if s == nil || s.cfg != cfg {
		s = &datasetSet{cfg: cfg}
		datasets.Store(s)
	}
	if d, ok := s.m.Load(path); ok {
		return d.(*generate.Dataset), nil
	}
	d, err := generate.LoadDataset(path)
	if err != nil {
		return nil, err
	}
	s.m.Store(path, d)
	return d, nil
}

func (e *Engine) jsonContent(r *request, target int) string {
	if len(r.req.ResponseSchema) > 0 {
		if s, err := toolargs.Parse(r.req.ResponseSchema); err == nil {
			if b, err := toolargs.Generate(s, r.rng, argOptions(r.gen)); err == nil {
				return string(b)
			}
		}
	}
	m := map[string]any{"answer": r.text.Text(r.rng, target/2+1), "confidence": float64(r.rng.IntN(100)) / 100, "items": []string{generate.LoremWords[r.rng.IntN(len(generate.LoremWords))], generate.LoremWords[r.rng.IntN(len(generate.LoremWords))]}}
	b, _ := json.Marshal(m)
	return string(b)
}

func renderTemplate(tpl string, r *request) string {
	last := r.req.LastUserMessage()
	rep := strings.NewReplacer("{{model}}", r.model.ID, "{{last_user_message}}", last, "{{request_id}}", r.req.RequestID, "{{prompt_tokens}}", strconv.Itoa(r.prompt.Total))
	return rep.Replace(tpl)
}

// randomToolCalls picks tools and generates schema-valid arguments.
func (e *Engine) randomToolCalls(r *request) []core.ToolCall {
	req, rng := r.req, r.rng
	opts := argOptions(r.gen)
	var picks []core.ToolDef
	if req.ToolChoice.Mode == "named" {
		for _, t := range req.Tools {
			if t.Name == req.ToolChoice.Name {
				picks = append(picks, t)
			}
		}
	}
	if len(picks) == 0 {
		picks = append(picks, req.Tools[rng.IntN(len(req.Tools))])
		extra := 0.0
		if r.gen.ToolCalls != nil {
			extra = r.gen.ToolCalls.ExtraCallProbability
		}
		parallel := req.ParallelToolCalls == nil || *req.ParallelToolCalls
		for parallel && len(picks) < 8 && rng.Chance(extra) {
			picks = append(picks, req.Tools[rng.IntN(len(req.Tools))])
		}
	}
	out := make([]core.ToolCall, 0, len(picks))
	for i, t := range picks {
		args := "{}"
		if len(t.Parameters) > 0 {
			if s, err := toolargs.Parse(t.Parameters); err == nil {
				if b, err := toolargs.Generate(s, rng, opts); err == nil {
					args = string(b)
				}
			}
		}
		out = append(out, core.ToolCall{ID: r.toolCallID(i), Name: t.Name, Arguments: args})
	}
	return out
}

func shortID(r *generate.RNG) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// toolBreakDriverAllowed applies errors.tool_misbehavior.drivers: an empty
// list allows every driver.
func (e *Engine) toolBreakDriverAllowed(r *request) bool {
	tm := r.errs.ToolMisbehavior
	if tm == nil || len(tm.Drivers) == 0 {
		return true
	}
	if r.in.Driver == nil {
		return false
	}
	name := r.in.Driver.Name()
	for _, d := range tm.Drivers {
		if d == name {
			return true
		}
	}
	return false
}

// breakTools applies a misbehaviour to the tool calls (creating one if
// needed). With tool set, only the first call of that tool is broken; broke
// is false when there is no such call (the calls are returned untouched).
func (e *Engine) breakTools(r *request, calls []core.ToolCall, kind, tool string) (out []core.ToolCall, broke bool) {
	req, rng := r.req, r.rng
	if len(calls) == 0 {
		calls = e.randomToolCalls(r)
	}
	idx := 0
	if tool != "" {
		idx = -1
		for i := range calls {
			if calls[i].Name == tool {
				idx = i
				break
			}
		}
	}
	if len(calls) == 0 || idx < 0 {
		return calls, false
	}
	switch kind {
	case "unknown_tool":
		calls[idx].Name += "_v2"
		return calls, true
	case "duplicate_id":
		dup := calls[idx]
		return append(calls, dup), true
	case "length_mid_tool":
		a := calls[idx].Arguments
		if len(a) > 3 {
			calls[idx].Arguments = a[:len(a)/2]
		}
		return calls[idx : idx+1], true
	case "empty_response", "reasoning_only":
		return calls, true
	}
	var def *core.ToolDef
	for i := range req.Tools {
		if req.Tools[i].Name == calls[idx].Name {
			def = &req.Tools[i]
		}
	}
	if def == nil || len(def.Parameters) == 0 {
		return calls, true
	}
	s, err := toolargs.Parse(def.Parameters)
	if err != nil {
		return calls, true
	}
	calls[idx].Arguments = string(toolargs.Break(s, []byte(calls[idx].Arguments), kind, rng))
	return calls, true
}
