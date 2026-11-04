package opencode

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func covNew(t *testing.T) *Driver {
	t.Helper()
	drv, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	return drv.(*Driver)
}

func TestRegisterOptions(t *testing.T) {
	if _, err := driver.New("opencode", map[string]string{"require_session": "false"}); err == nil {
		t.Error("accepted require_session=false")
	}
	if _, err := driver.New("opencode", map[string]string{"reasoning_field": "thoughts"}); err == nil {
		t.Error("accepted reasoning_field=thoughts")
	}
}

func TestParseRequestUnknownKindAndReasonByDefault(t *testing.T) {
	d := covNew(t)
	_, e := d.ParseRequest(driver.KindModels, nil, driver.MapHeaders{})
	if e == nil || e.Kind != core.ErrNotFound {
		t.Errorf("err = %v", e)
	}
	// Already requested: untouched. A body that is not an object: untouched.
	req := &core.Request{Reasoning: core.ReasoningRequest{Requested: true, Effort: "low"}}
	reasonByDefault(req, []byte(`{}`), "reasoning")
	if req.Reasoning.Effort != "low" {
		t.Errorf("reasoning = %+v", req.Reasoning)
	}
	req = &core.Request{}
	reasonByDefault(req, []byte(`[1]`), "reasoning")
	if req.Reasoning.Requested {
		t.Error("reasoning requested from a non-object body")
	}
	reasonByDefault(req, []byte(`{"reasoning":1}`), "reasoning")
	if req.Reasoning.Requested {
		t.Error("reasoning requested although the body names the field")
	}
	reasonByDefault(req, []byte(`{}`), "reasoning")
	if !req.Reasoning.Requested {
		t.Error("default reasoning not requested")
	}
}

func TestPresentedKeyAndErrorParts(t *testing.T) {
	if presentedKey(nil) != "" {
		t.Error("nil request")
	}
	d := covNew(t)
	keyed := &core.Request{Headers: map[string]string{"authorization": "Bearer abc"}}
	// A custom message is prefixed as the upstream's.
	_, _, msg, _, _ := d.errorParts(keyed, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "token expired"})
	if !strings.HasSuffix(msg, "token expired") {
		t.Errorf("msg = %q", msg)
	}
	// An explicit status and code replace the kind's.
	status, typ, _, _, _ := d.errorParts(keyed, &core.Error{Kind: core.ErrServerError, Status: 503, Code: "custom_code"})
	if status != 503 || typ != "custom_code" {
		t.Errorf("status %d typ %s", status, typ)
	}
}

type failingWriter struct {
	*core.MemWriter
	fail bool
}

var errBoom = errors.New("boom")

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.fail {
		return 0, errBoom
	}
	return f.MemWriter.Write(p)
}

func TestGoogleWriteFailuresAndHeaders(t *testing.T) {
	req := &core.Request{Headers: map[string]string{"x-wwz-path": "/google/v1beta/models/gemini-4-argon:generateContent"}}
	resp := &core.Response{ID: "r1", Model: "gemini-4-argon", Content: "hi", Headers: map[string]string{"x-extra": "1"}}
	w := core.NewMemWriter()
	if err := writeGoogle(w, req, resp); err != nil {
		t.Fatal(err)
	}
	if w.Headers()["x-extra"] != "1" || w.Headers()["x-zen-model"] != "gemini-4-argon" {
		t.Errorf("headers = %v", w.Headers())
	}
	if err := writeGoogle(&failingWriter{MemWriter: core.NewMemWriter(), fail: true}, req, resp); err == nil {
		t.Error("writeGoogle swallowed the write error")
	}

	for _, model := range []string{"gemini-4-argon", "deepseek-v4.1-flash"} {
		resp := &core.Response{ID: "r1", Model: model}
		fw := &failingWriter{MemWriter: core.NewMemWriter()}
		s := newGoogleStream(fw, req)
		if err := s.Start(resp); err != nil {
			t.Fatal(err)
		}
		if err := s.TextDelta("a"); err != nil {
			t.Fatal(err)
		}
		if err := s.Raw([]byte(": hi\n\n")); err != nil {
			t.Fatal(err)
		}
		fw.fail = true
		if s.TextDelta("b") == nil || s.Raw([]byte("x")) == nil || s.Error(&core.Error{Kind: core.ErrServerError}) == nil {
			t.Errorf("%s: a stream write error was swallowed", model)
		}
	}
}

func TestModelJSONNameAndAuthorDefaults(t *testing.T) {
	m := modelJSON(driver.ModelInfo{Cfg: &config.ModelConfig{ID: "kimi-k3", OwnedBy: "opencode", Version: "kimi-k3-0905"}}, time.Unix(1, 0))
	for k, want := range map[string]any{"name": "kimi-k3", "canonical_id": "kimi/kimi-k3", "family": "kimi-k3-0905"} {
		if v, _ := m.Get(k); v != want {
			t.Errorf("%s = %v, want %v", k, v, want)
		}
	}
}

func TestShapeChatKeepsTextChoices(t *testing.T) {
	for _, p := range []oai.Part{oai.PartChoice, oai.PartChunkChoice} {
		o := oai.O("index", 0, "text", "x", "logprobs", nil)
		got := shapeChat(p, o)
		if _, ok := got.Get("logprobs"); !ok {
			t.Errorf("%s: text choice was reshaped: %v", p, got)
		}
	}
}

func TestWriteUsageOnAMonday(t *testing.T) {
	w := core.NewMemWriter()
	writeUsage(w, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), quota{})
	if !strings.Contains(string(w.Body()), "2026-11-09T00:00:00") {
		t.Errorf("a Monday resets the next Monday: %s", w.Body())
	}
}
