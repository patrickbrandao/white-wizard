package oai_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

var errBoom = errors.New("boom")

// rec is a core.ResponseWriter that records everything. Writes fail once
// okWrites successful writes happened (okWrites < 0: never); failFlush makes
// every Flush fail.
type rec struct {
	status    int
	headers   map[string]string
	body      bytes.Buffer
	frames    []string
	okWrites  int
	failFlush bool
}

func newRec() *rec { return &rec{headers: map[string]string{}, okWrites: -1} }

func failingRec(okWrites int) *rec { r := newRec(); r.okWrites = okWrites; return r }

func (r *rec) Status(c int)                   { r.status = c }
func (r *rec) Header(k, v string)             { r.headers[k] = v }
func (r *rec) Fault(core.TransportFault) bool { return false }

func (r *rec) Write(p []byte) (int, error) {
	if r.okWrites == 0 {
		return 0, errBoom
	}
	if r.okWrites > 0 {
		r.okWrites--
	}
	r.frames = append(r.frames, string(p))
	return r.body.Write(p)
}

func (r *rec) Flush() error {
	if r.failFlush {
		return errBoom
	}
	return nil
}

// data decodes the JSON of every "data:" frame (except [DONE]).
func (r *rec) data(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range r.frames {
		if !strings.HasPrefix(f, "data: {") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(f, "data: "))), &m); err != nil {
			t.Fatalf("frame %q: %v", f, err)
		}
		out = append(out, m)
	}
	return out
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %q: %v", b, err)
	}
	return m
}

func dig(m any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, _ := m.(map[string]any)
			m = mm[k]
		case int:
			s, _ := m.([]any)
			if k >= len(s) {
				return nil
			}
			m = s[k]
		}
	}
	return m
}

var created = time.Unix(1_700_000_000, 0)

func resp() *core.Response {
	return &core.Response{ID: "chatcmpl-1", Model: "gpt-x", Created: created, Content: "hi", FinishReason: core.FinishStop, Usage: core.Usage{PromptTokens: 3, CompletionTokens: 5, ReasoningTokens: 2, CachedTokens: 1}}
}

