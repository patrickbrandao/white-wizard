package anth

import (
	"bytes"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

var tplTexts = []string{"Hello", ` "q"`, `a\b`, "l\n\t", "\x02", " <b>&</b>", "\u2028", "x\xff", " ação 🎉", ""}

// eventStream writes a stream with thinking, text and two tool calls.
func eventStream(t *testing.T, h Hooks) []byte {
	t.Helper()
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{Endpoint: core.EndpointMessages}, nil)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Start(&core.Response{ID: "msg_tpl", Model: "claude-x", Reasoning: "r"}))
	for _, x := range tplTexts {
		must(s.ReasoningDelta(x))
	}
	for _, x := range tplTexts {
		must(s.TextDelta(x))
	}
	for i := range 2 {
		must(s.ToolCallStart(i, "toolu_"+string(rune('a'+i)), "fn"))
		for _, x := range tplTexts {
			must(s.ToolArgsDelta(i, x))
		}
		must(s.ToolCallEnd(i))
	}
	must(s.Finish(core.FinishToolCalls, core.Usage{PromptTokens: 5, CompletionTokens: 9}, true))
	return w.Body()
}

// TestDeltaTemplateMatchesRender: the pre-rendered deltas write the same
// bytes (padding draws included) as rendering every delta, across block
// index changes, for the API and the gateway dialects.
func TestDeltaTemplateMatchesRender(t *testing.T) {
	shaped := DefaultHooks()
	shaped.Shape = func(p Part, o oai.Obj) oai.Obj {
		if p == PartEvent {
			return append(o, oai.Field{Key: "provider", Val: "P"})
		}
		return o
	}
	for _, h := range []Hooks{APIHooks(), DefaultHooks(), shaped} {
		got := eventStream(t, h)
		newTemplate = func(func(string, string) ([]byte, error), bool, bool) *oai.FrameTemplate { return nil }
		want := eventStream(t, h)
		newTemplate = oai.NewFrameTemplate
		if !bytes.Equal(got, want) {
			t.Fatalf("API %v:\n got %s\nwant %s", h.API, got, want)
		}
	}
}

// TestDeltaTemplateFallsBack: a shape that reads the text disables the
// template and every delta goes through it.
func TestDeltaTemplateFallsBack(t *testing.T) {
	h := DefaultHooks()
	h.Shape = func(p Part, o oai.Obj) oai.Obj {
		if d, ok := o.Get("delta"); ok && p == PartEvent {
			if v, _ := d.(oai.Obj).Get("text"); v != nil && len(v.(string)) > 10 {
				o = append(o, oai.Field{Key: "long", Val: true})
			}
		}
		return o
	}
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{}, nil)
	if err := s.Start(&core.Response{ID: "msg_1", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("a rather long text"); err != nil {
		t.Fatal(err)
	}
	if s.deltas[deltaText].t != nil || !strings.Contains(string(w.Body()), `"long":true`) {
		t.Fatalf("general render expected: %s", w.Body())
	}
}

// TestTextDeltaAllocs: a text delta allocates nothing once its template
// and the frame buffers exist (task 158).
func TestTextDeltaAllocs(t *testing.T) {
	h := APIHooks()
	s := NewStream(&h, discard{}, &core.Request{}, nil)
	if err := s.Start(&core.Response{ID: "msg_1", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	_ = s.TextDelta(" warm <up> \"text\"")
	if n := testing.AllocsPerRun(200, func() { _ = s.TextDelta(" token") }); n != 0 {
		t.Fatalf("%v allocs per text delta", n)
	}
}

type discard struct{}

func (discard) Status(int)                     {}
func (discard) Header(string, string)          {}
func (discard) Write(p []byte) (int, error)    { return len(p), nil }
func (discard) Flush() error                   { return nil }
func (discard) Fault(core.TransportFault) bool { return false }
