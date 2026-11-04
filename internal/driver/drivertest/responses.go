package drivertest

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The Responses API group (POST .../responses, spec 004 "Responses API",
// ADRs 0054 and 0055; task 239). It runs for every Responses route the
// driver declares and checks what every implementation shares (OpenAI,
// Azure and the two gateways that translate the API): the request parsed
// into messages and tools, the response object (object, id, status,
// output items, usage) and the event stream (response.created first,
// response.completed or response.incomplete last, consecutive
// sequence_number, deltas that add up to the output). The provider habits
// (event: lines, data: [DONE], state refused, id form) come from
// Profile.Responses.

// ResponsesProfile is the Responses API of a provider.
type ResponsesProfile struct {
	// EventLines: every frame carries "event: <type>" equal to data.type
	// (OpenAI, Azure, OpenCode); false requires none (OpenRouter).
	EventLines bool
	// Done: the stream ends with data: [DONE] (OpenRouter); false forbids it.
	Done bool
	// Stateless: the provider keeps no response, so previous_response_id
	// and store: true are refused (the gateways, ADR 0055); false requires
	// both accepted (the engine resolves the chain).
	Stateless bool
	// ID is the form of the response id (driver.IDMinter, KindResponses).
	ID *regexp.Regexp
}

var (
	reResp50 = regexp.MustCompile(`^resp_[0-9a-f]{50}$`)
	reResp32 = regexp.MustCompile(`^resp_[0-9a-f]{32}$`)
)

// responsesRoutes are the driver's Responses paths.
func (s *suite) responsesRoutes() []string {
	var out []string
	for _, r := range s.routes() {
		if r.Kind == driver.KindResponses {
			out = append(out, r.Path)
		}
	}
	return out
}

// testResponses runs the group on each Responses route.
func (s *suite) testResponses(t *testing.T) {
	for _, p := range s.responsesRoutes() {
		s.run(t, strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "_"), func(t *testing.T) {
			s.run(t, "Parse", func(t *testing.T) { s.respParse(t, p) })
			s.run(t, "Response", func(t *testing.T) { s.respResponse(t, p) })
			s.run(t, "Stream", func(t *testing.T) { s.respStream(t, p) })
			s.run(t, "Errors", func(t *testing.T) { s.respErrors(t, p) })
		})
	}
	s.run(t, "IDs", s.respIDs)
}

// respHeaders are the request headers of a call on path: the credential
// and the route the server stores in x-wwz-path.
func (s *suite) respHeaders(path string) driver.MapHeaders {
	h := driver.MapHeaders{"x-wwz-path": path}
	for k, v := range s.p.Credential {
		h[k] = v
	}
	return h
}

// respParse parses body on path and fails on an error.
func (s *suite) respParse(t *testing.T, path string) {
	tool := `{"type":"function","name":"get_weather","description":"Weather of a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`
	s.run(t, "string_input", func(t *testing.T) {
		req := s.respRequest(t, path, `{"model":"model-a","input":"Hi there","instructions":"Be brief.","max_output_tokens":64}`)
		if req.Stream {
			t.Error("Stream = true without stream")
		}
		if req.MaxTokens != 64 {
			t.Errorf("MaxTokens = %d, want 64 (max_output_tokens)", req.MaxTokens)
		}
		if !hasMessage(req.Messages, "user", "Hi there") {
			t.Errorf("input string not a user message: %+v", req.Messages)
		}
		if !hasContent(req.Messages, "Be brief.") {
			t.Errorf("instructions not counted as a message: %+v", req.Messages)
		}
	})
	s.run(t, "items_tool_roundtrip", func(t *testing.T) {
		body := `{"model":"model-a","stream":true,"tools":[` + tool + `],"input":[` +
			`{"role":"user","content":"Weather in Paris?"},` +
			`{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},` +
			`{"type":"function_call_output","call_id":"call_1","output":"22C"}]}`
		req := s.respRequest(t, path, body)
		if !req.Stream {
			t.Error("Stream = false with stream: true")
		}
		if got := toolNames(req.Tools); !eqStrings(got, []string{"get_weather"}) {
			t.Errorf("tools = %v, want [get_weather]", got)
		}
		var call, out bool
		for _, m := range req.Messages {
			if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call_1" && m.ToolCalls[0].Name == "get_weather" {
				call = true
			}
			if m.Role == "tool" && m.ToolCallID == "call_1" && m.Content == "22C" {
				out = true
			}
		}
		if !call || !out {
			t.Errorf("function_call/function_call_output not turned into an assistant call and a tool result: %+v", req.Messages)
		}
	})
	for name, body := range map[string]string{
		"invalid_json":  `{"model":"model-a","input":`,
		"missing_input": `{"model":"model-a"}`,
	} {
		s.run(t, name, func(t *testing.T) { s.respRefused(t, path, body) })
	}
	// State: OpenAI keeps the response and the engine resolves the chain;
	// a gateway refuses both fields (ADR 0055).
	for name, body := range map[string]string{
		"previous_response_id": `{"model":"model-a","input":"And now?","previous_response_id":"resp_0123456789abcdef0123456789abcdef"}`,
		"store_true":           `{"model":"model-a","input":"Hi","store":true}`,
	} {
		s.run(t, name, func(t *testing.T) {
			if s.p.Responses == nil {
				t.Skip("no Responses profile")
			}
			if s.p.Responses.Stateless {
				s.respRefused(t, path, body)
				return
			}
			s.respRequest(t, path, body)
		})
	}
}

