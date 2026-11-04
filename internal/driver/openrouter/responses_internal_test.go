package openrouter

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// failAfter fails every write from the n-th on.
type failAfter struct {
	*core.MemWriter
	n, writes int
}

var errWrite = errors.New("write")

func (f *failAfter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.n {
		return 0, errWrite
	}
	return f.MemWriter.Write(p)
}

func respReq(body string) *core.Request {
	return &core.Request{Raw: []byte(body), Headers: map[string]string{"x-wwz-path": responsesPath}, Stream: true}
}

// drive runs a stream with every kind of item; it returns the first error.
func drive(s core.StreamWriter) error {
	for _, f := range []func() error{
		func() error { return s.Start(&core.Response{ID: "gen-1", Model: "m", Created: time.Unix(1, 0)}) },
		func() error { return s.ReasoningDelta("think") },
		func() error { return s.ReasoningDelta(" more") },
		func() error { return s.TextDelta("a") },
		func() error { return s.ReasoningDelta("again") },
		func() error { return s.TextDelta("b") },
		func() error { return s.ToolCallStart(0, "call_1", "f") },
		func() error { return s.ToolArgsDelta(0, "{}") },
		func() error { return s.ToolArgsDelta(9, "ignored") },
		func() error { return s.ToolCallEnd(0) },
		func() error { return s.ToolCallEnd(9) },
		func() error { return s.ToolCallStart(2, "call_2", "g") },
		func() error { return s.ToolCallStart(1, "call_3", "h") },
		func() error {
			return s.Finish(core.FinishToolCalls, core.Usage{PromptTokens: 1, CompletionTokens: 2}, true)
		},
	} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// TestRespStreamWriteErrors: every write error reaches the caller.
func TestRespStreamWriteErrors(t *testing.T) {
	d := &Driver{}
	ok := false
	for n := 0; n < 60; n++ {
		w := &failAfter{MemWriter: core.NewMemWriter(), n: n}
		if drive(newRespStream(d, w, respReq(`{}`))) == nil {
			ok = true
			if w.writes > n {
				t.Fatalf("n=%d: a failed write was not reported", n)
			}
		}
	}
	if !ok {
		t.Fatal("the stream never completed")
	}
	for n := 3; n < 8; n++ { // a message still open at Finish
		s := newRespStream(d, &failAfter{MemWriter: core.NewMemWriter(), n: n}, respReq(`{}`))
		_ = s.Start(&core.Response{ID: "gen-1"})
		_ = s.TextDelta("a")
		if s.Finish(core.FinishLength, core.Usage{}, true) == nil {
			t.Errorf("n=%d: no error", n)
		}
	}
	w := core.NewMemWriter()
	if err := drive(newRespStream(d, w, respReq(`{}`))); err != nil {
		t.Fatal(err)
	}
	body := string(w.Body())
	for _, want := range []string{`"type":"reasoning","summary":[{"type":"summary_text","text":"think more"}]`, `"call_id":"call_2"`, "data: [DONE]\n\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream without %s:\n%s", want, body)
		}
	}
	if strings.Index(body, `"call_3"`) > strings.LastIndex(body, `"call_2"`) {
		t.Error("pending calls not closed in index order")
	}
}

