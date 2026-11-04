package oai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

func TestParseResponsesNamedToolMissingFallsBackToAuto(t *testing.T) {
	req, e := ParseResponses([]byte(`{"model":"m","input":"x","tool_choice":{"type":"function","name":"ghost"},"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`), "openai")
	if e != nil {
		t.Fatal(e.Message)
	}
	if req.ToolChoice.Mode != core.ToolChoiceAuto {
		t.Errorf("tool choice = %+v", req.ToolChoice)
	}
	req, e = ParseResponses([]byte(`{"model":"m","input":"x"}`), "openai")
	if e != nil || req.ToolChoice.Mode != core.ToolChoiceAuto {
		t.Errorf("default tool choice = %+v %v", req.ToolChoice, e)
	}
}

func TestRespViewEchoesTheRequest(t *testing.T) {
	raw := `{"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"model":"m","instructions":"Be brief.","max_output_tokens":9,"max_tool_calls":2,"tool_choice":"required",
	  "top_logprobs":3,"metadata":{"k":"v"},"user":"u",
	  "text":{"verbosity":"high","format":{"type":"json_schema","name":"n","description":"d","schema":{"type":"object"},"strict":true}}}`
	h := DefaultHooks()
	w := core.NewMemWriter()
	resp := respFixed()
	resp.Content = "hello"
	resp.Headers = map[string]string{"x-extra": "1"}
	if err := WriteResponses(&h, w, streamReq(raw), resp, nil); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "Be brief." || body["max_output_tokens"] != float64(9) || body["tool_choice"] != "required" {
		t.Errorf("echo = %s", w.Body())
	}
	if w.Headers()["x-extra"] != "1" {
		t.Errorf("headers = %v", w.Headers())
	}
	if !strings.Contains(string(w.Body()), `"strict": true`) || !strings.Contains(string(w.Body()), `"verbosity": "high"`) {
		t.Errorf("format/verbosity not echoed: %s", w.Body())
	}
	if !strings.Contains(string(w.Body()), `"type": "message"`) {
		t.Errorf("no message item: %s", w.Body())
	}

	// A body that is not JSON echoes nothing and does not break.
	bad := streamReq("{broken")
	v := newRespView(bad, resp)
	if v.echo.Instructions != nil {
		t.Errorf("echo = %+v", v.echo)
	}
	if got := rawOr(nil, "def"); got != "def" {
		t.Errorf("rawOr = %v", got)
	}
}