// respRequest parses body on path and fails on an error.
func (s *suite) respRequest(t *testing.T, path, body string) *core.Request {
	t.Helper()
	req, e := s.d.ParseRequest(driver.KindResponses, []byte(body), s.respHeaders(path))
	if e != nil {
		t.Fatalf("ParseRequest(responses): %v\n%s", e, body)
	}
	if req == nil {
		t.Fatal("ParseRequest(responses): nil request and nil error")
	}
	if req.Driver != s.d.Name() {
		t.Errorf("Driver = %q, want %q", req.Driver, s.d.Name())
	}
	if req.Model != "model-a" {
		t.Errorf("Model = %q, want model-a", req.Model)
	}
	return req
}

// respRefused requires body to be refused with a 4xx the driver renders.
func (s *suite) respRefused(t *testing.T, path, body string) {
	t.Helper()
	req, e := s.d.ParseRequest(driver.KindResponses, []byte(body), s.respHeaders(path))
	if e == nil {
		t.Fatalf("ParseRequest accepted %s: %+v", body, req)
	}
	if e.Kind != core.ErrInvalidRequest {
		t.Errorf("error kind %s, want %s", e.Kind, core.ErrInvalidRequest)
	}
	w := core.NewMemWriter()
	s.d.WriteError(w, s.respErrReq(path), e, nil)
	// The body is the provider's (OpenCode answers a body that is not
	// JSON with an empty one, conferred); only the status is common.
	if w.StatusCode() != 400 {
		t.Errorf("WriteError: status %d, want 400; body %s", w.StatusCode(), w.Body())
	}
}

func hasMessage(ms []core.Message, role, content string) bool {
	for _, m := range ms {
		if m.Role == role && m.Content == content {
			return true
		}
	}
	return false
}

func hasContent(ms []core.Message, content string) bool {
	for _, m := range ms {
		if m.Content == content {
			return true
		}
	}
	return false
}

// respBase is the request every response case answers.
func (s *suite) respBase(t *testing.T, path string, stream bool) *core.Request {
	t.Helper()
	body := `{"model":"model-a","input":"Hi","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}},{"type":"function","name":"get_time","parameters":{"type":"object","properties":{}}}]`
	if stream {
		body += `,"stream":true`
	}
	req := s.respRequest(t, path, body+`}`)
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	req.Headers["x-wwz-path"] = path
	req.RequestID = s.requestID(driver.KindResponses, "req-1")
	req.Received = created
	return req
}

// respErrReq is the request of an error on path (no body parsed).
func (s *suite) respErrReq(path string) *core.Request {
	return &core.Request{
		Driver: s.d.Name(), Endpoint: core.EndpointResponses, Model: "model-a",
		RequestID: s.requestID(driver.KindResponses, "req-err"),
		Headers:   map[string]string{"x-wwz-path": path}, Received: created,
	}
}

func (s *suite) respResponse(t *testing.T, path string) {
	for _, c := range s.respCases(driver.KindResponses) {
		if c.finish == core.FinishRefusal {
			continue // the refusal part is not conferred on any provider
		}
		s.run(t, c.name, func(t *testing.T) {
			req := s.respBase(t, path, false)
			resp := &core.Response{
				ID: s.responseID(driver.KindResponses, "r1"), Model: "model-a", Created: created,
				Content: c.content, Reasoning: c.reasoning, ToolCalls: c.tools, FinishReason: c.finish,
				Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5, ReasoningTokens: 2, CachedTokens: 3},
			}
			w := core.NewMemWriter()
			if err := s.d.WriteResponse(w, req, resp, nil); err != nil {
				t.Fatal(err)
			}
			body := expectJSONResponse(t, w, 200)
			s.checkRespObject(t, body, resp, c.content, c.tools, c.finish)
		})
	}
}

