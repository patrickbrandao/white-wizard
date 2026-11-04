package oai

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestParseResponses(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{not json`, "invalid_json"},
		{`{"input":"x"}`, "missing_required_parameter"},
		{`{"model":"m"}`, "missing_required_parameter"},
		{`{"model":"m","input":null}`, "missing_required_parameter"},
		{`{"model":"m","input":5}`, "invalid_type"},
		{`{"model":"m","input":"x","tools":[5]}`, "invalid_type"},
	} {
		if _, e := ParseResponses([]byte(tc.body), "openai"); e == nil || e.Code != tc.code {
			t.Errorf("%s: %+v, want %s", tc.body, e, tc.code)
		}
	}
	req, e := ParseResponses([]byte(`{"model":"m","instructions":"Be brief.","max_output_tokens":32,"stream":true,
		"metadata":{"a":"1"},"user":"u","reasoning":{"effort":"low"},
		"text":{"format":{"type":"json_schema","name":"o","schema":{"type":"object"}}},
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}},{"type":"web_search"}],
		"tool_choice":{"type":"function","name":"f"},
		"input":[{"role":"developer","content":"dev"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"x"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"reasoning","id":"rs_1","summary":[]},{"type":"item_reference","id":"msg_1"}]}`), "openai")
	if e != nil {
		t.Fatal(e)
	}
	if req.Endpoint != core.EndpointResponses || !req.Stream || !req.IncludeUsage || req.MaxTokens != 32 || req.System != "" || req.SystemPrompt() != "Be brief." {
		t.Errorf("request %+v", req)
	}
	if req.Metadata["a"] != "1" || req.Metadata["user"] != "u" || !req.Reasoning.Requested || req.ResponseFormat != "json_schema" || string(req.ResponseSchema) != `{"type":"object"}` {
		t.Errorf("fields %+v", req)
	}
	if len(req.Tools) != 1 || req.ToolChoice.Mode != core.ToolChoiceNamed {
		t.Errorf("tools %+v %+v", req.Tools, req.ToolChoice)
	}
	// strict omitted and fitting: arguments come from the normalized schema.
	if got := string(req.Tools[0].Parameters); got != `{"type":"object","additionalProperties":false,"required":[]}` {
		t.Errorf("strict parameters %s", got)
	}
	for raw, want := range map[string]string{
		`{"type":"function","name":"f","strict":true}`:                                                                                   ``,
		`{"type":"function","name":"f","strict":false,"parameters":{"type":"object"}}`:                                                   `{"type":"object"}`,
		`{"type":"function","name":"f","parameters":{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}}}}`: `{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}}}`,
	} {
		var tl responsesTool
		_ = json.Unmarshal([]byte(raw), &tl)
		if got := string(strictParameters(json.RawMessage(raw), tl.Parameters)); got != want {
			t.Errorf("%s: %s", raw, got)
		}
	}
	if got := strictParameters(json.RawMessage(`5`), nil); got != nil {
		t.Errorf("not an object: %s", got)
	}
	roles := []string{}
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,system,user,assistant,tool" || len(req.Messages[3].ToolCalls) != 2 || req.Messages[2].ImageParts != 1 || req.Messages[4].Content != "ok" {
		t.Errorf("messages %v %+v", roles, req.Messages)
	}
	req, _ = ParseResponses([]byte(`{"model":"m","input":"x","tool_choice":{"type":"function","name":"nope"},"reasoning":{"effort":"none"},"text":{"format":{"type":"text"}}}`), "openai")
	if req.ToolChoice.Mode != core.ToolChoiceAuto || req.Reasoning.Requested || req.ResponseFormat != "" {
		t.Errorf("defaults %+v", req)
	}
}

func TestJSONKind(t *testing.T) {
	for raw, want := range map[string]string{`{}`: "an object", `[]`: "an array", `true`: "a boolean", `null`: "null", `"s"`: "a string", `1`: "a number"} {
		if got := jsonKind(json.RawMessage(raw)); got != want {
			t.Errorf("%s: %s", raw, got)
		}
	}
}

func TestResponsesIDs(t *testing.T) {
	now := time.Unix(0x6ae5f0ed, 0)
	id := ResponsesID(generate.NewRNG(1, "x"), now)
	if !regexp.MustCompile(`^resp_[0-9a-f]{16}016ae5f0ed[0-9a-f]{24}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	msg := itemID("msg", id, now, 0)
	if msg[4:20] != id[5:21] || len(msg) != 54 || msg == itemID("msg", id, now, 1) {
		t.Errorf("item id %q of %q", msg, id)
	}
	if got := string(appendTimeHex(nil, time.Unix(5, 0))); got != "00000005" {
		t.Errorf("time hex %q", got)
	}
}

