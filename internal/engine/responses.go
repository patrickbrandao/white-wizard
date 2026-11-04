package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// State of the Responses API (spec 004 "Responses API", ADR 0055,
// cache/valkey/README.md 2.5): every response with store other than false
// leaves a compact record (account, model, message and token counts, the
// prompt-cache prefix) when its body is sent or its stream ends; previous_response_id
// reads it before Check, adds the recorded conversation to the prompt and
// counts its prefix as already written to the account's prompt cache.

// respState is the Responses API state of one request.
type respState struct {
	reg   limits.ResponseRegistry // nil: the store keeps no records
	store bool                    // store != false: the response is recorded
	opts  pcRawOpts               // prompt_cache_key and retention
	// instr is the token count of the instructions message, which the next
	// response does not inherit; newMsgs counts the input messages without it.
	instr   int
	newMsgs int
	prev    *limits.ResponseRecord
	put     bool
}

// respRawBody is the part of the body the state needs.
type respRawBody struct {
	pcRawOpts
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	Instructions       json.RawMessage `json:"instructions"`
}

// previousNotFound is the real answer to an unknown, foreign, expired or
// unstored previous_response_id (previous_response_not_found,
// previous_response_unstored).
func previousNotFound(id string) *core.Error {
	return &core.Error{
		Kind: core.ErrInvalidRequest, Message: "Previous response with id '" + id + "' not found.",
		Param: "previous_response_id", Code: "previous_response_not_found",
	}
}

// responsesPrevious prepares the state of a Responses request once its
// prompt is counted: it reads previous_response_id (before Check) and adds
// the recorded conversation to the prompt (27 = 10 + 7 + 7 + 3 in
// previous_response_id). It returns the error of a record not found.
func (e *Engine) responsesPrevious(ctx context.Context, r *request) *core.Error {
	if r.req.Endpoint != core.EndpointResponses {
		return nil
	}
	var body respRawBody
	_ = json.Unmarshal(r.req.Raw, &body) // the driver parsed it already
	st := &respState{store: body.Store == nil || *body.Store, opts: body.pcRawOpts, newMsgs: len(r.req.Messages)}
	var instr string
	if json.Unmarshal(body.Instructions, &instr) == nil && instr != "" {
		st.instr = r.tk.Count(instr) + msgOverhead(r.tk, "system", "")
		st.newMsgs--
	}
	st.reg, _ = e.Store.(limits.ResponseRegistry)
	r.rs = st
	if body.PreviousResponseID == "" {
		return nil
	}
	if st.reg != nil {
		rec, err := st.reg.ResponseGet(ctx, body.PreviousResponseID, r.account(), time.Now())
		if err != nil {
			e.debug("response registry lookup failed", err)
		}
		st.prev = rec
	}
	if st.prev == nil {
		return previousNotFound(body.PreviousResponseID)
	}
	r.prompt.User += st.prev.Tokens
	r.prompt.Total += st.prev.Tokens
	return nil
}

// chained reports whether r continues a recorded response.
func (r *request) chained() bool { return r.rs != nil && r.rs.prev != nil }

// outputTokens is what the response weighs as an assistant message of the
// next turn: the template overhead, the text and each tool call.
func outputTokens(r *request, resp *core.Response) int {
	n := msgOverhead(r.tk, "assistant", "") + r.tk.Count(resp.Content)
	for _, tc := range resp.ToolCalls {
		n += r.tk.Count(tc.Name) + r.tk.Count(tc.Arguments) + 8
	}
	return n
}

// responsesUsage is the output count of the Responses API: the generated
// tokens plus the end token, one more for a text answer when functions were
// offered, capped at max_output_tokens, which a response reaching it does
// not complete. Observed: default_store "Hello there!" = 3 + 1;
// strict_default_open_schema "Ok." = 2 + 2; function_call_stream and
// echo_fields: argument tokens + 1 (one call; several calls not
// conferred); strict_default_no_additional and incomplete_stream: 16 of
// 16, incomplete. Tool calls carry no other overhead, unlike Chat.
func (r *request) responsesUsage() {
	if r.rs == nil || r.resp == nil {
		return
	}
	resp := r.resp
	out := resp.Usage.ReasoningTokens + r.tk.Count(resp.Content) + 1
	for _, tc := range resp.ToolCalls {
		out += r.tk.Count(tc.Arguments)
	}
	if len(resp.ToolCalls) == 0 && len(r.req.Tools) > 0 {
		out++
	}
	if max := r.req.MaxTokens; max > 0 && out >= max {
		out = max
		if resp.FinishReason == core.FinishStop {
			resp.FinishReason = core.FinishLength
		}
	}
	resp.Usage.CompletionTokens = out
	setCost(r.model.Pricing, &resp.Usage)
}

