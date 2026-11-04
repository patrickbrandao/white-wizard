package drivertest

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// created is the fixed creation time of every test response.
var created = time.Unix(1700000000, 0)

// minter returns the driver's IDMinter, if any.
func (s *suite) minter() (driver.IDMinter, bool) {
	m, ok := s.d.(driver.IDMinter)
	return m, ok
}

func (s *suite) requestID(k driver.Kind, token string) string {
	if m, ok := s.minter(); ok {
		return m.RequestID(k, token)
	}
	return "wwz-" + token
}

func (s *suite) responseID(k driver.Kind, seed string) string {
	if m, ok := s.minter(); ok {
		return m.ResponseID(k, generate.NewRNG(7, "drivertest", seed), created)
	}
	return "resp-" + seed
}

func (s *suite) toolCallID(k driver.Kind, seed string) string {
	if m, ok := s.minter(); ok {
		return m.ToolCallID(k, generate.NewRNG(7, "drivertest-tool", seed))
	}
	return "tool-" + seed
}

func (s *suite) newReq(k driver.Kind, stream bool) *core.Request {
	return &core.Request{
		Driver: s.d.Name(), Endpoint: endpointOf(k), Model: "model-a", Stream: stream,
		IncludeUsage: anthropicWire(k), RequestID: s.requestID(k, "req-1"),
		Headers: map[string]string{}, Received: created,
	}
}

type respCase struct {
	name      string
	content   string
	reasoning string
	tools     []core.ToolCall
	finish    core.FinishReason
}

func (s *suite) respCases(k driver.Kind) []respCase {
	cases := []respCase{
		{name: "text", content: "Hello there.", finish: core.FinishStop},
		{name: "length", content: "Cut", finish: core.FinishLength},
	}
	if k == driver.KindCompletions {
		return cases
	}
	return append(cases,
		respCase{name: "tool_calls", tools: []core.ToolCall{
			{ID: s.toolCallID(k, "a"), Name: "get_weather", Arguments: `{"city":"Paris"}`},
			{ID: s.toolCallID(k, "b"), Name: "get_time", Arguments: `{}`},
		}, finish: core.FinishToolCalls},
		respCase{name: "reasoning", reasoning: "Let me think.", content: "Answer.", finish: core.FinishStop},
		respCase{name: "refusal", content: "I can't help with that.", finish: core.FinishRefusal},
	)
}

func (s *suite) testResponse(t *testing.T) {
	for _, k := range s.surfaces() {
		s.run(t, string(k), func(t *testing.T) {
			for _, c := range s.respCases(k) {
				s.run(t, c.name, func(t *testing.T) { s.response(t, k, c) })
			}
		})
	}
}

func (s *suite) response(t *testing.T, k driver.Kind, c respCase) {
	req := s.newReq(k, false)
	resp := &core.Response{
		ID: s.responseID(k, "r1"), Model: "model-a", Created: created,
		Content: c.content, Reasoning: c.reasoning, ToolCalls: c.tools, FinishReason: c.finish,
		Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5, ReasoningTokens: 2, CachedTokens: 3, CacheWriteTokens: 2, CostMicroUSD: 1500},
	}
	w := core.NewMemWriter()
	if err := s.d.WriteResponse(w, req, resp, nil); err != nil {
		t.Fatal(err)
	}
	body := expectJSONResponse(t, w, 200)
	s.checkSuccessHeaders(t, k, w.Headers())
	if anthropicWire(k) {
		s.checkMessage(t, body, w.Headers(), req, resp)
		return
	}
	s.checkChatCompletion(t, k, body, resp)
}

// checkSuccessHeaders requires the surface's SuccessHeaders on a 200.
func (s *suite) checkSuccessHeaders(t *testing.T, k driver.Kind, h map[string]string) {
	t.Helper()
	for _, n := range s.surface(k).SuccessHeaders {
		if v, ok := header(h, n); !ok || v == "" {
			t.Errorf("header %s missing on a 200 (spec 006 16.7)", n)
		}
	}
}

// openAIFinish maps a finish reason to Chat Completions.
func openAIFinish(fr core.FinishReason) string {
	if fr == core.FinishRefusal {
		return "stop"
	}
	return string(fr)
}