// checkRespObject checks a finished response object against what was
// written.
func (s *suite) checkRespObject(t *testing.T, body map[string]any, resp *core.Response, text string, tools []core.ToolCall, fr core.FinishReason) {
	t.Helper()
	expectPath(t, body, "object", "response")
	expectPath(t, body, "id", resp.ID)
	expectPath(t, body, "model", resp.Model)
	if fr == core.FinishLength {
		expectPath(t, body, "status", "incomplete")
		expectPath(t, body, "incomplete_details.reason", "max_output_tokens")
	} else {
		expectPath(t, body, "status", "completed")
	}
	var gotText string
	var calls []map[string]any
	for _, it := range arr(body, "output") {
		switch str(it, "type") {
		case "message":
			expectPath(t, it, "role", "assistant")
			for _, part := range arr(it, "content") {
				if str(part, "type") == "output_text" {
					gotText += str(part, "text")
				}
			}
		case "function_call":
			calls = append(calls, it.(map[string]any))
		}
	}
	if gotText != text {
		t.Errorf("output_text = %q, want %q", gotText, text)
	}
	if len(calls) != len(tools) {
		t.Fatalf("%d function_call items, want %d: %v", len(calls), len(tools), arr(body, "output"))
	}
	for i, tc := range tools {
		expectPath(t, calls[i], "call_id", tc.ID)
		expectPath(t, calls[i], "name", tc.Name)
		if a := str(calls[i], "arguments"); !jsonEq(a, tc.Arguments) {
			t.Errorf("function_call %d arguments = %q, want %q", i, a, tc.Arguments)
		}
	}
	in, ok1 := num(body, "usage.input_tokens")
	out, ok2 := num(body, "usage.output_tokens")
	total, ok3 := num(body, "usage.total_tokens")
	switch {
	case !ok1 || !ok2 || !ok3:
		t.Errorf("usage without input/output/total tokens: %v", body["usage"])
	case total != in+out:
		t.Errorf("usage.total_tokens = %v, want input+output = %v", total, in+out)
	}
	expectPresent(t, body, "usage.output_tokens_details.reasoning_tokens")
	expectPresent(t, body, "usage.input_tokens_details.cached_tokens")
}

func (s *suite) respStream(t *testing.T, path string) {
	for _, c := range s.streamCases(driver.KindResponses) {
		s.run(t, c.name, func(t *testing.T) {
			w := core.NewMemWriter()
			req := s.respBase(t, path, true)
			// The engine finishes with req.IncludeUsage, which the
			// Responses API always sets (usage on every final event).
			c.includeUsage = req.IncludeUsage
			resp := &core.Response{ID: s.responseID(driver.KindResponses, "s"), Model: "model-a", Created: created, Usage: core.Usage{PromptTokens: c.usage.PromptTokens, CachedTokens: c.usage.CachedTokens}}
			drive(t, s.d.StreamWriter(w, req, nil), resp, c)
			if w.StatusCode() != 200 {
				t.Errorf("status %d, want 200", w.StatusCode())
			}
			if ct, _ := header(w.Headers(), "Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("Content-Type %q, want text/event-stream", ct)
			}
			s.checkRespStream(t, parseSSE(t, w.Body()), resp, c)
		})
	}
}