// respPrefix identifies a recorded conversation in the prompt cache: the
// chain of the previous record and this response, under the driver, the
// model and the routing key. It stands for the hash of section 2.4 at the
// end of the conversation, which the wwz cannot recompute without the text
// (not conferred: the real rule for chained calls has no fixture yet).
func respPrefix(drv, model, key, prev, id string) string {
	h := sha256.New()
	for _, s := range []string{"wwz-resp/1", drv, model, key, prev, id} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// respCacheHash is the prompt-cache entry of a recorded conversation as
// read with routing key key: another key misses, as in the cache of 2.4.
func respCacheHash(prefix, key string) string {
	sum := sha256.Sum256([]byte("wwz-resp-pc/1\x00" + prefix + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// cutBelow is the longest cacheable prefix of n tokens under rule: the
// largest min + k·granularity not above n (n itself with granularity 0).
func cutBelow(n int, rule pcRule) int {
	if n < max(rule.min, 1) {
		return 0
	}
	if rule.gran <= 0 {
		return n
	}
	return rule.min + (n-rule.min)/rule.gran*rule.gran
}

// respCacheTail is how much of the end of a prompt the Responses API keeps
// out of its prompt cache: one step of the rule. Observed three times on
// 2026-10-31 (cache_automatic_prefix and previous_response_cache): prompts
// of 1 695, 1 697 and 1 707 tokens are read back up to 1 536, never 1 664,
// while Chat Completions reads 1 280 of 1 400 (task 162).
func respCacheTail(r *request, rule pcRule) int {
	if r.req.Endpoint != core.EndpointResponses {
		return 0
	}
	return max(rule.gran, 0)
}

// chainedCacheLookup is the prompt cache of a chained request: the
// recorded conversation is a prefix the account already wrote, read up to
// the largest cut it covers (less the tail the Responses API does not
// cache: 1 536 of 1 698 in previous_response_cache) when its entry is
// alive, and renewed (or written) when the response sends its first byte.
func (e *Engine) chainedCacheLookup(ctx context.Context, r *request) {
	cacher, ok := e.Store.(limits.PromptCacher)
	prev := r.rs.prev
	if !ok || r.model == nil || prev.Prefix == "" {
		return
	}
	rule := pcRuleFor(r.decl, r.model, r.req.Endpoint)
	n := cutBelow(prev.Tokens-respCacheTail(r, rule), rule)
	if rule.kind != pcAutomatic || n == 0 {
		return
	}
	h := respCacheHash(prev.Prefix, r.rs.opts.PromptCacheKey)
	st := &pcState{cacher: cacher, account: pcAccount(r)}
	hit, err := cacher.PromptCacheLookup(ctx, st.account, []string{h}, time.Now())
	if err != nil {
		e.debug("prompt cache lookup failed", err)
		hit = -1
	}
	if hit >= 0 {
		st.read = min(n, r.prompt.Total)
	}
	ttl, life := pcAutoTTL(rule.style, &r.rs.opts)
	st.commit = []limits.PromptCacheEntry{{Hash: h, TTL: ttl, MaxLife: life}}
	r.pc = st
}

// responsesPut records the response (store != false) with its prefix in
// the account's prompt cache: a body when it sends its first byte, a stream
// when it ends, with the output it delivered (ADR 0070). A failure only
// goes to the debug log (README 2.5).
func (e *Engine) responsesPut(ctx context.Context, r *request) {
	st := r.rs
	if st == nil || !st.store || st.reg == nil || st.put || r.resp == nil || r.model == nil {
		return
	}
	st.put = true
	drv := r.in.Driver.Name()
	out := r.resp // the reply delivered: a finish_length cut keeps what was sent
	if r.delivered != nil {
		out = r.delivered
	}
	rec := limits.ResponseRecord{
		Account: r.account(), Driver: drv, Model: r.model.ID, Messages: st.newMsgs + 1,
		Tokens: r.prompt.Total - generate.ReplyPriming - r.prompt.ToolSchemas - st.instr + outputTokens(r, out),
		At:     time.Now(),
	}
	prevPrefix := ""
	if st.prev != nil {
		rec.Messages += st.prev.Messages
		prevPrefix = st.prev.Prefix
	}
	rule := pcRuleFor(r.decl, r.model, r.req.Endpoint)
	if rule.kind == pcAutomatic {
		rec.Prefix = respPrefix(drv, r.model.ID, st.opts.PromptCacheKey, prevPrefix, r.resp.ID)
		if cacher, ok := e.Store.(limits.PromptCacher); ok && cutBelow(rec.Tokens, rule) > 0 {
			ttl, life := pcAutoTTL(rule.style, &st.opts)
			entry := limits.PromptCacheEntry{Hash: respCacheHash(rec.Prefix, st.opts.PromptCacheKey), TTL: ttl, MaxLife: life}
			if _, err := cacher.PromptCacheCommit(ctx, pcAccount(r), []limits.PromptCacheEntry{entry}, time.Now()); err != nil {
				e.debug("prompt cache commit failed", err)
			}
		}
	}
	if err := st.reg.ResponsePut(ctx, r.resp.ID, rec); err != nil {
		e.debug("response registry put failed", err)
	}
}

// modelNotFound is the error of a model the driver does not serve. A model
// past retires_at is flagged, so the Responses API can answer "has been
// deprecated" (deprecated_model; oai.ExtraModelRetired).
func (e *Engine) modelNotFound(m *config.ModelConfig) *core.Error {
	er := &core.Error{Kind: core.ErrModelNotFound}
	if m != nil && m.Retired(e.now()) {
		er.Extra = map[string]any{"model_retired": true}
	}
	return er
}