func chatReq() *core.Request { return &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x"} }

func TestDefaultHooksWriteChat(t *testing.T) {
	h := oai.DefaultHooks()
	if h.Name != "openai" || h.ReasoningField != oai.ReasoningHidden || h.SystemFingerprint == "" {
		t.Fatalf("DefaultHooks: %+v", h)
	}
	r := newRec()
	rs := resp()
	rs.Reasoning = "secret"
	rs.Headers = map[string]string{"x-wwz-scenario": "s"}
	rl := &core.RateLimitState{Present: true, RequestsLimit: 10, RequestsRemaining: 9, RequestsReset: 500 * time.Millisecond}
	if err := oai.WriteChat(&h, r, chatReq(), rs, rl); err != nil {
		t.Fatal(err)
	}
	if r.status != 200 || r.headers["Content-Type"] != "application/json" || r.headers["x-wwz-scenario"] != "s" || r.headers["x-ratelimit-limit-requests"] != "10" {
		t.Fatalf("status/headers: %d %v", r.status, r.headers)
	}
	b := decode(t, r.body.Bytes())
	if b["object"] != "chat.completion" || b["id"] != "chatcmpl-1" || b["created"] != float64(created.Unix()) || b["system_fingerprint"] != h.SystemFingerprint {
		t.Fatalf("envelope: %v", b)
	}
	msg := dig(b, "choices", 0, "message").(map[string]any)
	if msg["content"] != "hi" || msg["refusal"] != nil || msg["reasoning_content"] != nil || msg["reasoning"] != nil {
		t.Fatalf("hidden reasoning must not leak: %v", msg)
	}
	if dig(b, "choices", 0, "finish_reason") != "stop" || dig(b, "usage", "total_tokens") != float64(8) ||
		dig(b, "usage", "completion_tokens_details", "reasoning_tokens") != float64(2) || dig(b, "usage", "prompt_tokens_details", "cached_tokens") != float64(1) {
		t.Fatalf("finish/usage: %v", b)
	}
}

func TestWriteChatReasoningFields(t *testing.T) {
	cases := []struct {
		field   string
		details bool
		exclude bool
		check   func(t *testing.T, msg map[string]any)
	}{
		{"reasoning_content", false, false, func(t *testing.T, m map[string]any) {
			if m["reasoning_content"] != "think" || m["content"] != "hi" {
				t.Fatalf("%v", m)
			}
		}},
		{"reasoning", true, false, func(t *testing.T, m map[string]any) {
			if m["reasoning"] != "think" || dig(m, "reasoning_details", 0, "text") != "think" || dig(m, "reasoning_details", 0, "type") != "reasoning.text" {
				t.Fatalf("%v", m)
			}
		}},
		{"reasoning", false, false, func(t *testing.T, m map[string]any) {
			if m["reasoning"] != "think" || m["reasoning_details"] != nil {
				t.Fatalf("%v", m)
			}
		}},
		{"inline_tag", false, false, func(t *testing.T, m map[string]any) {
			if m["content"] != "<thinking>think</thinking>hi" {
				t.Fatalf("%v", m)
			}
		}},
		{"reasoning", true, true, func(t *testing.T, m map[string]any) {
			if m["reasoning"] != nil || m["content"] != "hi" {
				t.Fatalf("exclude must hide: %v", m)
			}
		}},
	}
	for _, c := range cases {
		h := oai.Hooks{ReasoningField: c.field, ReasoningDetails: c.details, ErrorBody: oai.OpenAIErrorBody}
		r := newRec()
		rs := resp()
		rs.Reasoning = "think"
		req := chatReq()
		req.Reasoning.Exclude = c.exclude
		if err := oai.WriteChat(&h, r, req, rs, nil); err != nil {
			t.Fatal(err)
		}
		b := decode(t, r.body.Bytes())
		if _, ok := b["system_fingerprint"]; ok {
			t.Fatalf("empty SystemFingerprint must be omitted: %v", b)
		}
		c.check(t, dig(b, "choices", 0, "message").(map[string]any))
	}
}

func TestWriteChatToolCallsRefusalAndExtras(t *testing.T) {
	h := oai.Hooks{
		NativeFinishReason: true,
		UsageExtras: func(*core.Request, *core.Response, core.Usage) map[string]any {
			return map[string]any{"cost": 0.5}
		},
		TopExtras: func(*core.Request, *core.Response) map[string]any { return map[string]any{"provider": "P"} },
	}
	r := newRec()
	rs := resp()
	rs.Content = ""
	rs.FinishReason = core.FinishToolCalls
	rs.ToolCalls = []core.ToolCall{{ID: "c1", Name: "f", Arguments: `{"a":1}`}}
	if err := oai.WriteChat(&h, r, chatReq(), rs, &core.RateLimitState{Present: true}); err != nil {
		t.Fatal(err)
	}
	b := decode(t, r.body.Bytes())
	msg := dig(b, "choices", 0, "message").(map[string]any)
	if msg["content"] != nil || dig(msg, "tool_calls", 0, "function", "name") != "f" || dig(msg, "tool_calls", 0, "type") != "function" || dig(msg, "tool_calls", 0, "id") != "c1" {
		t.Fatalf("tool calls: %v", msg)
	}
	if dig(b, "choices", 0, "native_finish_reason") != "tool_calls" || b["provider"] != "P" || dig(b, "usage", "cost") != 0.5 {
		t.Fatalf("extras: %v", b)
	}

	r = newRec()
	rs = resp()
	rs.Content = "no"
	rs.FinishReason = core.FinishRefusal
	if err := oai.WriteChat(&h, r, chatReq(), rs, nil); err != nil {
		t.Fatal(err)
	}
	b = decode(t, r.body.Bytes())
	if dig(b, "choices", 0, "message", "refusal") != "no" || dig(b, "choices", 0, "message", "content") != nil || dig(b, "choices", 0, "finish_reason") != "stop" {
		t.Fatalf("refusal: %v", b)
	}

	r = newRec()
	rs = resp()
	rs.FinishReason = ""
	if err := oai.WriteChat(&h, r, chatReq(), rs, nil); err != nil {
		t.Fatal(err)
	}
	if b = decode(t, r.body.Bytes()); dig(b, "choices", 0, "finish_reason") != nil {
		t.Fatalf("empty finish reason must be null: %v", b)
	}
}

func TestWriteChatCompletionsEndpoint(t *testing.T) {
	for _, native := range []bool{false, true} {
		h := oai.Hooks{NativeFinishReason: native}
		r := newRec()
		req := &core.Request{Endpoint: core.EndpointCompletions}
		if err := oai.WriteChat(&h, r, req, resp(), nil); err != nil {
			t.Fatal(err)
		}
		b := decode(t, r.body.Bytes())
		c := dig(b, "choices", 0).(map[string]any)
		if b["object"] != "text_completion" || c["text"] != "hi" || c["message"] != nil {
			t.Fatalf("text_completion: %v", b)
		}
		if _, ok := c["native_finish_reason"]; ok != native {
			t.Fatalf("native_finish_reason presence %v: %v", native, c)
		}
	}
}

func TestWriteChatErrors(t *testing.T) {
	bad := oai.Hooks{TopExtras: func(*core.Request, *core.Response) map[string]any { return map[string]any{"x": make(chan int)} }}
	if err := oai.WriteChat(&bad, newRec(), chatReq(), resp(), nil); err == nil {
		t.Fatal("unmarshalable body: want error")
	}
	h := oai.DefaultHooks()
	if err := oai.WriteChat(&h, failingRec(0), chatReq(), resp(), nil); !errors.Is(err, errBoom) {
		t.Fatalf("write error: %v", err)
	}
	r := newRec()
	r.failFlush = true
	if err := oai.WriteChat(&h, r, chatReq(), resp(), nil); !errors.Is(err, errBoom) {
		t.Fatalf("flush error: %v", err)
	}
}

func TestOpenAIRateLimitHeaders(t *testing.T) {
	m := oai.OpenAIRateLimitHeaders(&core.RateLimitState{
		RequestsLimit: 100, RequestsRemaining: -3, RequestsReset: 0,
		TokensLimit: 1000, TokensRemaining: 400, TokensReset: 1500 * time.Millisecond,
	})
	want := map[string]string{
		"x-ratelimit-limit-requests": "100", "x-ratelimit-remaining-requests": "0", "x-ratelimit-reset-requests": "0s",
		"x-ratelimit-limit-tokens": "1000", "x-ratelimit-remaining-tokens": "400", "x-ratelimit-reset-tokens": "1.5s",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if m := oai.OpenAIRateLimitHeaders(&core.RateLimitState{RequestsLimit: 1, RequestsReset: 20 * time.Millisecond}); m["x-ratelimit-reset-requests"] != "20ms" || len(m) != 3 {
		t.Fatalf("sub-second reset / no tokens: %v", m)
	}
}

func TestCeilSecondsAndLimitNumbers(t *testing.T) {
	for d, want := range map[time.Duration]int64{45 * time.Second: 45, 1500 * time.Millisecond: 2, 0: 1, time.Millisecond: 1} {
		if got := oai.CeilSeconds(d); got != want {
			t.Errorf("CeilSeconds(%v) = %d, want %d", d, got, want)
		}
	}
	if _, _, _, ok := oai.LimitNumbers(nil); ok {
		t.Fatal("nil error")
	}
	if _, _, _, ok := oai.LimitNumbers(&core.Error{}); ok {
		t.Fatal("nil extra")
	}
	if _, _, _, ok := oai.LimitNumbers(&core.Error{Extra: map[string]any{"limit_value": 0}}); ok {
		t.Fatal("zero limit")
	}
	l, u, r, ok := oai.LimitNumbers(&core.Error{Extra: map[string]any{"limit_value": 10, "limit_used": 9, "limit_requested": 2}})
	if !ok || l != 10 || u != 9 || r != 2 {
		t.Fatalf("LimitNumbers = %d %d %d %v", l, u, r, ok)
	}
}

func TestOpenAIErrorBody(t *testing.T) {
	req := &core.Request{Model: "gpt-x"}
	cases := []struct {
		e      core.Error
		status int
		typ    string
		code   any
		msg    string
	}{
		{core.Error{Kind: core.ErrInvalidRequest}, 400, "invalid_request_error", nil, "could not parse the JSON"},
		{core.Error{Kind: core.ErrInvalidRequest, Message: "custom", Param: "p"}, 400, "invalid_request_error", nil, "custom"},
		{core.Error{Kind: core.ErrContextLength}, 400, "invalid_request_error", "context_length_exceeded", "maximum context length"},
		{core.Error{Kind: core.ErrContentFilter}, 400, "invalid_request_error", "content_filter", "safety system"},
		{core.Error{Kind: core.ErrInvalidAPIKey}, 401, "invalid_request_error", "invalid_api_key", "Incorrect API key"},
		{core.Error{Kind: core.ErrPermission}, 403, "invalid_request_error", "permission_denied", "insufficient permissions"},
		{core.Error{Kind: core.ErrGeoBlocked}, 403, "request_forbidden", "unsupported_country_region_territory", "Country, region"},
		{core.Error{Kind: core.ErrAccountSuspended}, 403, "invalid_request_error", "account_suspended", "has been suspended"},
		{core.Error{Kind: core.ErrAccountBanned}, 403, "invalid_request_error", "account_deactivated", "deactivated"},
		{core.Error{Kind: core.ErrModelNotFound}, 404, "invalid_request_error", "model_not_found", "The model `gpt-x` does not exist"},
		{core.Error{Kind: core.ErrNotFound}, 404, "invalid_request_error", nil, "Invalid URL"},
		{core.Error{Kind: core.ErrMethodNotAllowed}, 405, "invalid_request_error", nil, "Invalid URL"},
		{core.Error{Kind: core.ErrRequestTooLarge}, 413, "invalid_request_error", "request_too_large", "Request too large"},
		{core.Error{Kind: core.ErrRateLimit}, 429, "requests", "rate_limit_exceeded", "Rate limit reached for gpt-x in organization org-wwz on requests per min (RPM). Visit"},
		{core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "tpm", "limit_value": 100, "limit_used": 90, "limit_requested": 20}, RetryAfter: 1500 * time.Millisecond}, 429, "tokens", "rate_limit_exceeded", "on tokens per min (TPM): Limit 100, Used 90, Requested 20. Please try again in 1.5s."},
		{core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "rpd"}}, 429, "requests", "rate_limit_exceeded", "requests per day (RPD)"},
		{core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "tpd"}}, 429, "tokens", "rate_limit_exceeded", "tokens per day (TPD)"},
		{core.Error{Kind: core.ErrSlowDown, RetryAfter: time.Second}, 429, "rate_limit_error", "slow_down", "slow down"},
		{core.Error{Kind: core.ErrQuota}, 429, "insufficient_quota", "insufficient_quota", "exceeded your current quota"},
		{core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "credits"}}, 429, "insufficient_quota", "credit_balance_exhausted", "current quota"},
		{core.Error{Kind: core.ErrSpendLimit}, 429, "insufficient_quota", "organization_spend_limit_exceeded", "organization's spend limit"},
		{core.Error{Kind: core.ErrBudget}, 429, "insufficient_quota", "project_spend_limit_exceeded", "project's spend limit"},
		{core.Error{Kind: core.ErrServerError}, 500, "server_error", nil, "had an error"},
		{core.Error{Kind: core.ErrOverloaded, RetryAfter: 2 * time.Second}, 503, "service_unavailable_error", "server_is_overloaded", "overloaded"},
		{core.Error{Kind: core.ErrGatewayTimeout}, 504, "server_error", nil, "Gateway timeout"},
		{core.Error{Kind: core.ErrMissingSession}, 500, "server_error", nil, "had an error"},
		{core.Error{Kind: core.ErrServerError, Status: 599, Code: "custom_code"}, 599, "server_error", "custom_code", "had an error"},
	}
	for _, c := range cases {
		e := c.e
		status, body, ct, _ := oai.OpenAIErrorBody(req, &e)
		if status != c.status || !strings.HasPrefix(ct, "application/json") && ct != "text/plain" {
			t.Errorf("%s: status %d ct %q, want %d", e.Kind, status, ct, c.status)
			continue
		}
		b := body.([]byte)
		m := decode(t, b)
		if dig(m, "error", "type") != c.typ || dig(m, "error", "code") != c.code || !strings.Contains(dig(m, "error", "message").(string), c.msg) {
			t.Errorf("%s: body %s, want type %q code %v msg %q", e.Kind, b, c.typ, c.code, c.msg)
		}
		var wantParam any
		if e.Param != "" {
			wantParam = e.Param
		}
		if dig(m, "error", "param") != wantParam {
			t.Errorf("%s: param %v", e.Kind, dig(m, "error", "param"))
		}
	}
}