func TestEchoTool(t *testing.T) {
	cases := []struct{ in, want string }{
		{`5`, `5`},
		{`{"type":"web_search"}`, `{"type":"web_search"}`},
		{`{"type":"function","name":"f","strict":false,"parameters":{"type":"object"}}`, `{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"type":"object"},"strict":false}`},
		{
			`{"type":"function","name":"f","parameters":{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"b":{"anyOf":[{"type":"object","properties":{}}]}}}}},"$defs":{"d":{"type":"string"}}}}`,
			`{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"b":{"anyOf":[{"type":"object","properties":{},"additionalProperties":false,"required":[]}]}},"additionalProperties":false,"required":["b"]}}},"$defs":{"d":{"type":"string"}},"additionalProperties":false,"required":["a"]},"strict":true}`,
		},
		{`{"type":"function","name":"f","parameters":{"type":"object"}}`, `{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"type":"object","additionalProperties":false,"required":[]},"strict":true}`},
		// Not fitting at any depth: echoed as sent, strict false.
		{`{"type":"function","name":"f","parameters":{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}}}}`, `{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}}},"strict":false}`},
		{`{"type":"function","name":"f","parameters":{"type":"array","items":{"type":"object","additionalProperties":{}}}}`, `{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"type":"array","items":{"type":"object","additionalProperties":{}}},"strict":false}`},
		{`{"type":"function","name":"f","parameters":{"anyOf":[{"type":"object","additionalProperties":true}]}}`, `{"type":"function","description":null,"name":"f","output_schema":null,"parameters":{"anyOf":[{"type":"object","additionalProperties":true}]},"strict":false}`},
	}
	for _, tc := range cases {
		b, err := Format{RawHTML: true}.Marshal(echoTool(json.RawMessage(tc.in)))
		if err != nil || string(b) != tc.want {
			t.Errorf("%s\n got  %s\n want %s", tc.in, b, tc.want)
		}
	}
}

func TestEchoFormatAndValues(t *testing.T) {
	for in, want := range map[string]string{
		`5`:                      `5`,
		`{"type":"json_object"}`: `{"type":"json_object"}`,
		`{"type":"json_schema","name":"o","schema":{}}`: `{"type":"json_schema","description":null,"name":"o","schema":{}}`,
	} {
		b, _ := Format{RawHTML: true}.Marshal(echoFormat([]byte(in)))
		if string(b) != want {
			t.Errorf("%s: %s", in, b)
		}
	}
	if v, ok := decodeOrdered([]byte(`{"a":`)).(json.RawMessage); !ok || string(v) != `{"a":` {
		t.Errorf("invalid: %v", v)
	}
	if v, ok := decodeOrdered([]byte(`[1,{"a":}]`)).(json.RawMessage); !ok || v == nil {
		t.Errorf("invalid array: %v", v)
	}
	if v := floatOr(json.RawMessage(`"x"`), 1); v != "x" {
		t.Errorf("non-number: %v", v)
	}
	if v := floatOr(json.RawMessage(`null`), 1); string(v.(json.RawMessage)) != "1.0" {
		t.Errorf("null: %v", v)
	}
}

// streamReq is a Responses request asking for a reasoning summary and the
// encrypted reasoning.
func streamReq(raw string) *core.Request {
	return &core.Request{Endpoint: core.EndpointResponses, Raw: []byte(raw), Received: time.Now()}
}

func respFixed() *core.Response {
	return &core.Response{ID: "resp_0123456789abcdef016ae5f0ed0000000000000000000000ab", Model: "m", Created: time.Unix(1793454317, 0)}
}