func TestOrderedValueSyntaxErrors(t *testing.T) {
	for _, in := range []string{`{`, `{"a"`, `{"a":`, `{,}`, `[1,`, `[`, `{"a":[1,}`} {
		if _, err := decodeOrderedErr(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func decodeOrderedErr(in string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(in))
	return orderedValue(dec)
}

func TestStrictSchemaShapes(t *testing.T) {
	if got, ok := strictSchema("scalar"); !ok || got != "scalar" {
		t.Errorf("scalar = %v %v", got, ok)
	}
	// An object schema with no properties gets an empty required list.
	got, ok := strictSchema(O("type", "object"))
	if !ok {
		t.Fatal("did not fit")
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"type":"object","additionalProperties":false,"required":[]}` {
		t.Errorf("schema = %s", b)
	}
	// A partial required list is completed in place.
	got, ok = strictSchema(O("type", "object", "properties", O("a", O("type", "string"), "b", O("type", "string")), "required", []any{"a"}))
	b, _ = json.Marshal(got)
	if !ok || !strings.Contains(string(b), `"required":["a","b"]`) {
		t.Errorf("partial required = %s", b)
	}
	// echoFormat of a json_schema without strict leaves it out.
	f := echoFormat([]byte(`{"type":"json_schema","name":"n","schema":{"type":"object"}}`))
	if _, has := f.(Obj).Get("strict"); has {
		t.Error("strict invented")
	}
	if got := echoFormat([]byte(`"text"`)); got != "text" {
		t.Errorf("non-object format = %v", got)
	}
}

// failAfter makes the n-th write onward fail.
func failingStream(t *testing.T, raw string, k int) *RespStream {
	t.Helper()
	h := DefaultHooks()
	return NewRespStream(&h, &countWriter{ResponseWriter: core.NewMemWriter(), ok: k}, streamReq(raw), nil)
}

// TestRespStreamFailureAtEveryWrite fails the writer after k writes for
// every k, over scripts that open and close each kind of item in each order.
func TestRespStreamFailureAtEveryWrite(t *testing.T) {
	scripts := map[string]func(s *RespStream) []error{
		"reasoning then text": func(s *RespStream) []error {
			return []error{s.ReasoningDelta("a"), s.TextDelta("b"), s.Finish(core.FinishStop, core.Usage{}, true)}
		},
		"text then reasoning": func(s *RespStream) []error {
			return []error{s.TextDelta("b"), s.ReasoningDelta("a"), s.ToolCallStart(0, "c", "f"), s.ToolArgsDelta(0, "{}"), s.ToolCallEnd(0), s.Finish(core.FinishStop, core.Usage{}, true)}
		},
		"only reasoning": func(s *RespStream) []error {
			return []error{s.ReasoningDelta("a"), s.ReasoningDelta("b"), s.Finish(core.FinishLength, core.Usage{}, true)}
		},
	}
	for _, raw := range []string{`{"reasoning":{"summary":"auto"}}`, `{}`} {
		for name, script := range scripts {
			failures := 0
			for k := range 60 {
				s := failingStream(t, raw, k)
				if s.Start(respFixed()) != nil {
					failures++
				}
				for _, err := range script(s) {
					if err != nil {
						failures++
					}
				}
			}
			if failures == 0 {
				t.Errorf("%s %s: never failed", raw, name)
			}
		}
	}
	// Writers that fail from the very first write, with Start skipped.
	for _, raw := range []string{`{"reasoning":{"summary":"auto"}}`, `{}`} {
		s := failingStream(t, raw, 0)
		s.v = newRespView(s.req, respFixed())
		if s.ReasoningDelta("a") == nil {
			t.Errorf("%s: ReasoningDelta did not fail", raw)
		}
		s = failingStream(t, raw, 0)
		s.v = newRespView(s.req, respFixed())
		if s.TextDelta("a") == nil {
			t.Errorf("%s: TextDelta did not fail", raw)
		}
	}
}

// TestRespStreamOpenFailsClosingTheOpenItem: switching kind closes the open
// item first, and a write error there is reported.
func TestRespStreamOpenFailsClosingTheOpenItem(t *testing.T) {
	for _, first := range []string{"reasoning", "message"} {
		for k := range 12 {
			s := failingStream(t, `{"reasoning":{"summary":"auto"}}`, 100)
			s.v = newRespView(s.req, respFixed())
			var err error
			if first == "reasoning" {
				err = s.ReasoningDelta("a")
			} else {
				err = s.TextDelta("a")
			}
			if err != nil {
				t.Fatal(err)
			}
			s.w.W.(*countWriter).ok = k
			if first == "reasoning" {
				_ = s.TextDelta("b")
			} else {
				_ = s.ReasoningDelta("b")
			}
		}
	}
}

func TestRespStreamRepeatedTextAndHeaders(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewRespStream(&h, w, streamReq(`{}`), nil)
	resp := respFixed()
	resp.Headers = map[string]string{"x-extra": "1"}
	for _, err := range []error{s.Start(resp), s.TextDelta("a"), s.TextDelta("b"), s.Finish(core.FinishStop, core.Usage{}, true)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if w.Headers()["x-extra"] != "1" || strings.Count(string(w.Body()), "event: response.content_part.added") != 1 {
		t.Errorf("headers %v\n%s", w.Headers(), w.Body())
	}
}

func TestFinishUsageChunkWriteError(t *testing.T) {
	h := Hooks{FinishUsage: true}
	for k := range 8 {
		w := &countWriter{ResponseWriter: core.NewMemWriter(), ok: k}
		s := NewStream(&h, w, &core.Request{Endpoint: core.EndpointChat}, nil)
		_ = s.Start(&core.Response{ID: "x", Model: "m"})
		_ = s.TextDelta("hi")
		_ = s.Finish(core.FinishStop, core.Usage{PromptTokens: 1}, true)
	}
}

func TestDeprecatedModelMessage(t *testing.T) {
	req := &core.Request{Model: "gpt-old", Endpoint: core.EndpointResponses}
	e := &core.Error{Kind: core.ErrModelNotFound, Extra: map[string]any{ExtraModelRetired: true}}
	_, body, _, _ := OpenAIErrorBody(req, e)
	if !strings.Contains(string(body.([]byte)), "has been deprecated") {
		t.Errorf("body = %v", body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
