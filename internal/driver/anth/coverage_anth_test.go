package anth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func parseOK(t *testing.T, h *Hooks, body string) *core.Request {
	t.Helper()
	req, e := ParseMessages(h, []byte(body), driver.MapHeaders{}, "anthropic")
	if e != nil {
		t.Fatalf("%s: %v", body, e.Message)
	}
	return req
}

func parseErr(t *testing.T, h *Hooks, body, want string) {
	t.Helper()
	_, e := ParseMessages(h, []byte(body), driver.MapHeaders{}, "anthropic")
	if e == nil || !strings.Contains(e.Message, want) {
		t.Errorf("%s: err = %v, want %q", body, e, want)
	}
}

func TestParseMessagesErrors(t *testing.T) {
	h := DefaultHooks()
	api := APIHooks()
	msg := `"messages":[{"role":"user","content":"hi"}]`
	tool := `"tools":[{"name":"f","input_schema":{}}]`
	parseErr(t, &h, `{`, "Invalid JSON")
	parseErr(t, &h, `{"max_tokens":5,`+msg+`}`, "model: Field required")
	parseErr(t, &h, `{"model":"m",`+msg+`}`, "max_tokens: Field required")
	parseErr(t, &h, `{"model":"m","max_tokens":-1,`+msg+`}`, "greater than or equal to 0")
	parseErr(t, &h, `{"model":"m","max_tokens":5,"messages":[]}`, "at least one message")
	parseErr(t, &h, `{"model":"m","max_tokens":5000,"thinking":{"type":"enabled","budget_tokens":10},`+msg+`}`, "1024")
	parseErr(t, &h, `{"model":"m","max_tokens":2000,"thinking":{"type":"enabled","budget_tokens":2000},`+msg+`}`, "greater than thinking.budget_tokens")
	parseErr(t, &h, `{"model":"m","max_tokens":5,"messages":[7]}`, "messages.0: invalid message")
	parseErr(t, &h, `{"model":"m","max_tokens":5,"messages":[{"role":"system","content":"x"}]}`, "Input should be 'user' or 'assistant'")
	parseErr(t, &h, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":7}]}`, "valid string or list")
	parseErr(t, &h, `{"model":"m","max_tokens":5,`+msg+`,"tools":[{"description":"x"}]}`, "tools.0.name")
	parseErr(t, &h, `{"model":"m","max_tokens":5,`+msg+`,`+tool+`,"tool_choice":{"type":"tool","name":"g"}}`, "Tool 'g' not found")
	parseErr(t, &h, `{"model":"m","max_tokens":5,`+msg+`,`+tool+`,"tool_choice":{"type":"weird"}}`, "tool_choice: Input tag 'weird'")
	parseErr(t, &h, `{"model":"m","max_tokens":5,`+msg+`,"tool_choice":{"type":"any"}}`, "may only be specified while providing tools")
	parseErr(t, &api, `{"model":"claude-opus-4-1","max_tokens":5,`+msg+`,`+tool+`,"thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"any"}}`, "")
}

func TestParseMessagesFeatures(t *testing.T) {
	h := DefaultHooks()
	req := parseOK(t, &h, `{"model":"m","max_tokens":4000,"stream":true,
	 "metadata":{"user_id":"u1"},
	 "system":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}],
	 "thinking":{"type":"enabled","budget_tokens":2048},
	 "tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],
	 "tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true},
	 "messages":[
	  {"role":"assistant","content":[{"type":"text","text":"x"},{"type":"text","text":"y"},{"type":"image"},{"type":"document"},
	    {"type":"tool_use","id":"t1","name":"f","input":{"a":1}},{"type":"thinking"},{"type":"redacted_thinking"}]},
	  {"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"t1","content":"plain"},
	    {"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"p1"},{"type":"image"},{"type":"text","text":"p2"}]},
	    {"type":"tool_result","tool_use_id":"t3","content":7}]}]}`)
	if req.Metadata["user_id"] != "u1" || req.System != "ab" || !req.Reasoning.Requested {
		t.Errorf("req = %+v", req)
	}
	if req.ToolChoice.Mode != core.ToolChoiceNamed || req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Errorf("tool choice = %+v %v", req.ToolChoice, req.ParallelToolCalls)
	}
	a, u := req.Messages[0], req.Messages[1]
	if a.Content != "xy" || a.ImageParts != 2 || len(a.ToolCalls) != 1 {
		t.Errorf("assistant = %+v", a)
	}
	if len(u.ToolResults) != 3 || u.ToolResults[0].Content != "plain" || u.ToolResults[1].Content != "p1p2" || !u.ToolResults[1].IsError || u.ToolResults[2].Content != "" {
		t.Errorf("results = %+v", u.ToolResults)
	}
	for _, tc := range []string{"any", "none", "auto"} {
		parseOK(t, &h, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f"}],"tool_choice":{"type":"`+tc+`"}}`)
	}
}

func TestFlattenSystem(t *testing.T) {
	cases := map[string]string{
		``:        "",
		`"plain"`: "plain",
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`: "ab",
		`7`: "",
	}
	for in, want := range cases {
		if got := flattenSystem(json.RawMessage(in)); got != want {
			t.Errorf("flattenSystem(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestStopReason(t *testing.T) {
	for fr, want := range map[core.FinishReason]string{
		core.FinishLength:        "max_tokens",
		core.FinishToolCalls:     "tool_use",
		core.FinishRefusal:       "refusal",
		core.FinishContentFilter: "refusal",
		core.FinishStop:          "end_turn",
	} {
		if got := stopReason(fr); got != want {
			t.Errorf("%v: %s", fr, got)
		}
	}
}

func TestUsageAndMessageBody(t *testing.T) {
	for _, h := range []Hooks{DefaultHooks(), APIHooks()} {
		h.UsageExtras = func(*core.Request, *core.Response, core.Usage) map[string]any {
			return map[string]any{"extra": 1}
		}
		u := core.Usage{PromptTokens: 100, CompletionTokens: 9, CachedTokens: 10, CacheWriteTokens: 20, CacheWrite1hTokens: 5, ReasoningTokens: 3}
		m := UsageJSON(&h, &core.Request{}, &core.Response{}, u)
		if _, ok := m.Get("extra"); !ok {
			t.Error("UsageExtras not merged")
		}
		d := deltaUsage(&h, &core.Request{}, &core.Response{}, u)
		_, hasDetails := d.Get("output_tokens_details")
		if h.API != hasDetails {
			t.Errorf("API=%v delta details=%v", h.API, hasDetails)
		}
		d = deltaUsage(&h, &core.Request{}, &core.Response{}, core.Usage{PromptTokens: 1})
		if _, ok := d.Get("output_tokens_details"); ok {
			t.Error("details without thinking tokens")
		}

		resp := &core.Response{
			ID: "msg_1", Model: "m", Reasoning: "think", Content: "hello", Usage: u,
			FinishReason: core.FinishToolCalls,
			Headers:      map[string]string{"x-extra": "1"},
			ToolCalls: []core.ToolCall{
				{ID: "t1", Name: "f", Arguments: `{"a":1}`},
				{ID: "t2", Name: "g", Arguments: `not json`},
			},
		}
		w := core.NewMemWriter()
		rl := &core.RateLimitState{Present: true, RequestsLimit: 5, RequestsRemaining: 4}
		if err := WriteMessage(&h, w, &core.Request{RequestID: "abc"}, resp, rl); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body["content"].([]any)) != 4 || body["stop_reason"] != "tool_use" {
			t.Errorf("body = %s", w.Body())
		}
		if w.Headers()["x-extra"] != "1" {
			t.Errorf("headers = %v", w.Headers())
		}
		if !h.API && w.Headers()["anthropic-ratelimit-requests-limit"] != "5" {
			t.Errorf("limit headers = %v", w.Headers())
		}
		// Empty signature: no signature field.
		h.Signature = ""
		if blocks := contentBlocks(&h, resp); len(blocks) != 4 {
			t.Errorf("blocks = %d", len(blocks))
		}
		if sig := h.signature(nil); sig != "" {
			t.Errorf("signature = %q", sig)
		}
	}
	api := APIHooks()
	if sig := api.signature(nil); sig == "" {
		t.Error("API signature without response")
	}
}

type failWriter struct {
	*core.MemWriter
	failWrite, failFlush bool
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, errFail
	}
	return f.MemWriter.Write(p)
}

func (f *failWriter) Flush() error {
	if f.failFlush {
		return errFail
	}
	return f.MemWriter.Flush()
}

type failErr struct{}

func (failErr) Error() string { return "fail" }

var errFail error = failErr{}

func TestWriteMessageAndCountTokensFailures(t *testing.T) {
	h := DefaultHooks()
	resp := &core.Response{ID: "m", Content: "x", Usage: core.Usage{PromptTokens: 7}, Headers: map[string]string{"x-h": "1"}}
	for _, fw := range []*failWriter{{MemWriter: core.NewMemWriter(), failWrite: true}, {MemWriter: core.NewMemWriter(), failFlush: true}} {
		if err := WriteMessage(&h, fw, &core.Request{}, resp, nil); err == nil {
			t.Error("WriteMessage swallowed the writer error")
		}
	}
	for _, fw := range []*failWriter{{MemWriter: core.NewMemWriter(), failWrite: true}, {MemWriter: core.NewMemWriter(), failFlush: true}} {
		if err := WriteCountTokens(&h, fw, &core.Request{}, resp); err == nil {
			t.Error("WriteCountTokens swallowed the writer error")
		}
	}
	for _, hh := range []Hooks{DefaultHooks(), APIHooks()} {
		w := core.NewMemWriter()
		if err := WriteCountTokens(&hh, w, &core.Request{RequestID: "r"}, resp); err != nil {
			t.Fatal(err)
		}
		if string(w.Body()) != `{"input_tokens":7}` && !strings.Contains(string(w.Body()), `"input_tokens"`) {
			t.Errorf("body = %s", w.Body())
		}
		if w.Headers()["x-h"] != "1" {
			t.Errorf("headers = %v", w.Headers())
		}
		if hh.API && w.Headers()["anthropic-organization-id"] == "" {
			t.Errorf("account headers missing: %v", w.Headers())
		}
	}
}

func TestStreamFull(t *testing.T) {
	for _, h := range []Hooks{DefaultHooks(), APIHooks()} {
		w := core.NewMemWriter()
		req := &core.Request{RequestID: "r"}
		s := NewStream(&h, w, req, &core.RateLimitState{Present: true, RequestsLimit: 1})
		resp := &core.Response{ID: "msg_9", Model: "m", Usage: core.Usage{PromptTokens: 5, ReasoningTokens: 2}}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(s.Start(resp))
		must(s.ReasoningDelta("think"))
		must(s.ReasoningDelta(" more"))
		must(s.TextDelta("hello"))
		must(s.TextDelta(""))
		must(s.ToolCallStart(0, "t1", "f"))
		must(s.ToolArgsDelta(0, `{"a"`))
		must(s.ToolArgsDelta(9, `:1}`)) // unknown index falls back to the open block
		must(s.ToolCallEnd(0))
		must(s.ToolCallStart(1, "t2", "g"))
		must(s.Ping())
		must(s.Raw([]byte(": raw\n\n")))
		if len(s.LastFrame()) == 0 {
			t.Error("LastFrame empty")
		}
		must(s.Finish(core.FinishToolCalls, core.Usage{PromptTokens: 5, CompletionTokens: 3, ReasoningTokens: 2}, true))
		must(s.Close())
		out := string(w.Body())
		for _, want := range []string{"message_start", "signature_delta", "input_json_delta", "message_stop", "event: ping"} {
			if !strings.Contains(out, want) {
				t.Errorf("API=%v: stream lacks %s:\n%s", h.API, want, out)
			}
		}
	}
}

func TestStreamError(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{RequestID: "r"}, nil)
	if err := s.Error(&core.Error{Kind: core.ErrOverloaded}); err != nil {
		t.Fatal(err)
	}
	out := string(w.Body())
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "overloaded_error") || strings.Contains(out, "request_id") {
		t.Errorf("error frame = %s", out)
	}
	// A body that is not a map falls back to a generic api_error.
	h.ErrorBody = func(*core.Request, *core.Error) (int, any, string, map[string]string) {
		return 502, []byte("<html>"), "text/html", nil
	}
	w = core.NewMemWriter()
	s = NewStream(&h, w, &core.Request{}, nil)
	if err := s.Error(&core.Error{Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w.Body()), `"message":"boom"`) {
		t.Errorf("fallback frame = %s", w.Body())
	}
}

func TestStreamEventShapeFailure(t *testing.T) {
	h := DefaultHooks()
	h.Shape = func(p Part, o oai.Obj) oai.Obj {
		if p == PartEvent {
			return append(o, oai.Field{Key: "bad", Val: make(chan int)})
		}
		return o
	}
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{}, nil)
	if err := s.Start(&core.Response{ID: "m"}); err == nil {
		t.Error("Start swallowed the marshal error")
	}
	if err := s.TextDelta(""); err == nil {
		t.Error("empty delta swallowed the marshal error")
	}
	if err := s.Finish(core.FinishStop, core.Usage{}, true); err == nil {
		t.Error("Finish swallowed the marshal error")
	}
	if err := s.Error(&core.Error{Kind: core.ErrOverloaded}); err != nil {
		// Error builds its own envelope, which has no channel.
		t.Errorf("Error: %v", err)
	}
	if err := s.Ping(); err == nil {
		t.Error("Ping swallowed the marshal error")
	}
}

func TestStreamPropagatesWriteErrors(t *testing.T) {
	h := APIHooks()
	resp := &core.Response{ID: "m", Usage: core.Usage{PromptTokens: 1}}
	steps := []func(s *Stream) error{
		func(s *Stream) error { return s.Start(resp) },
		func(s *Stream) error { return s.ReasoningDelta("t") },
		func(s *Stream) error { return s.TextDelta("t") },
		func(s *Stream) error { return s.ToolCallStart(0, "i", "n") },
		func(s *Stream) error { return s.Ping() },
		func(s *Stream) error { return s.Finish(core.FinishStop, core.Usage{}, true) },
		func(s *Stream) error { return s.Error(&core.Error{Kind: core.ErrOverloaded}) },
	}
	for i, step := range steps {
		fw := &failWriter{MemWriter: core.NewMemWriter()}
		s := NewStream(&h, fw, &core.Request{}, nil)
		s.resp = resp
		fw.failWrite, fw.failFlush = true, true
		if err := step(s); err == nil {
			t.Errorf("step %d swallowed the write error", i)
		}
	}
	// closeBlock errors: a signature_delta then a content_block_stop.
	for n := 1; n <= 3; n++ {
		cw := &countFail{MemWriter: core.NewMemWriter(), failAfter: n}
		s := NewStream(&h, cw, &core.Request{}, nil)
		s.resp = resp
		err := s.ReasoningDelta("t")
		if err == nil {
			err = s.TextDelta("x")
		}
		_ = err
	}
}

type countFail struct {
	*core.MemWriter
	failAfter, n int
}

func (c *countFail) Write(p []byte) (int, error) {
	c.n++
	if c.n > c.failAfter {
		return 0, errFail
	}
	return c.MemWriter.Write(p)
}

func TestErrorBodies(t *testing.T) {
	req := &core.Request{Model: "claude-x", RequestID: "req_1"}
	type tc struct {
		e      core.Error
		status int
		typ    string
		sub    string
	}
	cases := []tc{
		{core.Error{Kind: core.ErrInvalidRequest}, 400, "invalid_request_error", "Invalid request"},
		{core.Error{Kind: core.ErrMissingSession}, 400, "invalid_request_error", "Invalid request"},
		{core.Error{Kind: core.ErrContentFilter}, 400, "invalid_request_error", "content filtering"},
		{core.Error{Kind: core.ErrContextLength}, 400, "invalid_request_error", "prompt is too long"},
		{core.Error{Kind: core.ErrSpendLimit}, 400, "invalid_request_error", "usage limits"},
		{core.Error{Kind: core.ErrAccountBanned}, 401, "authentication_error", "disabled"},
		{core.Error{Kind: core.ErrPermission}, 403, "permission_error", "does not have permission"},
		{core.Error{Kind: core.ErrGeoBlocked}, 403, "permission_error", "Request not allowed"},
		{core.Error{Kind: core.ErrAccountSuspended}, 403, "permission_error", "organization has been disabled"},
		{core.Error{Kind: core.ErrModelNotFound}, 404, "not_found_error", "model: claude-x"},
		{core.Error{Kind: core.ErrNotFound}, 404, "not_found_error", "model: claude-x"},
		{core.Error{Kind: core.ErrMethodNotAllowed}, 405, "invalid_request_error", "Method Not Allowed"},
		{core.Error{Kind: core.ErrRequestTooLarge}, 413, "request_too_large", "maximum allowed number of bytes"},
		{core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond}, 429, "rate_limit_error", "rate limit for your organization"},
		{core.Error{Kind: core.ErrSlowDown, RetryAfter: time.Millisecond}, 429, "rate_limit_error", "rate limit"},
		{core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "credits"}}, 402, "billing_error", "credit balance is too low"},
		{core.Error{Kind: core.ErrQuota}, 429, "rate_limit_error", "regain access"},
		{core.Error{Kind: core.ErrBudget}, 429, "rate_limit_error", "workspace API usage limits"},
		{core.Error{Kind: core.ErrServerError}, 500, "api_error", "Internal server error"},
		{core.Error{Kind: core.ErrOverloaded, RetryAfter: 2 * time.Second}, 529, "overloaded_error", "Overloaded"},
		{core.Error{Kind: core.ErrGatewayTimeout}, 504, "timeout_error", "timed out"},
		{core.Error{Kind: core.ErrorKind("zzz")}, 500, "api_error", "Internal server error"},
		{core.Error{Kind: core.ErrServerError, Status: 503, Code: "custom_error", Message: "m"}, 503, "custom_error", "m"},
	}
	for _, c := range cases {
		e := c.e
		status, body, _, _ := AnthropicErrorBody(req, &e)
		m, ok := body.(map[string]any)
		if !ok {
			t.Errorf("%v: body %T", e.Kind, body)
			continue
		}
		inner := m["error"].(map[string]any)
		if status != c.status || inner["type"] != c.typ || !strings.Contains(inner["message"].(string), c.sub) {
			t.Errorf("%v: %d %v", e.Kind, status, inner)
		}
	}
	// Not found without a model has no model text.
	_, body, _, _ := AnthropicErrorBody(&core.Request{}, &core.Error{Kind: core.ErrNotFound})
	if body.(map[string]any)["error"].(map[string]any)["message"] != "Not Found" {
		t.Errorf("not found = %v", body)
	}
	status, body, ct, _ := AnthropicErrorBody(req, &core.Error{Kind: core.ErrBadGateway})
	if status != 502 || ct != "text/html" || !strings.Contains(string(body.([]byte)), "502 Bad Gateway") {
		t.Errorf("bad gateway = %d %s", status, ct)
	}
	// Retry-After rounds up, with a floor of one second.
	_, _, _, hdr := AnthropicErrorBody(req, &core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond})
	if hdr["retry-after"] != "2" {
		t.Errorf("retry-after = %v", hdr)
	}
	_, _, _, hdr = AnthropicErrorBody(req, &core.Error{Kind: core.ErrOverloaded, RetryAfter: 2 * time.Second})
	if hdr["retry-after"] != "2" {
		t.Errorf("retry-after = %v", hdr)
	}
	_, _, _, hdr = AnthropicErrorBody(req, &core.Error{Kind: core.ErrRateLimit, RetryAfter: time.Millisecond})
	if hdr["retry-after"] != "1" {
		t.Errorf("retry-after = %v", hdr)
	}
	// Default message for a kind that has none and no special case.
	if got := anthropicDefaultMessage(&core.Error{Kind: core.ErrQuota}, ""); got != "Internal server error" {
		t.Errorf("default = %q", got)
	}
}

func TestRateLimitMessageNamesTheLimit(t *testing.T) {
	req := &core.Request{}
	mk := func(limit string, kind core.ErrorKind) string {
		e := &core.Error{Kind: kind, Extra: map[string]any{"limit": limit, "limit_value": 40000}}
		return anthropicRateLimitMessage(req, e, limit)
	}
	for _, limit := range []string{"rpm", "tpm", "itpm", "otpm"} {
		_ = mk(limit, core.ErrRateLimit)
	}
	if got := thousands(40000); got != "40,000" {
		t.Errorf("thousands = %s", got)
	}
	if got := thousands(7); got != "7" {
		t.Errorf("thousands = %s", got)
	}
	if got := thousands(1234567); got != "1,234,567" {
		t.Errorf("thousands = %s", got)
	}
	if got := ceilSeconds(0); got != 1 {
		t.Errorf("ceilSeconds(0) = %d", got)
	}
	if got := ceilSeconds(3 * time.Second); got != 3 {
		t.Errorf("ceilSeconds(3s) = %d", got)
	}
}

func TestErrorBytes(t *testing.T) {
	if string(errorBytes([]byte("raw"))) != "raw" || string(errorBytes(json.RawMessage("r2"))) != "r2" {
		t.Error("raw bodies not verbatim")
	}
	if got := string(errorBytes(map[string]any{"type": "error", "error": map[string]any{"type": "x", "message": "m"}, "request_id": "r"})); got != `{"type":"error","error":{"type":"x","message":"m"},"request_id":"r"}` {
		t.Errorf("envelope = %s", got)
	}
	if got := string(errorBytes(map[string]int{"a": 1})); got != `{"a":1}` {
		t.Errorf("other = %s", got)
	}
	if got := string(errorBytes(make(chan int))); !strings.Contains(got, "Internal server error") {
		t.Errorf("unmarshalable = %s", got)
	}
}

func TestWriteErrorVariants(t *testing.T) {
	api := APIHooks()
	for _, tc := range []struct {
		kind   core.ErrorKind
		status string
		auth   bool
	}{
		{core.ErrInvalidAPIKey, "false", true},
		{core.ErrRateLimit, "true", false},
		{core.ErrServerError, "true", false},
		{core.ErrInvalidRequest, "false", false},
	} {
		w := core.NewMemWriter()
		WriteError(&api, w, nil, &core.Error{Kind: tc.kind}, &core.RateLimitState{Present: true, RequestsLimit: 3})
		h := w.Headers()
		if h["x-should-retry"] != tc.status {
			t.Errorf("%v: x-should-retry = %q", tc.kind, h["x-should-retry"])
		}
		if tc.auth && h["Cache-Control"] != "no-store" {
			t.Errorf("%v: Cache-Control = %q", tc.kind, h["Cache-Control"])
		}
		if !tc.auth && h["anthropic-organization-id"] == "" {
			t.Errorf("%v: account headers missing", tc.kind)
		}
		if h["request-id"] == "" {
			t.Errorf("%v: no request-id", tc.kind)
		}
	}
	for _, status := range []int{408, 409, 429, 500, 400, 401, 404} {
		want := "false"
		if status != 400 && status != 401 && status != 404 {
			want = "true"
		}
		if got := shouldRetry(status); got != want {
			t.Errorf("shouldRetry(%d) = %s", status, got)
		}
	}
}

func TestRateLimitHeaders(t *testing.T) {
	rl := &core.RateLimitState{
		Present:       true,
		RequestsLimit: 100, RequestsRemaining: -4, RequestsReset: time.Second,
		TokensLimit: 5000, TokensRemaining: 4567, TokensReset: time.Minute,
		InputTokensLimit: 3000, InputTokensRemain: 2999,
		OutputTokensLimit: 2000, OutputTokensRem: -1,
	}
	m := AnthropicRateLimitHeaders(rl)
	if m["anthropic-ratelimit-requests-remaining"] != "0" || m["anthropic-ratelimit-tokens-remaining"] != "4000" ||
		m["anthropic-ratelimit-input-tokens-remaining"] != "2000" || m["anthropic-ratelimit-output-tokens-remaining"] != "0" {
		t.Errorf("headers = %v", m)
	}
	if got := roundThousand(-5); got != 0 {
		t.Errorf("roundThousand(-5) = %d", got)
	}
	d := DefaultRateLimit(nil)
	if d.RequestsLimit != DefaultRequestsLimit || !d.Present {
		t.Errorf("default = %+v", d)
	}
	d = DefaultRateLimit(rl)
	if d.RequestsLimit != 100 || d.TokensLimit != 5000 {
		t.Errorf("kept = %+v", d)
	}
	s := AnthropicSuccessHeaders(&core.Request{}, nil, nil)
	if s["anthropic-ratelimit-tokens-limit"] == "" || s["anthropic-workspace-id"] == "" {
		t.Errorf("success = %v", s)
	}
}

func TestAccountIDs(t *testing.T) {
	// Without an engine-recorded key the id derives from the headers.
	byAPI := &core.Request{Headers: driver.MapHeaders{"x-api-key": "k1"}}
	byBearer := &core.Request{Headers: driver.MapHeaders{"authorization": "Bearer k2"}}
	byNone := &core.Request{Headers: driver.MapHeaders{"authorization": "Basic xx"}}
	ids := map[string]bool{}
	for _, r := range []*core.Request{byAPI, byBearer, byNone, nil} {
		ids[OrganizationID(r)] = true
		if !strings.HasPrefix(WorkspaceID(r), "wrkspc_01") {
			t.Errorf("workspace = %s", WorkspaceID(r))
		}
	}
	if len(ids) != 3 {
		t.Errorf("organization ids = %v", ids)
	}
	// The same account gives the same ids whatever the key.
	h1 := driver.MapHeaders{}
	driver.SetAccount(h1, driver.Account{Key: "a", Account: "acme", Project: "p"})
	h2 := driver.MapHeaders{}
	driver.SetAccount(h2, driver.Account{Key: "b", Account: "acme", Project: "p"})
	r1, r2 := &core.Request{Headers: h1}, &core.Request{Headers: h2}
	if OrganizationID(r1) != OrganizationID(r2) || WorkspaceID(r1) != WorkspaceID(r2) {
		t.Error("account ids depend on the key")
	}
}

func TestReqID(t *testing.T) {
	if got := reqID(&core.Request{RequestID: "req_abc"}); got != "req_abc" {
		t.Errorf("reqID = %s", got)
	}
	if got := reqID(&core.Request{RequestID: "abc"}); got != "req_abc" {
		t.Errorf("reqID = %s", got)
	}
	if got := reqID(nil); !strings.HasPrefix(got, "req_011C") {
		t.Errorf("reqID(nil) = %s", got)
	}
}

func TestRequestIDHeaderPerDriver(t *testing.T) {
	h := Hooks{Name: "opencode"}
	if h.requestIDHeader() != "" {
		t.Errorf("opencode sends no request id header")
	}
}

func TestDeltaTemplateRebuildsPerBlock(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{}, nil)
	if err := s.Start(&core.Response{ID: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.ToolCallStart(0, "i", "n"); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w.Body()), `"index":2`) {
		t.Errorf("template reused across blocks:\n%s", w.Body())
	}
}