// anthropicStop maps a finish reason to Messages stop_reason.
func anthropicStop(fr core.FinishReason) string {
	switch fr {
	case core.FinishLength:
		return "max_tokens"
	case core.FinishToolCalls:
		return "tool_use"
	case core.FinishRefusal, core.FinishContentFilter:
		return "refusal"
	}
	return "end_turn"
}

func (s *suite) checkChatCompletion(t *testing.T, k driver.Kind, body map[string]any, resp *core.Response) {
	t.Helper()
	sf := s.surface(k)
	expectPath(t, body, "id", resp.ID)
	expectPath(t, body, "created", created.Unix())
	expectPath(t, body, "model", resp.Model)
	expectPath(t, body, "choices.0.index", 0)
	expectPath(t, body, "choices.0.finish_reason", openAIFinish(resp.FinishReason))
	if sf.NativeFinishReason {
		expectPresent(t, body, "choices.0.native_finish_reason")
	}
	s.checkChatUsage(t, sf, body, "usage", resp.Usage)
	if sf.ContentFilter {
		checkPromptFilterResults(t, body)
		c0, _ := path(body, "choices.0")
		checkChoiceFilter(t, c0, resp.Content != "" && resp.FinishReason != core.FinishRefusal)
	}
	if k == driver.KindCompletions {
		expectPath(t, body, "object", "text_completion")
		expectPath(t, body, "choices.0.text", resp.Content)
		return
	}
	expectPath(t, body, "object", "chat.completion")
	expectPath(t, body, "choices.0.message.role", "assistant")
	msg, _ := path(body, "choices.0.message")
	if sf.ServiceTier != "" {
		expectPath(t, body, "service_tier", sf.ServiceTier)
	}
	if a, ok := path(body, "choices.0.message.annotations"); sf.Annotations {
		if l, isArr := a.([]any); !isArr || len(l) != 0 {
			t.Errorf("message.annotations = %v, want []", a)
		}
	} else if ok && sf.Envelope != EnvAny {
		t.Errorf("message.annotations present: %v", a)
	}
	switch {
	case resp.FinishReason == core.FinishRefusal:
		expectPath(t, msg, "refusal", resp.Content)
		expectPath(t, msg, "content", nil)
	case len(resp.ToolCalls) > 0 && resp.Content == "":
		expectPath(t, msg, "content", nil)
	default:
		expectPath(t, msg, "content", resp.Content)
	}
	tcs := arr(msg, "tool_calls")
	if len(tcs) != len(resp.ToolCalls) {
		t.Fatalf("tool_calls has %d entries, want %d", len(tcs), len(resp.ToolCalls))
	}
	for i, tc := range resp.ToolCalls {
		expectPath(t, tcs[i], "id", tc.ID)
		expectPath(t, tcs[i], "type", "function")
		expectPath(t, tcs[i], "function.name", tc.Name)
		expectPath(t, tcs[i], "function.arguments", tc.Arguments)
	}
	_, hasRC := path(msg, "reasoning_content")
	_, hasR := path(msg, "reasoning")
	hidden := sf.ReasoningField == "hidden" || (sf.ReasoningField == "" && !hasRC && !hasR)
	for _, f := range []string{"reasoning_content", "reasoning"} {
		if f == "reasoning" && sf.ReasoningNull && (resp.Reasoning == "" || hidden) {
			expectPath(t, msg, f, nil)
			continue
		}
		if resp.Reasoning == "" || hidden {
			expectAbsent(t, msg, f)
		}
	}
	if resp.Reasoning != "" && hidden {
		// ADR 0022: the text stays out of the body, inline tags included.
		if strings.Contains(str(msg, "content"), resp.Reasoning) {
			t.Errorf("hidden reasoning leaked into content: %v", msg)
		}
	}
	if resp.Reasoning != "" && !hidden {
		field := sf.ReasoningField
		if field == "" {
			field = "reasoning_content"
			if hasR {
				field = "reasoning"
			}
		}
		expectPath(t, msg, field, resp.Reasoning)
	}
	s.checkReasoningDetails(t, sf, msg, resp.Reasoning != "" && !hidden, resp.Reasoning)
}