func TestRespStreamReasoningAndTools(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewRespStream(&h, w, streamReq(`{"reasoning":{"summary":"auto"},"include":["reasoning.encrypted_content"],"stream_options":{"include_obfuscation":false},"service_tier":"flex"}`), nil)
	steps := []error{
		s.Start(respFixed()), s.ReasoningDelta("think"), s.ReasoningDelta("ing"), s.Ping(),
		s.TextDelta("Hi"), s.ToolCallStart(0, "call_1", "f"), s.ToolArgsDelta(0, "{}"), s.ToolArgsDelta(9, "x"), s.ToolCallEnd(9),
		s.ToolCallStart(1, "call_2", "g"), s.Finish(core.FinishToolCalls, core.Usage{PromptTokens: 5, CompletionTokens: 7, ReasoningTokens: 2}, true),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	body := string(w.Body())
	for _, want := range []string{
		`"service_tier":"flex"`, "event: response.reasoning_summary_part.added", `"type":"response.reasoning_summary_text.delta","delta":"think"`,
		`"type":"response.reasoning_summary_text.done","item_id":"rs_`, `"text":"thinking"`, `"encrypted_content":"gAAAAA`,
		": ping", `"type":"function_call","status":"completed","arguments":"{}","call_id":"call_1","name":"f"`,
		`"arguments":"","call_id":"call_2","name":"g"}`, `"output_index":3`, `"reasoning_tokens":2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "obfuscation") {
		t.Error("include_obfuscation: false")
	}
	if len(s.LastFrame()) == 0 || s.Raw([]byte("x")) != nil || s.Close() != nil {
		t.Error("raw / last frame")
	}
}

func TestRespStreamIncompleteEmptyAndError(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewRespStream(&h, w, streamReq(`{"stream":true}`), nil)
	_ = s.Start(respFixed())
	if err := s.Finish(core.FinishContentFilter, core.Usage{}, false); err != nil {
		t.Fatal(err)
	}
	body := string(w.Body())
	for _, want := range []string{`"status":"incomplete","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}]`, `"incomplete_details":{"reason":"content_filter"}`, `"usage":null`, "event: response.incomplete"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}

	w = core.NewMemWriter()
	s = NewRespStream(&h, w, streamReq(`{}`), nil)
	_ = s.Start(respFixed())
	_ = s.TextDelta("No.")
	_ = s.Finish(core.FinishRefusal, core.Usage{}, true)
	if !strings.Contains(string(w.Body()), `{"type":"refusal","refusal":"No."}`) {
		t.Errorf("refusal: %s", w.Body())
	}

	for _, tc := range []struct {
		e    *core.Error
		want string
	}{
		{&core.Error{Kind: core.ErrServerError}, `{"type":"error","code":"server_error","message":"The server had an error while processing your request. Sorry about that!","param":null,"sequence_number":2}`},
		{&core.Error{Kind: core.ErrRateLimit}, `"code":"rate_limit_exceeded"`},
		{&core.Error{Kind: core.ErrBadGateway}, `{"type":"error","code":"server_error","message":"","param":null,"sequence_number":2}`},
	} {
		w = core.NewMemWriter()
		s = NewRespStream(&h, w, streamReq(`{}`), nil)
		_ = s.Start(respFixed())
		if err := s.Error(tc.e); err != nil || !strings.Contains(string(w.Body()), "event: error\ndata: ") || !strings.Contains(string(w.Body()), tc.want) {
			t.Errorf("%s: %v %s", tc.e.Kind, err, w.Body())
		}
	}
}

func TestWriteResponsesShapes(t *testing.T) {
	h := DefaultHooks()
	resp := respFixed()
	resp.Reasoning, resp.Content, resp.FinishReason = "why", "", core.FinishToolCalls
	resp.ToolCalls = []core.ToolCall{{ID: "call_1", Name: "f", Arguments: "{}"}}
	w := core.NewMemWriter()
	req := streamReq(`{"reasoning":{"effort":"low"},"text":{"verbosity":"low","format":null},"temperature":0.5,"top_p":1,"truncation":"auto","background":true}`)
	if err := WriteResponses(&h, w, req, resp, nil); err != nil {
		t.Fatal(err)
	}
	body := string(w.Body())
	for _, want := range []string{
		`"type": "reasoning",` + "\n" + `      "summary": []`, `"type": "function_call"`, `"effort": "low"`, `"verbosity": "low"`,
		`"temperature": 0.5`, `"top_p": 1.0`, `"truncation": "auto"`, `"background": true`, `"format": {` + "\n" + `      "type": "text"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `"type": "message"`) {
		t.Error("a tool-call-only response has no message item")
	}
	resp.Extra = nil
	if err := WriteResponses(&h, failWriter{}, req, resp, nil); err == nil {
		t.Error("write error not reported")
	}
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Status(int)                     {}
func (failWriter) Header(string, string)          {}
func (failWriter) Write([]byte) (int, error)      { return 0, errFail }
func (failWriter) Flush() error                   { return nil }
func (failWriter) Fault(core.TransportFault) bool { return false }

var errFail = &core.Error{Kind: core.ErrServerError, Message: "write"}

func TestRespStreamWriteErrors(t *testing.T) {
	h := DefaultHooks()
	s := NewRespStream(&h, failWriter{}, streamReq(`{}`), nil)
	if s.Start(respFixed()) == nil {
		t.Error("start")
	}
	for name, f := range map[string]func() error{
		"text":      func() error { return s.TextDelta("x") },
		"reasoning": func() error { return s.ReasoningDelta("x") },
		"tool":      func() error { return s.ToolCallStart(0, "c", "f") },
		"finish":    func() error { return s.Finish(core.FinishStop, core.Usage{}, true) },
	} {
		if f() == nil {
			t.Errorf("%s: no error", name)
		}
	}
	h.ChunkFormat = Format{}
	bad := NewRespStream(&h, core.NewMemWriter(), streamReq(`{}`), nil)
	bad.v = newRespView(bad.req, respFixed())
	if bad.event(O("type", "x", "bad", make(chan int))) == nil {
		t.Error("unencodable event")
	}
}

// countWriter fails every write after ok successful ones.
type countWriter struct {
	core.ResponseWriter
	ok int
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.ok <= 0 {
		return 0, errFail
	}
	c.ok--
	return c.ResponseWriter.Write(p)
}

// TestRespStreamFailsAtEveryFrame stops the writer at each frame of three
// scripted streams: the failure is reported and nothing panics.
func TestRespStreamFailsAtEveryFrame(t *testing.T) {
	h := DefaultHooks()
	scripts := []func(s *RespStream) []error{
		func(s *RespStream) []error {
			return []error{s.ReasoningDelta("a"), s.ReasoningDelta("b"), s.TextDelta("c"), s.ToolCallStart(0, "c1", "f"), s.ToolArgsDelta(0, "{}"), s.Finish(core.FinishToolCalls, core.Usage{}, true)}
		},
		func(s *RespStream) []error {
			return []error{s.TextDelta("c"), s.ReasoningDelta("a"), s.ToolCallStart(0, "c1", "f"), s.ToolCallEnd(0), s.ToolCallStart(1, "c2", "g"), s.Finish(core.FinishStop, core.Usage{}, true)}
		},
		func(s *RespStream) []error { return []error{s.Finish(core.FinishLength, core.Usage{}, true)} },
	}
	for i, script := range scripts {
		failed := false
		for k := range 40 {
			s := NewRespStream(&h, &countWriter{ResponseWriter: core.NewMemWriter(), ok: k}, streamReq(`{"reasoning":{"summary":"auto"}}`), nil)
			for _, err := range append([]error{s.Start(respFixed())}, script(s)...) {
				failed = failed || err != nil
			}
		}
		if !failed {
			t.Errorf("script %d never failed", i)
		}
	}
}

func TestRespContent(t *testing.T) {
	text, images, audio := respContent(json.RawMessage(`[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"},{"type":"input_file","file_id":"f"},{"type":"input_audio"}]`))
	if text != "a\nb" || images != 1 || audio != 1 {
		t.Errorf("%q %d %d", text, images, audio)
	}
	if text, _, _ = respContent(json.RawMessage(`"plain"`)); text != "plain" {
		t.Errorf("string: %q", text)
	}
	if text, _, _ = respContent(json.RawMessage(`[1]`)); text != "[1]" {
		t.Errorf("bad parts: %q", text)
	}
}
