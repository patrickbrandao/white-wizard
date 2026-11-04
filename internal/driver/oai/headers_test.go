package oai_test

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func TestMaskKey(t *testing.T) {
	for k, want := range map[string]string{
		"sk-invalid-wwz-drift":            "sk-inval********rift", // observed on the real API
		"abcdefghijkl":                    "abcdef********jkl",
		"abcdefghijklm":                   "abcdefgh*jklm",
		"sk-invalid-wwz-0000000000000000": "sk-inval*******************0000", // auth-order/badkey_*
		"wwz-key-01":                      "wwz-k********01",
		"ab":                              "a********",
		"":                                "********",
	} {
		if got := oai.MaskKey(k); got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestAuthError: the 401 masks the bearer key and carries the real
// headers; without a bearer key the message names none.
func TestAuthError(t *testing.T) {
	for auth, want := range map[string]string{
		"Bearer sk-invalid-wwz-drift": "Incorrect API key provided: sk-inval********rift. You can find",
		"Basic xyz":                   "Incorrect API key provided. You can find",
		"":                            "Incorrect API key provided. You can find",
	} {
		req := &core.Request{Headers: map[string]string{"authorization": auth}}
		status, body, ct, hdr := oai.OpenAIErrorBody(req, &core.Error{Kind: core.ErrInvalidAPIKey})
		b := body.([]byte)
		if status != 401 || ct != "text/plain" || !strings.Contains(string(b), want) || !strings.HasSuffix(string(b), "\n  \"status\": 401\n}") {
			t.Errorf("%q: %d %s %s", auth, status, ct, b)
		}
		if hdr["x-error-json"] != base64.StdEncoding.EncodeToString(b) || hdr["x-openai-authorization-error"] != "401" || hdr["x-openai-ide-error-code"] != "invalid_api_key" {
			t.Errorf("%q: headers %v", auth, hdr)
		}
	}
}

// TestNoKeyError: without a key the edge answers the long text, code null,
// in its layout and without x-error-json (auth-order/nokey_*).
func TestNoKeyError(t *testing.T) {
	status, body, ct, hdr := oai.OpenAIErrorBody(&core.Request{}, &core.Error{Kind: core.ErrInvalidAPIKey, Message: oai.NoKeyMessage})
	b := string(body.([]byte))
	if status != 401 || ct != "application/json; charset=utf-8" || hdr["x-error-json"] != "" {
		t.Fatalf("%d %s %v", status, ct, hdr)
	}
	if !strings.HasPrefix(b, "{\n    \"error\": {\n        \"message\": \"You didn't provide an API key. You need to provide") ||
		!strings.HasSuffix(b, "\"param\": null,\n        \"code\": null\n    }\n}\n") {
		t.Fatalf("body %q", b)
	}
}

// TestErrorLayouts: the API's errors (2 spaces, no newline) and the edge's
// (4 spaces, newline, charset).
func TestErrorLayouts(t *testing.T) {
	cases := []struct {
		e    core.Error
		ct   string
		edge bool
	}{
		{core.Error{Kind: core.ErrInvalidRequest, Message: "Invalid type"}, "application/json", false},
		{core.Error{Kind: core.ErrInvalidRequest, Message: oai.MissingModelMessage}, "application/json; charset=utf-8", true},
		{core.Error{Kind: core.ErrInvalidRequest}, "application/json; charset=utf-8", true},
		{core.Error{Kind: core.ErrModelNotFound}, "application/json; charset=utf-8", true},
		{core.Error{Kind: core.ErrNotFound}, "application/json; charset=utf-8", true},
		{core.Error{Kind: core.ErrRateLimit}, "application/json", false},
	}
	for _, c := range cases {
		e := c.e
		_, body, ct, _ := oai.OpenAIErrorBody(&core.Request{Model: "m"}, &e)
		b := string(body.([]byte))
		prefix := "{\n  \"error\": {\n    \"message\": "
		if c.edge {
			prefix = "{\n    \"error\": {\n        \"message\": "
		}
		if ct != c.ct || !strings.HasPrefix(b, prefix) || strings.HasSuffix(b, "\n") != c.edge {
			t.Errorf("%s %q: %q %q", e.Kind, e.Message, ct, b)
		}
		if strings.Index(b, `"param"`) > strings.Index(b, `"code"`) {
			t.Errorf("%s: param after code: %s", e.Kind, b)
		}
	}
	// The mid-stream error is compact; the HTML page stays as it is.
	b, _ := json.Marshal(oai.OpenAIStreamError(&core.Error{Kind: core.ErrServerError, Message: "x"}))
	if string(b) != `{"error":{"message":"x","type":"server_error","param":null,"code":null}}` {
		t.Errorf("stream error %s", b)
	}
	if v, ok := oai.OpenAIStreamError(&core.Error{Kind: core.ErrBadGateway}).([]byte); !ok || !strings.Contains(string(v), "502") {
		t.Errorf("bad gateway mid-stream: %v", v)
	}
}

// TestSuccessHeaders: defaults of the tier without limiter state (ADR
// 0049), the limiter's values when present, account headers stable per key.
func TestSuccessHeaders(t *testing.T) {
	req := &core.Request{
		Headers:  map[string]string{"authorization": "Bearer k1"},
		System:   "abc",
		Messages: []core.Message{{Content: "Responda só"}},
		Received: time.Now().Add(-25 * time.Millisecond),
	}
	m := oai.OpenAISuccessHeaders(req, nil, nil)
	want := map[string]string{
		"openai-version": "2020-10-01", "x-ratelimit-limit-requests": "5000", "x-ratelimit-remaining-requests": "4999",
		"x-ratelimit-reset-requests": "12ms", "x-ratelimit-limit-tokens": "2000000", "x-ratelimit-remaining-tokens": "1999994",
		"x-ratelimit-reset-tokens": "0s",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if ms := m["openai-processing-ms"]; ms == "0" || ms == "" {
		t.Errorf("openai-processing-ms %q", ms)
	}
	if !regexp.MustCompile(`^user-[a-z0-9]{24}$`).MatchString(m["openai-organization"]) || !regexp.MustCompile(`^proj_[A-Za-z0-9]{24}$`).MatchString(m["openai-project"]) {
		t.Errorf("account headers %q %q", m["openai-organization"], m["openai-project"])
	}
	again := oai.OpenAISuccessHeaders(req, nil, nil)
	other := oai.OpenAISuccessHeaders(&core.Request{Headers: map[string]string{"authorization": "Bearer k2", "openai-project": "proj_mine"}}, nil, nil)
	if again["openai-organization"] != m["openai-organization"] || again["openai-project"] != m["openai-project"] ||
		other["openai-organization"] == m["openai-organization"] || other["openai-project"] != "proj_mine" {
		t.Errorf("stable per key: %v %v %v", m, again, other)
	}
	if other["openai-processing-ms"] != "0" {
		t.Errorf("no arrival time: %q", other["openai-processing-ms"])
	}
	rl := &core.RateLimitState{Present: true, RequestsLimit: 10, RequestsRemaining: 3, RequestsReset: time.Second}
	m = oai.OpenAISuccessHeaders(nil, nil, rl)
	if m["x-ratelimit-limit-requests"] != "10" || m["x-ratelimit-remaining-requests"] != "3" || m["x-ratelimit-limit-tokens"] != "2000000" || m["x-ratelimit-remaining-tokens"] != "2000000" {
		t.Errorf("limiter state plus default tokens: %v", m)
	}
	if oai.EstimateTokens(nil) != 0 || oai.ProcessingMS(nil) != "0" {
		t.Error("nil request")
	}
}

// TestEstimateTokensSystemOnce: the Chat Completions system message is
// counted once, not again as req.System (task 245).
func TestEstimateTokensSystemOnce(t *testing.T) {
	req, e := oai.ParseChat([]byte(`{"model":"m","messages":[{"role":"system","content":"12345678"},{"role":"user","content":"1234"}]}`), driver.MapHeaders{}, "openai")
	if e != nil {
		t.Fatal(e)
	}
	if got := oai.EstimateTokens(req); got != (12+3)/4+2 {
		t.Errorf("EstimateTokens = %d, want %d", got, (12+3)/4+2)
	}
}

func TestIDs(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	req := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	if a, b := oai.NewUUID(), oai.NewUUID(); !uuid.MatchString(a) || a == b {
		t.Errorf("UUID %q %q", a, b)
	}
	if a, b := oai.NewRequestID(), oai.NewRequestID(); !req.MatchString(a) || a == b {
		t.Errorf("request id %q %q", a, b)
	}
}

// TestWriteErrorRequestID: the engine's header wins; else the request's
// id; else a fresh one.
func TestWriteErrorRequestID(t *testing.T) {
	h := oai.DefaultHooks()
	for _, c := range []struct {
		req  *core.Request
		e    *core.Error
		want string
	}{
		{nil, &core.Error{Kind: core.ErrNotFound}, "fresh"},
		{&core.Request{RequestID: "req_abc"}, &core.Error{Kind: core.ErrNotFound}, "req_abc"},
		{&core.Request{RequestID: "abc"}, &core.Error{Kind: core.ErrNotFound}, "req_abc"},
		{&core.Request{RequestID: "abc"}, &core.Error{Kind: core.ErrNotFound, Extra: map[string]any{"header:x-request-id": "req_engine"}}, "req_engine"},
	} {
		r := newRec()
		oai.WriteError(&h, r, c.req, c.e, nil)
		got := r.headers["x-request-id"]
		if c.want == "fresh" {
			if !strings.HasPrefix(got, "req_") || len(got) != 36 {
				t.Errorf("fresh id %q", got)
			}
		} else if got != c.want {
			t.Errorf("%+v: %q, want %q", c.req, got, c.want)
		}
	}
	// A dialect without RequestIDHeader writes none.
	r := newRec()
	plain := oai.Hooks{ErrorBody: oai.OpenAIErrorBody}
	oai.WriteError(&plain, r, nil, &core.Error{Kind: core.ErrNotFound}, nil)
	if _, ok := r.headers["x-request-id"]; ok {
		t.Error("x-request-id without RequestIDHeader")
	}
}

// TestStreamObfuscationAndUsageNull: obfuscation in every chunk unless
// the client turns it off; usage: null only with include_usage.
func TestStreamObfuscationAndUsageNull(t *testing.T) {
	for _, c := range []struct {
		raw          string
		includeUsage bool
		obf          bool
	}{
		{`{"stream":true}`, true, true},
		{`{"stream_options":{"include_obfuscation":false}}`, false, false},
		{`{"stream_options":{"include_obfuscation":true}}`, true, true},
		{`{"stream_options":{"include_usage":true},"include_obfuscation":1}`, true, true},
		{`not json include_obfuscation`, false, true},
	} {
		h := oai.DefaultHooks()
		w := newRec()
		req := chatReq()
		req.Raw, req.IncludeUsage = []byte(c.raw), c.includeUsage
		s := oai.NewStream(&h, w, req, nil)
		must(t, s.Start(resp()))
		must(t, s.TextDelta("x"))
		must(t, s.Finish(core.FinishStop, core.Usage{}, c.includeUsage))
		for i, ch := range w.data(t) {
			o, has := ch["obfuscation"].(string)
			if has != c.obf || c.obf && o == "" {
				t.Errorf("%s chunk %d: obfuscation %q", c.raw, i, o)
			}
			u, hasUsage := ch["usage"]
			last := i == len(w.data(t))-1 && c.includeUsage
			if hasUsage != c.includeUsage || !last && u != nil {
				t.Errorf("%s chunk %d: usage %v %v", c.raw, i, u, hasUsage)
			}
			if ch["service_tier"] != "default" {
				t.Errorf("chunk %d without service_tier: %v", i, ch)
			}
		}
	}
}

// TestGatewayHooksUnchanged: hooks without the OpenAI fields keep the
// gateway bodies as they were, with limit headers only from the limiter.
func TestGatewayHooksUnchanged(t *testing.T) {
	h := oai.Hooks{RateLimitHeaders: oai.OpenAIRateLimitHeaders}
	r := newRec()
	if err := oai.WriteChat(&h, r, chatReq(), resp(), &core.RateLimitState{Present: true, RequestsLimit: 3}); err != nil {
		t.Fatal(err)
	}
	want := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi","refusal":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"prompt_tokens_details":{"cached_tokens":1,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":2,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}`
	if r.body.String() != want || r.headers["x-ratelimit-limit-requests"] != "3" || r.headers["openai-version"] != "" {
		t.Fatalf("gateway body %s headers %v", r.body.String(), r.headers)
	}
	bad := oai.Hooks{TopExtras: func(*core.Request, *core.Response) map[string]any { return map[string]any{"x": make(chan int)} }}
	s := newStream(bad, newRec(), chatReq())
	if err := s.Start(resp()); err == nil {
		t.Fatal("unencodable chunk: want an error")
	}
	// A gateway's mid-stream error body (a map) is marshalled as it was.
	gw := oai.Hooks{StreamErrorBody: func(*core.Error) any { return map[string]any{"error": map[string]any{"code": 502}} }}
	w := newRec()
	s = oai.NewStream(&gw, w, chatReq(), nil)
	must(t, s.Start(resp()))
	must(t, s.Error(&core.Error{Kind: core.ErrBadGateway}))
	if !strings.Contains(w.body.String(), "data: {\"error\":{\"code\":502}}\n\n") {
		t.Fatalf("gateway stream error: %q", w.body.String())
	}
	// OpenAI's raw text: "<=" stays as it is in bodies, chunks and errors.
	b, _ := oai.Format{RawHTML: true}.Marshal(oai.O("m", "a <= b & c > d"))
	if string(b) != `{"m":"a <= b & c > d"}` {
		t.Fatalf("RawHTML: %s", b)
	}
	if b, _ = (oai.Format{RawHTML: true}).Marshal(map[string]string{"m": "<"}); string(b) != `{"m":"<"}` {
		t.Fatalf("RawHTML fallback: %s", b)
	}
}

// TestProjectTokensHeaders (task 234): the project tokens unit renders as
// x-ratelimit-*-project-tokens only with a project limit.
func TestProjectTokensHeaders(t *testing.T) {
	m := oai.OpenAIRateLimitHeaders(&core.RateLimitState{Present: true, ProjectTokensLimit: 100, ProjectTokensRemain: -1, ProjectTokensReset: 1500 * time.Millisecond})
	if m["x-ratelimit-limit-project-tokens"] != "100" || m["x-ratelimit-remaining-project-tokens"] != "0" || m["x-ratelimit-reset-project-tokens"] != "1.5s" || len(m) != 3 {
		t.Errorf("project headers %v", m)
	}
	if m := oai.OpenAIRateLimitHeaders(&core.RateLimitState{Present: true, TokensLimit: 10}); m["x-ratelimit-limit-project-tokens"] != "" {
		t.Errorf("no project limit: %v", m)
	}
}