// checkRespStream checks the event sequence of one stream.
func (s *suite) checkRespStream(t *testing.T, frames []frame, resp *core.Response, c streamCase) {
	t.Helper()
	var data []frame
	for _, f := range frames {
		if f.Data != "" {
			data = append(data, f)
		}
	}
	if len(data) == 0 {
		t.Fatal("stream without data frames")
	}
	rp := s.p.Responses
	last := data[len(data)-1]
	switch {
	case last.Data == "[DONE]" && (rp == nil || rp.Done):
		data = data[:len(data)-1]
	case last.Data == "[DONE]":
		t.Error("stream ends with data: [DONE]; the Responses API sends none")
		data = data[:len(data)-1]
	case rp != nil && rp.Done:
		t.Error("stream does not end with data: [DONE]")
	}
	var events []map[string]any
	text := ""
	args := map[string]string{}
	var argOrder []string
	for i, f := range data {
		ev := decodeData(t, f)
		events = append(events, ev)
		typ := str(ev, "type")
		if typ == "" {
			t.Errorf("event %d without type: %s", i, f.Data)
		}
		switch {
		case rp != nil && rp.EventLines && f.Event != typ:
			t.Errorf("event %d: event line %q, data.type %q", i, f.Event, typ)
		case rp != nil && !rp.EventLines && f.Event != "":
			t.Errorf("event %d: event line %q, the provider sends none", i, f.Event)
		}
		if n, ok := num(ev, "sequence_number"); !ok || n != float64(i) {
			t.Errorf("event %d (%s): sequence_number %v, want %d", i, typ, ev["sequence_number"], i)
		}
		switch typ {
		case "response.output_text.delta":
			text += str(ev, "delta")
		case "response.function_call_arguments.delta":
			id := str(ev, "item_id")
			if _, ok := args[id]; !ok {
				argOrder = append(argOrder, id)
			}
			args[id] += str(ev, "delta")
		}
	}
	if typ := str(events[0], "type"); typ != "response.created" {
		t.Errorf("first event %s, want response.created", typ)
	}
	expectPath(t, events[0], "response.id", resp.ID)
	expectPath(t, events[0], "response.status", "in_progress")
	final := events[len(events)-1]
	want := "response.completed"
	if c.finish == core.FinishLength {
		want = "response.incomplete"
	}
	if typ := str(final, "type"); typ != want {
		t.Fatalf("last event %s, want %s", typ, want)
	}
	wantText := strings.Join(c.text, "")
	if text != wantText {
		t.Errorf("output_text deltas = %q, want %q", text, wantText)
	}
	if len(argOrder) != len(c.tools) {
		t.Errorf("arguments deltas for %d items, want %d", len(argOrder), len(c.tools))
	}
	var tools []core.ToolCall
	for i, tl := range c.tools {
		a := strings.Join(tl.args, "")
		if i < len(argOrder) && args[argOrder[i]] != a {
			t.Errorf("tool %d arguments deltas = %q, want %q", i, args[argOrder[i]], a)
		}
		tools = append(tools, core.ToolCall{ID: tl.id, Name: tl.name, Arguments: a})
	}
	r, _ := path(final, "response")
	obj, ok := r.(map[string]any)
	if !ok {
		t.Fatalf("%s without response object", want)
	}
	s.checkRespObject(t, obj, resp, wantText, tools, c.finish)
	expectPath(t, obj, "usage.output_tokens", c.usage.CompletionTokens)
}

// respErrors: WriteError on the Responses route keeps the status and puts
// the message in an error object.
func (s *suite) respErrors(t *testing.T, route string) {
	for _, c := range []struct {
		name   string
		err    core.Error
		status int
	}{
		{"invalid_request", core.Error{Kind: core.ErrInvalidRequest, Message: "bad input", Param: "input"}, 400},
		{"rate_limit", core.Error{Kind: core.ErrRateLimit, Message: "slow down", RetryAfter: 2 * time.Second, Extra: lim("rpm")}, 429},
		{"server_error", core.Error{Kind: core.ErrServerError, Message: "boom"}, 500},
	} {
		s.run(t, c.name, func(t *testing.T) {
			e := c.err
			w := core.NewMemWriter()
			s.d.WriteError(w, s.respErrReq(route), &e, nil)
			body := expectJSONResponse(t, w, c.status)
			if _, ok := path(body, "error"); !ok {
				t.Errorf("error body without error: %s", w.Body())
			}
		})
	}
}

func (s *suite) respIDs(t *testing.T) {
	m, ok := s.minter()
	if !ok || s.p.Responses == nil || s.p.Responses.ID == nil {
		t.Skip("no IDMinter or no Responses id form in the profile")
	}
	id := m.ResponseID(driver.KindResponses, generate.NewRNG(1, "a"), time.Now())
	if !s.p.Responses.ID.MatchString(id) {
		t.Errorf("ResponseID(responses) %q does not match %s", id, s.p.Responses.ID)
	}
	if id == m.ResponseID(driver.KindResponses, generate.NewRNG(2, "a"), time.Now()) {
		t.Error("ResponseID(responses) ignores the seed")
	}
}
