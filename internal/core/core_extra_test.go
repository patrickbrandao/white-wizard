package core_test

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

func TestErrorStringAndNew(t *testing.T) {
	var nilErr *core.Error
	if nilErr.Error() != "<nil>" {
		t.Fatalf("nil: %q", nilErr.Error())
	}
	e := core.NewError(core.ErrRateLimit, "slow down")
	if e.Kind != core.ErrRateLimit || e.Message != "slow down" {
		t.Fatalf("NewError: %+v", e)
	}
	if e.Error() != "rate_limit: slow down" {
		t.Fatalf("Error: %q", e.Error())
	}
}

func TestRequestHelpers(t *testing.T) {
	r := &core.Request{}
	if r.LastUserMessage() != "" || r.Header("x") != "" {
		t.Fatal("empty request must yield empty strings")
	}
	r.Messages = []core.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "last"},
		{Role: "assistant", Content: "b"},
	}
	r.Headers = map[string]string{"x-a": "1"}
	if got := r.LastUserMessage(); got != "last" {
		t.Fatalf("LastUserMessage %q", got)
	}
	if r.Header("x-a") != "1" || r.Header("missing") != "" {
		t.Fatal("Header lookup")
	}
	r.Messages = []core.Message{{Role: "assistant", Content: "a"}}
	if r.LastUserMessage() != "" {
		t.Fatal("no user turn must yield empty")
	}
}

// TestSystemPrompt: System when set (Anthropic, Gemini), else the first
// message when it is a system one (Chat Completions, Responses; task 245).
func TestSystemPrompt(t *testing.T) {
	for _, c := range []struct {
		r    core.Request
		want string
	}{
		{core.Request{}, ""},
		{core.Request{System: "top", Messages: []core.Message{{Role: "system", Content: "msg"}}}, "top"},
		{core.Request{Messages: []core.Message{{Role: "system", Content: "msg"}, {Role: "user", Content: "u"}}}, "msg"},
		{core.Request{Messages: []core.Message{{Role: "user", Content: "u"}, {Role: "system", Content: "late"}}}, ""},
	} {
		if got := c.r.SystemPrompt(); got != c.want {
			t.Errorf("SystemPrompt(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestUsageTotalTokens(t *testing.T) {
	if got := (core.Usage{PromptTokens: 3, CompletionTokens: 4}).TotalTokens(); got != 7 {
		t.Fatalf("got %d", got)
	}
}

func TestBufferedWriter(t *testing.T) {
	var buf bytes.Buffer
	b := &core.BufferedWriter{W: bufio.NewWriter(&buf)}
	b.Status(500)
	b.Header("a", "b")
	if n, err := b.Write([]byte("hi")); n != 2 || err != nil {
		t.Fatalf("write %d %v", n, err)
	}
	if buf.Len() != 0 {
		t.Fatal("must be buffered until Flush")
	}
	if err := b.Flush(); err != nil || buf.String() != "hi" {
		t.Fatalf("flush %v %q", err, buf.String())
	}
	if b.Fault(core.TransportReset) {
		t.Fatal("no OnFault: must return false")
	}
	var seen core.TransportFault
	b.OnFault = func(f core.TransportFault) bool { seen = f; return true }
	if !b.Fault(core.TransportHang) || seen != core.TransportHang {
		t.Fatal("OnFault must be invoked")
	}
}

func TestMemWriter(t *testing.T) {
	m := core.NewMemWriter()
	if m.StatusCode() != 200 {
		t.Fatalf("default status %d", m.StatusCode())
	}
	m.Status(429)
	m.Header("k", "v")
	hs := m.Headers()
	hs["k"] = "mutated"
	if m.StatusCode() != 429 || m.Headers()["k"] != "v" {
		t.Fatal("status/headers (Headers must return a copy)")
	}

	var flushed [][]byte
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	m.OnFlush = func(p []byte) { flushed = append(flushed, p) }
	if err := m.Flush(); err != nil || len(flushed) != 0 {
		t.Fatal("empty flush must not call OnFlush")
	}
	_, _ = m.Write([]byte("ab"))
	_, _ = m.Write([]byte("cd"))
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Write([]byte("e"))
	_ = m.Flush()
	if len(flushed) != 2 || string(flushed[0]) != "abcd" || string(flushed[1]) != "e" {
		t.Fatalf("flushed %q", flushed)
	}
	body := m.Body()
	body[0] = 'X'
	if string(m.Body()) != "abcde" {
		t.Fatalf("body %q (must be a copy)", m.Body())
	}

	if !m.Fault(core.TransportReset) || !m.Fault(core.TransportEmpty) {
		t.Fatal("MemWriter honours faults")
	}
	fs := m.Faults()
	if len(fs) != 2 || fs[0] != core.TransportReset || fs[1] != core.TransportEmpty {
		t.Fatalf("faults %v", fs)
	}

	called := false
	m.Stream(func(w core.ResponseWriter) {
		called = true
		if w != core.ResponseWriter(m) {
			t.Fatal("Stream must pass itself")
		}
	})
	if !called {
		t.Fatal("Stream must run fn")
	}
}