// checkReasoningDetails checks reasoning_details[] on a message or delta:
// present with reasoning.text entries whose text joins to want when the
// surface emits it and the reasoning is visible, absent otherwise.
func (s *suite) checkReasoningDetails(t *testing.T, sf Surface, at any, visible bool, want string) {
	t.Helper()
	if !sf.ReasoningDetails || !visible {
		expectAbsent(t, at, "reasoning_details")
		return
	}
	details := arr(at, "reasoning_details")
	if len(details) == 0 {
		t.Errorf("reasoning_details missing or empty: %v", at)
		return
	}
	var got strings.Builder
	for _, d := range details {
		expectPath(t, d, "type", "reasoning.text")
		expectPresent(t, d, "format")
		expectPresent(t, d, "index")
		got.WriteString(str(d, "text"))
	}
	if got.String() != want {
		t.Errorf("reasoning_details text %q, want %q", got.String(), want)
	}
}

func (s *suite) checkChatUsage(t *testing.T, sf Surface, body any, at string, u core.Usage) {
	t.Helper()
	expectPath(t, body, at+".prompt_tokens", u.PromptTokens)
	expectPath(t, body, at+".completion_tokens", u.CompletionTokens)
	expectPath(t, body, at+".total_tokens", u.PromptTokens+u.CompletionTokens)
	expectPath(t, body, at+".prompt_tokens_details.cached_tokens", u.CachedTokens)
	expectPath(t, body, at+".completion_tokens_details.reasoning_tokens", u.ReasoningTokens)
	if sf.UsageCost {
		expectPresent(t, body, at+".cost")
	}
}

func (s *suite) checkMessage(t *testing.T, body map[string]any, h map[string]string, req *core.Request, resp *core.Response) {
	t.Helper()
	sf := s.surface(driver.KindMessages)
	expectPath(t, body, "id", resp.ID)
	expectPath(t, body, "type", "message")
	expectPath(t, body, "role", "assistant")
	expectPath(t, body, "model", resp.Model)
	expectPath(t, body, "stop_reason", anthropicStop(resp.FinishReason))
	expectPath(t, body, "stop_sequence", nil)
	if sf.RefusalDetails && resp.FinishReason == core.FinishRefusal {
		expectRefusalDetails(t, body, "stop_details")
	}
	// input_tokens excludes cache reads and writes; the three add up to the
	// whole prompt (Anthropic usage semantics).
	u := resp.Usage
	expectPath(t, body, "usage.input_tokens", u.PromptTokens-u.CachedTokens-u.CacheWriteTokens)
	expectPath(t, body, "usage.output_tokens", u.CompletionTokens)
	expectPath(t, body, "usage.cache_read_input_tokens", u.CachedTokens)
	expectPath(t, body, "usage.cache_creation_input_tokens", u.CacheWriteTokens)
	if sf.UsageCost {
		expectPresent(t, body, "usage.cost")
	}
	if sf.RequestIDHeader {
		expectHeader(t, h, "request-id", req.RequestID)
	}
	// Blocks in order: thinking, text, tool_use (spec 007).
	var want []string
	if resp.Reasoning != "" {
		want = append(want, "thinking")
	}
	if resp.Content != "" {
		want = append(want, "text")
	}
	for range resp.ToolCalls {
		want = append(want, "tool_use")
	}
	blocks := arr(body, "content")
	var got []string
	for _, b := range blocks {
		got = append(got, str(b, "type"))
	}
	if !eqStrings(got, want) {
		t.Fatalf("content block types %v, want %v", got, want)
	}
	ti := 0
	for _, b := range blocks {
		switch str(b, "type") {
		case "thinking":
			expectPath(t, b, "thinking", resp.Reasoning)
			sig, has := path(b, "signature")
			switch nosig := s.surface(driver.KindMessages).NoSignature; {
			case nosig && has:
				t.Errorf("thinking block with signature %v", sig)
			case !nosig && str(b, "signature") == "":
				t.Error("thinking block without signature")
			}
		case "text":
			expectPath(t, b, "text", resp.Content)
		case "tool_use":
			tc := resp.ToolCalls[ti]
			ti++
			expectPath(t, b, "id", tc.ID)
			expectPath(t, b, "name", tc.Name)
			in, _ := path(b, "input")
			if _, ok := in.(map[string]any); !ok {
				t.Errorf("tool_use.input must be an object, got %T", in)
			}
			expectPath(t, b, "input", mustDecode(tc.Arguments))
		}
	}
}

