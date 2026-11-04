package opencode

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
	return &core.Request{Raw: []byte(body), Headers: map[string]string{"x-wwz-path": "/inference/go/openai/v1/responses"}, Stream: true}
}

func drive(s core.StreamWriter) error {
	for _, f := range []func() error{
		func() error { return s.Start(&core.Response{ID: "resp_1", Model: "m", Created: time.Unix(1, 0)}) },
		func() error { return s.ReasoningDelta("think") },
		func() error { return s.TextDelta("a") },
		func() error { return s.TextDelta("b") },
		func() error { return s.ToolCallStart(0, "call_1", "f") },
		func() error { return s.ToolArgsDelta(0, "{}") },
		func() error { return s.ToolArgsDelta(9, "ignored") },
		func() error { return s.ToolCallEnd(0) },
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
	for n := 0; n < 40; n++ {
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
	for n := 0; n < 4; n++ { // the empty answer's message
		s := newRespStream(d, &failAfter{MemWriter: core.NewMemWriter(), n: n}, respReq(`{}`))
		_ = s.Start(&core.Response{ID: "resp_1"})
		if s.Finish(core.FinishStop, core.Usage{}, true) == nil {
			t.Errorf("n=%d: no error", n)
		}
	}
}

// TestRespStreamExtras: the empty answer has its message; error, ping,
// raw and close; a cut message is incomplete.
func TestRespStreamExtras(t *testing.T) {
	d := &Driver{}
	w := core.NewMemWriter()
	s := newRespStream(d, w, respReq(`{"text":{"format":{"type":"text"}}}`))
	if err := s.Start(&core.Response{ID: "resp_1", Model: "m", Headers: map[string]string{"x-extra": "1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(core.FinishLength, core.Usage{}, true); err != nil {
		t.Fatal(err)
	}
	body := string(w.Body())
	for _, want := range []string{`"status":"incomplete","content":[{"type":"output_text","text":""`, "event: response.incomplete\n", `"max_output_tokens":null,"text":{"format":{"type":"text"}},"usage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("without %s:\n%s", want, body)
		}
	}
	if w.Headers()["x-extra"] != "1" {
		t.Error("response headers")
	}
	if err := s.Ping(); err != nil || s.Raw([]byte("x")) != nil || string(s.LastFrame()) != "x" || s.Close() != nil {
		t.Error("ping, raw")
	}
	w = core.NewMemWriter()
	s = newRespStream(d, w, respReq(`{}`))
	if err := s.Error(&core.Error{Kind: core.ErrServerError}); err != nil || string(w.Body()) != "event: error\ndata: {\"error\":{\"type\":\"server_error\",\"message\":\"Upstream request failed: Internal error\"}}\n\n" {
		t.Errorf("error: %v %q", err, w.Body())
	}
}

// TestWriteResponsesEdges: a failed write is reported; an answer with
// nothing has an empty message; response headers pass.
func TestWriteResponsesEdges(t *testing.T) {
	if writeResponses(&failAfter{MemWriter: core.NewMemWriter()}, respReq(`{}`), &core.Response{ID: "resp_1"}) == nil {
		t.Error("write error not reported")
	}
	w := core.NewMemWriter()
	if err := writeResponses(w, respReq(`{}`), &core.Response{ID: "resp_1", Headers: map[string]string{"x-extra": "1"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w.Body()), `"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":""`) || w.Headers()["x-extra"] != "1" {
		t.Errorf("%s", w.Body())
	}
}

// TestCheckResponsesEdges: the upstream's Go type names, a body that is
// not an object, input items the parser refuses.
func TestCheckResponsesEdges(t *testing.T) {
	for v, want := range map[string]string{`"x"`: "string", `true`: "bool", `[1]`: "array", `{}`: "object"} {
		err := checkResponses([]byte(`{"model":"m","input":"hi","max_output_tokens":` + v + `}`))
		if err == nil || !strings.Contains(err.Message, "cannot unmarshal "+want+" into") {
			t.Errorf("%s: %+v", v, err)
		}
	}
	if checkResponses([]byte(`{"model":"m","input":"hi","max_output_tokens":null}`)) != nil {
		t.Error("max_output_tokens null")
	}
	if len(bodyOrder([]byte(`[1]`))) != 0 || len(bodyOrder([]byte(`{"a":1,`))) != 1 || len(bodyOrder([]byte(`{"a":`))) != 0 {
		t.Error("bodyOrder")
	}
	d := &Driver{}
	if _, err := d.parseResponses([]byte(`{"model":"m","input":[5]}`)); err == nil || err.Extra[extraRawReply] != nil {
		t.Errorf("input items that are not objects: %+v", err)
	}
}