func TestOpenAIErrorBodyHeadersAndPaths(t *testing.T) {
	_, _, _, hdr := oai.OpenAIErrorBody(&core.Request{}, &core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond})
	if hdr["retry-after"] != "2" || hdr["retry-after-ms"] != "1500" {
		t.Fatalf("rate limit retry headers: %v", hdr)
	}
	for _, k := range []core.ErrorKind{core.ErrSlowDown, core.ErrOverloaded} {
		if _, _, _, hdr := oai.OpenAIErrorBody(&core.Request{}, &core.Error{Kind: k, RetryAfter: 3 * time.Second}); hdr["retry-after"] != "3" {
			t.Fatalf("%s retry-after: %v", k, hdr)
		}
	}
	status, body, ct, _ := oai.OpenAIErrorBody(&core.Request{}, &core.Error{Kind: core.ErrBadGateway})
	if b, ok := body.([]byte); status != 502 || ct != "text/html" || !ok || !bytes.Contains(b, []byte("502 Bad Gateway")) {
		t.Fatalf("bad gateway: %d %q %v", status, ct, body)
	}
	req := &core.Request{Headers: map[string]string{"x-wwz-path": "/v1/nope"}}
	_, body, _, _ = oai.OpenAIErrorBody(req, &core.Error{Kind: core.ErrNotFound})
	if b := body.([]byte); !bytes.Contains(b, []byte("Invalid URL (/v1/nope)")) {
		t.Fatalf("not found path: %s", b)
	}
	_, body, _, _ = oai.OpenAIErrorBody(&core.Request{}, &core.Error{Kind: core.ErrRateLimit})
	if b := body.([]byte); !bytes.Contains(b, []byte("Rate limit reached in organization")) {
		t.Fatalf("rate limit without model: %s", b)
	}
	b, _ := json.Marshal(oai.OpenAIStreamError(&core.Error{Kind: core.ErrServerError, Message: "mid"}))
	if m := decode(t, b); dig(m, "error", "message") != "mid" || dig(m, "error", "type") != "server_error" {
		t.Fatalf("stream error: %s", b)
	}
}