func mustDecode(s string) any {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// --- rate-limit headers ---

func (s *suite) testRateLimitHeaders(t *testing.T) {
	for _, k := range s.surfaces() {
		if s.surface(k).RateLimit == RLUnchecked {
			continue
		}
		s.run(t, string(k), func(t *testing.T) {
			s.run(t, "Response", func(t *testing.T) {
				w := core.NewMemWriter()
				resp := &core.Response{ID: s.responseID(k, "rl"), Model: "model-a", Created: created, Content: "x", FinishReason: core.FinishStop}
				if err := s.d.WriteResponse(w, s.newReq(k, false), resp, testRL()); err != nil {
					t.Fatal(err)
				}
				s.checkRateLimitHeaders(t, k, w.Headers())
			})
			s.run(t, "StreamStart", func(t *testing.T) {
				w := core.NewMemWriter()
				sw := s.d.StreamWriter(w, s.newReq(k, true), testRL())
				if err := sw.Start(&core.Response{ID: s.responseID(k, "rl"), Model: "model-a", Created: created}); err != nil {
					t.Fatal(err)
				}
				s.checkRateLimitHeaders(t, k, w.Headers())
			})
			s.run(t, "Error", func(t *testing.T) {
				w := core.NewMemWriter()
				s.d.WriteError(w, s.errReq(k), &core.Error{Kind: core.ErrServerError}, testRL())
				s.checkRateLimitHeaders(t, k, w.Headers())
			})
			s.run(t, "Absent", func(t *testing.T) {
				// No limiter state: no limit headers, or the default tier's
				// for the providers that always send them (ADR 0049).
				w := core.NewMemWriter()
				resp := &core.Response{ID: s.responseID(k, "rl"), Model: "model-a", Created: created, Content: "x", FinishReason: core.FinishStop}
				if err := s.d.WriteResponse(w, s.newReq(k, false), resp, nil); err != nil {
					t.Fatal(err)
				}
				if s.surface(k).RateLimitAlways {
					want := 6 // x-ratelimit-{limit,remaining,reset}-{requests,tokens}
					if s.surface(k).RateLimit == RLAnthropic {
						want = 12
					}
					if names := limitHeaderNames(w.Headers()); len(names) != want {
						t.Errorf("limit headers without limiter state: %v, want the %d of the default tier", names, want)
					}
					return
				}
				if names := limitHeaderNames(w.Headers()); len(names) > 0 {
					t.Errorf("limit headers without limiter state: %v", names)
				}
			})
		})
	}
}

var reGoDuration = regexp.MustCompile(`^([0-9]+h)?([0-9]+m)?([0-9]+(\.[0-9]+)?(ms|s))?$`)

func limitHeaderNames(h map[string]string) []string {
	var out []string
	for k := range h {
		l := strings.ToLower(k)
		if strings.HasPrefix(l, "anthropic-ratelimit-") ||
			(strings.HasPrefix(l, "x-ratelimit-") && (strings.HasSuffix(l, "-requests") || strings.HasSuffix(l, "-tokens"))) {
			out = append(out, k)
		}
	}
	return out
}

func (s *suite) checkRateLimitHeaders(t *testing.T, k driver.Kind, h map[string]string) {
	t.Helper()
	switch s.surface(k).RateLimit {
	case RLOpenAI:
		expectHeader(t, h, "x-ratelimit-limit-requests", "100")
		expectHeader(t, h, "x-ratelimit-remaining-requests", "99")
		expectHeader(t, h, "x-ratelimit-reset-requests", "600ms")
		expectHeader(t, h, "x-ratelimit-limit-tokens", "10000")
		expectHeader(t, h, "x-ratelimit-remaining-tokens", "9500")
		expectHeader(t, h, "x-ratelimit-reset-tokens", "6m0s")
		for _, n := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
			if v, _ := header(h, n); !reGoDuration.MatchString(v) || v == "" {
				t.Errorf("%s = %q is not a Go-style duration", n, v)
			}
		}
	case RLAnthropic:
		expectHeader(t, h, "anthropic-ratelimit-requests-limit", "100")
		expectHeader(t, h, "anthropic-ratelimit-requests-remaining", "99")
		expectHeader(t, h, "anthropic-ratelimit-tokens-limit", "10000")
		expectHeader(t, h, "anthropic-ratelimit-tokens-remaining", "9000") // rounded down to 1000
		expectHeader(t, h, "anthropic-ratelimit-input-tokens-limit", "8000")
		expectHeader(t, h, "anthropic-ratelimit-input-tokens-remaining", "7000")
		expectHeader(t, h, "anthropic-ratelimit-output-tokens-limit", "2000")
		expectHeader(t, h, "anthropic-ratelimit-output-tokens-remaining", "1000")
		now := time.Now().UTC()
		for _, n := range []string{"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset", "anthropic-ratelimit-input-tokens-reset", "anthropic-ratelimit-output-tokens-reset"} {
			v, _ := header(h, n)
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				t.Errorf("%s = %q is not RFC 3339", n, v)
				continue
			}
			if ts.Before(now.Add(-2*time.Second)) || ts.After(now.Add(7*time.Minute)) {
				t.Errorf("%s = %s, want now + reset", n, v)
			}
		}
	case RLNone:
		if names := limitHeaderNames(h); len(names) > 0 {
			t.Errorf("provider sends no rate-limit headers (spec 006), got %v", names)
		}
	}
}