// TestRespStreamExtras: the empty answer still has its message; error,
// ping, raw and close.
func TestRespStreamExtras(t *testing.T) {
	d := &Driver{}
	w := core.NewMemWriter()
	s := newRespStream(d, w, respReq(`{}`))
	if err := s.Start(&core.Response{ID: "gen-1", Model: "m", Headers: map[string]string{"x-extra": "1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(core.FinishStop, core.Usage{}, true); err != nil || !strings.Contains(string(w.Body()), `"text":""`) {
		t.Errorf("empty answer: %v %s", err, w.Body())
	}
	if w.Headers()["x-extra"] != "1" {
		t.Error("response headers on the stream")
	}
	fw := &failAfter{MemWriter: core.NewMemWriter(), n: 2}
	empty := newRespStream(d, fw, respReq(`{}`))
	if empty.Start(&core.Response{ID: "gen-1"}) != nil || empty.Finish(core.FinishStop, core.Usage{}, true) == nil {
		t.Error("empty answer: write error not reported")
	}
	if err := s.Ping(); err != nil || !strings.Contains(string(w.Body()), ": OPENROUTER PROCESSING") {
		t.Errorf("ping: %v", err)
	}
	if err := s.Raw([]byte("x")); err != nil || string(s.LastFrame()) != "x" || s.Close() != nil {
		t.Error("raw")
	}
	w = core.NewMemWriter()
	s = newRespStream(d, w, respReq(`{}`))
	_ = s.Start(&core.Response{ID: "gen-1", Model: "m"})
	if err := s.Error(&core.Error{Kind: core.ErrServerError}); err != nil || !strings.Contains(string(w.Body()), `"type":"error","code":500,"message":"Internal Server Error","param":null`) {
		t.Errorf("error: %v %s", err, w.Body())
	}
	for n := 0; n < 2; n++ {
		s = newRespStream(d, &failAfter{MemWriter: core.NewMemWriter(), n: n}, respReq(`{}`))
		if s.Error(&core.Error{Kind: core.ErrServerError, Message: "boom"}) == nil {
			t.Errorf("n=%d: error write not reported", n)
		}
	}
}

// TestWriteResponsesErrors: a failed body write is reported; reasoning
// and echoed fields render.
func TestWriteResponsesErrors(t *testing.T) {
	resp := &core.Response{ID: "gen-1", Model: "m", Reasoning: "r", Content: "c", Headers: map[string]string{"x-extra": "1"}}
	if writeResponses(&failAfter{MemWriter: core.NewMemWriter()}, respReq(`{}`), resp) == nil {
		t.Error("write error not reported")
	}
	w := core.NewMemWriter()
	req := respReq(`{"tools":[{"type":"web_search"},{"type":"function","name":"f","parameters":{},"strict":true}],"parallel_tool_calls":false}`)
	if err := writeResponses(w, req, resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"reasoning"`, `{"type":"web_search"}`, `{"type":"function","name":"f","parameters":{},"strict":true}`, `"parallel_tool_calls":false`} {
		if !strings.Contains(string(w.Body()), want) {
			t.Errorf("body without %s: %s", want, w.Body())
		}
	}
	if w.Headers()["x-extra"] != "1" {
		t.Error("response headers")
	}
}

// TestLenientMembers: the gateway's reading of a body.
func TestLenientMembers(t *testing.T) {
	for body, want := range map[string]string{
		`{"a":1,"b":2,}`:      `a=1 b=2`,
		`{"a":1,,"a":3}`:      `a=3`,
		`{"a":1,"b":`:         `a=1 b=null`,
		`{"a":1,"b"`:          `a=1 b=null`,
		`{"a":1,"b":[1,}`:     `a=1 b=null`,
		`{"a":1,"b`:           `a=1`,
		`{"a":1, 5}`:          `a=1`,
		`{"a\x":1}`:           ``,
		`{"a\"b":1}`:          `a"b=1`,
		`not json`:            ``,
		`  {"a" : "x" }  `:    `a="x"`,
		`{"a":{"b":[1,2]}} x`: `a={"b":[1,2]}`,
	} {
		ms, kind := lenientMembers([]byte(body))
		var got []string
		for _, m := range ms {
			got = append(got, m.key+"="+string(m.val))
		}
		if kind != "" || strings.Join(got, " ") != want {
			t.Errorf("%q: %q %q, want %q", body, kind, strings.Join(got, " "), want)
		}
	}
	for body, kind := range map[string]string{`[]`: "array", `5`: "number", `"s"`: "string", `true`: "boolean", `null`: "null"} {
		if _, k := lenientMembers([]byte(body)); k != kind {
			t.Errorf("%s: %q, want %q", body, k, kind)
		}
	}
	if zodType(nil) != "undefined" || zodType([]byte("{}")) != "object" {
		t.Error("zodType")
	}
}

// TestParseResponsesEdges: models[0] stands for model; a body the parser
// refuses after the checks keeps its error; a tool without strict or with
// additionalProperties: false passes.
func TestParseResponsesEdges(t *testing.T) {
	d := &Driver{}
	req, err := d.parseResponses([]byte(`{"models":["openai/gpt-4o-mini"],"input":"hi","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object","additionalProperties":false}},{"type":"web_search"}]}`))
	if err != nil || req.Model != "openai/gpt-4o-mini" {
		t.Fatalf("%+v %+v", req, err)
	}
	if _, err := d.parseResponses([]byte(`{"model":"m","input":[5]}`)); err == nil {
		t.Error("input items that are not objects")
	}
	if _, err := d.parseResponses([]byte(`{"model":"m","input":"hi","tools":5}`)); err == nil || err.Extra[extraRawReply] != nil {
		t.Errorf("tools not a list: %+v", err)
	}
	if _, err := d.parseResponses([]byte(`{"model":"m","input":"hi","previous_response_id":"x","store":true}`)); err == nil || err.Message != storeMessage {
		t.Errorf("first custom message: %+v", err)
	}
}