func TestWriteError(t *testing.T) {
	h := oai.DefaultHooks()
	r := newRec()
	e := &core.Error{Kind: core.ErrRateLimit, RetryAfter: time.Second, Extra: map[string]any{"header:x-custom": "v", "header:x-int": 1, "other": "o"}}
	oai.WriteError(&h, r, nil, e, &core.RateLimitState{Present: true, TokensLimit: 5})
	if r.status != 429 || r.headers["Content-Type"] != "application/json" || r.headers["retry-after"] != "1" || r.headers["x-custom"] != "v" || r.headers["x-ratelimit-limit-tokens"] != "5" {
		t.Fatalf("headers: %d %v", r.status, r.headers)
	}
	if _, ok := r.headers["x-int"]; ok {
		t.Fatal("non-string header extra must be ignored")
	}
	if r.headers["Content-Length"] != itoa(r.body.Len()) {
		t.Fatalf("content-length %s vs %d", r.headers["Content-Length"], r.body.Len())
	}

	r = newRec()
	oai.WriteError(&h, r, chatReq(), &core.Error{Kind: core.ErrServerError, Extra: map[string]any{core.ExtraRawBody: "plain"}}, nil)
	if r.status != 500 || r.body.String() != "plain" || r.headers["Content-Type"] != core.RawBodyText {
		t.Fatalf("raw body: %d %q %v", r.status, r.body.String(), r.headers)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// --- streaming ---

func newStream(h oai.Hooks, w *rec, req *core.Request) *oai.Stream {
	if h.StreamErrorBody == nil {
		h.StreamErrorBody = oai.OpenAIStreamError
	}
	return oai.NewStream(&h, w, req, nil)
}

func TestStreamText(t *testing.T) {
	h := oai.DefaultHooks()
	h.KeepAlive = "OPENROUTER PROCESSING"
	h.TopExtras = func(*core.Request, *core.Response) map[string]any { return map[string]any{"provider": "P"} }
	w := newRec()
	rs := resp()
	rs.Headers = map[string]string{"x-wwz-a": "1"}
	s := oai.NewStream(&h, w, chatReq(), &core.RateLimitState{Present: true, RequestsLimit: 2})
	must(t, s.Start(rs))
	must(t, s.TextDelta("he"))
	must(t, s.TextDelta("llo"))
	must(t, s.Ping())
	must(t, s.Finish(core.FinishStop, core.Usage{PromptTokens: 1, CompletionTokens: 2}, true))
	must(t, s.Close())
	if w.status != 200 || w.headers["Content-Type"] != "text/event-stream; charset=utf-8" || w.headers["x-wwz-a"] != "1" || w.headers["x-ratelimit-limit-requests"] != "2" {
		t.Fatalf("stream headers: %v", w.headers)
	}
	if w.frames[0] != ": OPENROUTER PROCESSING\n\n" || !strings.Contains(w.body.String(), ": ping\n\n") || w.frames[len(w.frames)-1] != "data: [DONE]\n\n" {
		t.Fatalf("frames: %q", w.frames)
	}
	if string(s.LastFrame()) != "data: [DONE]\n\n" {
		t.Fatalf("LastFrame: %q", s.LastFrame())
	}
	d := w.data(t)
	if len(d) != 5 {
		t.Fatalf("want role, 2 deltas, finish, usage: %d %q", len(d), w.frames)
	}
	if dig(d[0], "choices", 0, "delta", "role") != "assistant" || d[0]["object"] != "chat.completion.chunk" || d[0]["provider"] != "P" || d[0]["system_fingerprint"] != h.SystemFingerprint {
		t.Fatalf("role chunk: %v", d[0])
	}
	if dig(d[1], "choices", 0, "delta", "content") != "he" || dig(d[3], "choices", 0, "finish_reason") != "stop" {
		t.Fatalf("deltas/finish: %v %v", d[1], d[3])
	}
	if ch := d[4]["choices"].([]any); len(ch) != 0 || dig(d[4], "usage", "total_tokens") != float64(3) || d[4]["provider"] != "P" {
		t.Fatalf("usage chunk: %v", d[4])
	}
}

func TestStreamToolCallsAndFinalChoice(t *testing.T) {
	h := oai.Hooks{FinalChunkWithChoice: true, NativeFinishReason: true}
	w := newRec()
	s := newStream(h, w, chatReq())
	must(t, s.Start(resp()))
	must(t, s.ToolCallStart(0, "c1", "f"))
	must(t, s.ToolArgsDelta(0, `{"a":`))
	must(t, s.ToolCallEnd(0))
	must(t, s.Finish(core.FinishToolCalls, core.Usage{}, true))
	d := w.data(t)
	if dig(d[1], "choices", 0, "delta", "tool_calls", 0, "id") != "c1" || dig(d[1], "choices", 0, "delta", "tool_calls", 0, "function", "name") != "f" {
		t.Fatalf("tool start: %v", d[1])
	}
	if dig(d[2], "choices", 0, "delta", "tool_calls", 0, "function", "arguments") != `{"a":` {
		t.Fatalf("tool args: %v", d[2])
	}
	if dig(d[1], "choices", 0, "native_finish_reason") != nil {
		t.Fatalf("native_finish_reason only with a finish: %v", d[1])
	}
	if dig(d[3], "choices", 0, "native_finish_reason") != "tool_calls" {
		t.Fatalf("finish chunk: %v", d[3])
	}
	if dig(d[4], "choices", 0, "finish_reason") != "tool_calls" || dig(d[4], "usage") == nil {
		t.Fatalf("final chunk with choice: %v", d[4])
	}
	if _, ok := d[0]["system_fingerprint"]; ok {
		t.Fatal("no fingerprint configured")
	}
}

func TestStreamCompletionsEndpoint(t *testing.T) {
	for _, final := range []bool{false, true} {
		h := oai.Hooks{FinalChunkWithChoice: final, NativeFinishReason: true}
		w := newRec()
		s := newStream(h, w, &core.Request{Endpoint: core.EndpointCompletions})
		must(t, s.Start(resp()))
		must(t, s.TextDelta("t"))
		must(t, s.Finish(core.FinishLength, core.Usage{}, true))
		d := w.data(t)
		for _, c := range d {
			if c["object"] != "text_completion" {
				t.Fatalf("object: %v", c)
			}
		}
		if dig(d[1], "choices", 0, "text") != "t" || dig(d[2], "choices", 0, "native_finish_reason") != "length" {
			t.Fatalf("completions chunks: %v", d)
		}
		ch := d[3]["choices"].([]any)
		if final && (len(ch) != 1 || dig(ch, 0, "text") != "") || !final && len(ch) != 0 {
			t.Fatalf("final=%v usage choices: %v", final, ch)
		}
	}
}

func TestStreamReasoningFields(t *testing.T) {
	cases := []struct {
		field   string
		details bool
		want    []string // delta keys of the reasoning frames
	}{
		{"reasoning_content", false, []string{"reasoning_content"}},
		{"reasoning", false, []string{"reasoning"}},
		{"reasoning", true, []string{"reasoning", "reasoning_details"}},
	}
	for _, c := range cases {
		w := newRec()
		s := newStream(oai.Hooks{ReasoningField: c.field, ReasoningDetails: c.details}, w, chatReq())
		rs := resp()
		rs.Reasoning = "r"
		must(t, s.Start(rs))
		must(t, s.ReasoningDelta("r"))
		must(t, s.TextDelta("x"))
		must(t, s.Finish(core.FinishStop, core.Usage{}, false))
		d := w.data(t)
		delta := dig(d[1], "choices", 0, "delta").(map[string]any)
		if len(delta) != len(c.want) {
			t.Fatalf("%s/%v: delta %v", c.field, c.details, delta)
		}
		for _, k := range c.want {
			if _, ok := delta[k]; !ok {
				t.Fatalf("%s/%v: missing %s in %v", c.field, c.details, k, delta)
			}
		}
		if len(d) != 4 {
			t.Fatalf("no usage chunk without include_usage: %d", len(d))
		}
	}
}

func TestStreamInlineTag(t *testing.T) {
	w := newRec()
	s := newStream(oai.Hooks{ReasoningField: "inline_tag"}, w, chatReq())
	must(t, s.Start(resp()))
	must(t, s.ReasoningDelta("a"))
	must(t, s.ReasoningDelta("b"))
	must(t, s.TextDelta("x"))
	var got string
	for _, c := range w.data(t) {
		if v, ok := dig(c, "choices", 0, "delta", "content").(string); ok {
			got += v
		}
	}
	if got != "<thinking>ab</thinking>x" {
		t.Fatalf("inline content %q", got)
	}
	// The tag also closes before a tool call and before the finish.
	for _, next := range []func(s *oai.Stream) error{
		func(s *oai.Stream) error { return s.ToolCallStart(0, "c", "f") },
		func(s *oai.Stream) error { return s.Finish(core.FinishStop, core.Usage{}, false) },
	} {
		w := newRec()
		s := newStream(oai.Hooks{ReasoningField: "inline_tag"}, w, chatReq())
		must(t, s.Start(resp()))
		must(t, s.ReasoningDelta("a"))
		must(t, next(s))
		closed := false
		for _, c := range w.data(t) {
			if dig(c, "choices", 0, "delta", "content") == "</thinking>" {
				closed = true
			}
		}
		if !closed {
			t.Fatalf("closing tag missing: %s", w.body.String())
		}
	}
}

// TestStreamHiddenReasoning: with hidden reasoning the role chunk waits for
// the first visible frame (TTFT includes the silent thinking).
func TestStreamHiddenReasoning(t *testing.T) {
	visible := map[string]func(s *oai.Stream) error{
		"text":   func(s *oai.Stream) error { return s.TextDelta("x") },
		"tool":   func(s *oai.Stream) error { return s.ToolCallStart(0, "c", "f") },
		"finish": func(s *oai.Stream) error { return s.Finish(core.FinishStop, core.Usage{}, false) },
		"error":  func(s *oai.Stream) error { return s.Error(&core.Error{Kind: core.ErrServerError}) },
	}
	for name, next := range visible {
		h := oai.DefaultHooks()
		w := newRec()
		s := oai.NewStream(&h, w, chatReq(), nil)
		rs := resp()
		rs.Reasoning = "hidden"
		must(t, s.Start(rs))
		must(t, s.ReasoningDelta("hidden"))
		if len(w.frames) != 0 {
			t.Fatalf("%s: nothing visible before the first delta: %q", name, w.frames)
		}
		must(t, next(s))
		d := w.data(t)
		if dig(d[0], "choices", 0, "delta", "role") != "assistant" || strings.Contains(w.body.String(), "hidden") {
			t.Fatalf("%s: role chunk first, no reasoning text: %q", name, w.frames)
		}
	}
}

func TestStreamError(t *testing.T) {
	w := newRec()
	s := newStream(oai.Hooks{}, w, chatReq())
	must(t, s.Start(resp()))
	must(t, s.Error(&core.Error{Kind: core.ErrOverloaded}))
	d := w.data(t)
	if dig(d[1], "error", "code") != "server_is_overloaded" || w.frames[len(w.frames)-1] != "data: [DONE]\n\n" {
		t.Fatalf("stream error: %q", w.frames)
	}
	must(t, s.Raw([]byte("data: garbage\n\n")))
	if string(s.LastFrame()) != "data: garbage\n\n" {
		t.Fatalf("raw frame: %q", s.LastFrame())
	}
}

// TestStreamWriteErrors: every frame-writing method propagates the
// transport error, at whichever frame it happens.
func TestStreamWriteErrors(t *testing.T) {
	type op struct {
		name     string
		hooks    oai.Hooks
		reason   string // response reasoning (hidden defers the role chunk)
		prep     func(s *oai.Stream) error
		run      func(s *oai.Stream) error
		okWrites int // successful writes before failing, counted after prep
	}
	text := func(s *oai.Stream) error { return s.TextDelta("x") }
	tool := func(s *oai.Stream) error { return s.ToolCallStart(0, "c", "f") }
	finish := func(s *oai.Stream) error { return s.Finish(core.FinishStop, core.Usage{}, true) }
	streamErr := func(s *oai.Stream) error { return s.Error(&core.Error{Kind: core.ErrServerError}) }
	openInline := func(s *oai.Stream) error { return s.ReasoningDelta("r") }
	noop := func(*oai.Stream) error { return nil }
	hidden := oai.DefaultHooks()
	inline := oai.Hooks{ReasoningField: "inline_tag"}
	plain := oai.Hooks{}
	ops := []op{
		{"keepalive", oai.Hooks{KeepAlive: "k"}, "", nil, nil, 0},
		{"role", plain, "", nil, nil, 0},
		{"hidden text role", hidden, "r", noop, text, 0},
		{"hidden tool role", hidden, "r", noop, tool, 0},
		{"hidden finish role", hidden, "r", noop, finish, 0},
		{"hidden error role", hidden, "r", noop, streamErr, 0},
		{"inline text close", inline, "", openInline, text, 0},
		{"inline tool close", inline, "", openInline, tool, 0},
		{"inline finish close", inline, "", openInline, finish, 0},
		{"inline reasoning", inline, "", noop, openInline, 0},
		{"reasoning field", oai.Hooks{ReasoningField: "reasoning"}, "", noop, openInline, 0},
		{"text", plain, "", noop, text, 0},
		{"tool", plain, "", noop, tool, 0},
		{"args", plain, "", noop, func(s *oai.Stream) error { return s.ToolArgsDelta(0, "a") }, 0},
		{"ping", plain, "", noop, func(s *oai.Stream) error { return s.Ping() }, 0},
		{"finish chunk", plain, "", noop, finish, 0},
		{"usage chunk", plain, "", noop, finish, 1},
		{"done", plain, "", noop, finish, 2},
		{"error chunk", plain, "", noop, streamErr, 0},
		{"error done", plain, "", noop, streamErr, 1},
		{"raw", plain, "", noop, func(s *oai.Stream) error { return s.Raw([]byte("x")) }, 0},
	}
	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			w := newRec()
			s := newStream(o.hooks, w, chatReq())
			rs := resp()
			rs.Reasoning = o.reason
			if o.prep == nil {
				// The failure is in Start itself.
				w.okWrites = o.okWrites
				if err := s.Start(rs); !errors.Is(err, errBoom) {
					t.Fatalf("Start: %v", err)
				}
				return
			}
			must(t, s.Start(rs))
			must(t, o.prep(s))
			w.okWrites = o.okWrites
			if err := o.run(s); !errors.Is(err, errBoom) {
				t.Fatalf("got %v, want write error", err)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