// --- ids ---

func (s *suite) testIDs(t *testing.T) {
	m, ok := s.minter()
	if !ok {
		for _, k := range s.surfaces() {
			if s.surface(k).IDs != nil {
				t.Fatalf("driver does not implement driver.IDMinter but the profile expects %s ids", k)
			}
		}
		t.Skip("driver does not implement driver.IDMinter (optional)")
	}
	for _, k := range s.surfaces() {
		f := s.surface(k).IDs
		s.run(t, string(k), func(t *testing.T) {
			now := time.Now()
			rid := m.ResponseID(k, generate.NewRNG(1, "a"), now)
			if rid != m.ResponseID(k, generate.NewRNG(1, "a"), now) {
				t.Error("ResponseID is not reproducible under the same seed")
			}
			if rid == m.ResponseID(k, generate.NewRNG(2, "a"), now) {
				t.Error("ResponseID ignores the seed")
			}
			tid := m.ToolCallID(k, generate.NewRNG(1, "a"))
			if tid != m.ToolCallID(k, generate.NewRNG(1, "a")) {
				t.Error("ToolCallID is not reproducible under the same seed")
			}
			if tid == m.ToolCallID(k, generate.NewRNG(2, "a")) {
				t.Error("ToolCallID ignores the seed")
			}
			q1, q2 := m.RequestID(k, "token-1"), m.RequestID(k, "token-2")
			if q1 == q2 {
				t.Error("RequestID repeats for different tokens")
			}
			if q1 != m.RequestID(k, "token-1") {
				t.Error("RequestID is not a function of the token")
			}
			if f == nil {
				return
			}
			if !f.Response.MatchString(rid) {
				t.Errorf("ResponseID %q does not match %s", rid, f.Response)
			}
			if !f.ToolCall.MatchString(tid) {
				t.Errorf("ToolCallID %q does not match %s", tid, f.ToolCall)
			}
			if !f.Request.MatchString(q1) {
				t.Errorf("RequestID %q does not match %s", q1, f.Request)
			}
			if strings.HasPrefix(rid, "gen-") {
				if epoch := strings.Split(rid, "-")[1]; epoch != strconv.FormatInt(now.Unix(), 10) {
					t.Errorf("generation id %q does not carry the creation epoch %d", rid, now.Unix())
				}
			}
		})
	}
}

// expectRefusalDetails checks the stop_details of a refusal at prefix: the
// type, and category and explanation present as null (no named policy area).
func expectRefusalDetails(t *testing.T, body any, prefix string) {
	t.Helper()
	expectPath(t, body, prefix+".type", "refusal")
	expectPath(t, body, prefix+".category", nil)
	expectPath(t, body, prefix+".explanation", nil)
}
