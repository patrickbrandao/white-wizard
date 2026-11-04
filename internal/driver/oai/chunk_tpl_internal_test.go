package oai

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// memRW is a minimal core.ResponseWriter that keeps the body.
type memRW struct{ body bytes.Buffer }

func (m *memRW) Status(int)                     {}
func (m *memRW) Header(string, string)          {}
func (m *memRW) Write(p []byte) (int, error)    { return m.body.Write(p) }
func (m *memRW) Flush() error                   { return nil }
func (m *memRW) Fault(core.TransportFault) bool { return false }

// tplTexts covers escaping: quotes, backslash, control characters, HTML
// characters, U+2028, invalid UTF-8, multibyte runes and the empty text.
var tplTexts = []string{
	"Hello", " world", ` "quoted"`, `back\slash`, "line\nbreak\ttab\r", "\x01\x1f", " <b>&amp;</b>",
	"\u2028\u2029", "bad\xffutf8", " ação 日本語 🎉", "", " end.",
}

// streamBytes writes a whole stream; off forces the general render.
func streamBytes(t *testing.T, h Hooks, req *core.Request, off bool) []byte {
	t.Helper()
	w := &memRW{}
	s := NewStream(&h, w, req, &core.RateLimitState{})
	s.textOff = off
	resp := &core.Response{ID: "chatcmpl-tpl", Model: "gpt-x", Created: time.Unix(1793457687, 0)}
	if err := s.Start(resp); err != nil {
		t.Fatal(err)
	}
	for _, x := range tplTexts {
		if err := s.TextDelta(x); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(core.FinishStop, core.Usage{PromptTokens: 3, CompletionTokens: 12}, true); err != nil {
		t.Fatal(err)
	}
	if !off && s.text == nil {
		t.Fatalf("hooks %q: the content template was not built", h.Name)
	}
	return w.body.Bytes()
}

// TestContentTemplateMatchesRender: the pre-rendered content chunk writes
// the same bytes as rendering every chunk, obfuscation draws included, for
// the dialect variants the drivers use.
func TestContentTemplateMatchesRender(t *testing.T) {
	gateway := DefaultHooks()
	gateway.Name, gateway.Obfuscation, gateway.ChunkUsageNull = "gateway", false, false
	gateway.NativeFinishReason, gateway.NativeFinish = true, func(*core.Request, core.FinishReason) any { return "x" }
	gateway.TopExtras = func(*core.Request, *core.Response) map[string]any { return map[string]any{"provider": "P"} }
	gateway.Shape = func(p Part, o Obj) Obj {
		if p == PartChunk {
			return o.Reorder("id", "object", "created", "model", "provider")
		}
		return o
	}
	html := DefaultHooks()
	html.Name, html.ChunkFormat = "html", Format{}
	indented := DefaultHooks()
	indented.Name, indented.ChunkFormat = "indented", Format{Indent: "  ", RawHTML: true}

	chat := &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x", IncludeUsage: true, Stream: true}
	legacy := &core.Request{Endpoint: core.EndpointCompletions, Model: "gpt-x", Stream: true}
	for _, h := range []Hooks{DefaultHooks(), gateway, html, indented} {
		for _, req := range []*core.Request{chat, legacy} {
			got, want := streamBytes(t, h, req, false), streamBytes(t, h, req, true)
			if !bytes.Equal(got, want) {
				t.Fatalf("hooks %q endpoint %s:\n got %s\nwant %s", h.Name, req.Endpoint, got, want)
			}
		}
	}
}

// TestContentTemplateFallsBack: a shape that looks at the text makes the
// template unsafe; the stream then renders every chunk.
func TestContentTemplateFallsBack(t *testing.T) {
	h := DefaultHooks()
	h.Shape = func(p Part, o Obj) Obj {
		if v, _ := o.Get("content"); p == PartDelta && v != nil && len(v.(string)) > 14 {
			o = append(o, Field{"long", true})
		}
		return o
	}
	w := &memRW{}
	s := NewStream(&h, w, &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x"}, nil)
	if err := s.Start(&core.Response{ID: "chatcmpl-x", Model: "gpt-x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("a text longer than fourteen"); err != nil {
		t.Fatal(err)
	}
	if s.text != nil || !s.textOff {
		t.Fatal("a value-dependent shape must disable the template")
	}
	if !strings.Contains(w.body.String(), `"long":true`) {
		t.Fatalf("general render expected: %s", w.body.String())
	}
}

func TestNewFrameTemplate(t *testing.T) {
	boom := errors.New("boom")
	if NewFrameTemplate(func(string, string) ([]byte, error) { return nil, boom }, false, false) != nil {
		t.Fatal("render error: no template")
	}
	calls := 0
	second := func(text, _ string) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		return []byte(`{"t":"` + text + `"}`), nil
	}
	if NewFrameTemplate(second, false, false) != nil {
		t.Fatal("second render error: no template")
	}
	twice := func(text, _ string) ([]byte, error) { return []byte(`["` + text + `","` + text + `"]`), nil }
	if NewFrameTemplate(twice, false, false) != nil {
		t.Fatal("text twice: no template")
	}
	noObf := func(text, _ string) ([]byte, error) { return []byte(`["` + text + `"]`), nil }
	if NewFrameTemplate(noObf, true, false) != nil {
		t.Fatal("obfuscation hole missing: no template")
	}
	obfFirst := func(text, obf string) ([]byte, error) { return []byte(`{"o":"` + obf + `","t":"` + text + `"}`), nil }
	tp := NewFrameTemplate(obfFirst, true, true)
	if tp == nil {
		t.Fatal("obfuscation before text: template expected")
	}
	if got := string(tp.Append(nil, "<x>", fixedObf("abc"))); got != `{"o":"abc","t":"\u003cx\u003e"}` {
		t.Fatalf("append: %s", got)
	}
}

// TestObfuscatorAppendNext: appendNext draws exactly like next.
func TestObfuscatorAppendNext(t *testing.T) {
	a, b := newObfuscator("chatcmpl-1"), newObfuscator("chatcmpl-1")
	for range 50 {
		if x, y := a.next(), string(b.appendNext(nil)); x != y {
			t.Fatalf("%q != %q", x, y)
		}
	}
}

// TestContentChunkAllocs: once the template and buffers exist, a content
// chunk allocates nothing (task 158).
func TestContentChunkAllocs(t *testing.T) {
	h := DefaultHooks()
	s := NewStream(&h, &discardRW{}, &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x", IncludeUsage: true}, nil)
	if err := s.Start(&core.Response{ID: "chatcmpl-x", Model: "gpt-x"}); err != nil {
		t.Fatal(err)
	}
	_ = s.TextDelta(" warm <up> \"text\"")
	if n := testing.AllocsPerRun(200, func() { _ = s.TextDelta(" token") }); n != 0 {
		t.Fatalf("%v allocs per content chunk", n)
	}
}

type discardRW struct{ memRW }

func (*discardRW) Write(p []byte) (int, error) { return len(p), nil }
