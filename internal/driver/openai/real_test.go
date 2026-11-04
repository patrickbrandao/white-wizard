package openai_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

// realReply is one answer of the real API as wwz-drift saved it
// (internal/drift/testdata/real/openai): status line, headers, body.
type realReply struct {
	status  int
	headers map[string]string // lower-cased names
	body    string
}

func loadReal(t *testing.T, name string) realReply {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "drift", "testdata", "real", "openai", name+"-real.txt"))
	if err != nil {
		t.Fatal(err)
	}
	head, body, ok := strings.Cut(string(raw), "\n\n")
	if !ok {
		t.Fatalf("%s: no blank line", name)
	}
	lines := strings.Split(head, "\n")
	st, err := strconv.Atoi(strings.TrimPrefix(lines[0], "HTTP "))
	if err != nil {
		t.Fatalf("%s: status line %q", name, lines[0])
	}
	r := realReply{status: st, headers: map[string]string{}, body: body}
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l, ": ")
		r.headers[strings.ToLower(k)] = v
	}
	return r
}

// dialectHeader reports whether a header is the driver's to send: the
// infrastructure (cf-*, server, date, proxy headers) is not (ADR 0049).
func dialectHeader(k string) bool {
	switch {
	case k == "x-openai-proxy-wasm" || k == "x-openai-internal-caller":
		return false
	case strings.HasPrefix(k, "openai-"), strings.HasPrefix(k, "x-ratelimit-"), strings.HasPrefix(k, "x-openai-"):
		return true
	}
	return k == "x-request-id" || k == "x-error-json" || k == "content-type"
}

// checkHeaders compares the driver's header names with the real ones and
// the values that do not vary per call.
func checkHeaders(t *testing.T, real realReply, got map[string]string, sameValue ...string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		low[strings.ToLower(k)] = v
	}
	for k := range real.headers {
		if dialectHeader(k) {
			if _, ok := low[k]; !ok {
				t.Errorf("header %s missing (real %q)", k, real.headers[k])
			}
		}
	}
	for k := range low {
		if dialectHeader(k) {
			if _, ok := real.headers[k]; !ok {
				t.Errorf("header %s not in the real answer", k)
			}
		}
	}
	for _, k := range append(sameValue, "content-type") {
		if low[k] != real.headers[k] {
			t.Errorf("%s = %q, real %q", k, low[k], real.headers[k])
		}
	}
}

func newDriver(t *testing.T) driver.Driver {
	t.Helper()
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// probeReq is the drift probe: one message, max_tokens 16, the drift key.
func probeReq(stream bool) *core.Request {
	return &core.Request{
		Endpoint: core.EndpointChat, Model: "gpt-4o-mini", Stream: stream, IncludeUsage: stream, MaxTokens: 16,
		Messages: []core.Message{{Role: "user", Content: "Responda só: ok"}},
		Headers:  map[string]string{"authorization": "Bearer wwz-key-drift"}, Received: time.Now(),
	}
}

var rateLimitHeaders = []string{
	"x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests",
	"x-ratelimit-remaining-tokens", "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens",
}

// TestRealChat: the non-streaming body is the real one byte for byte when
// the generated values (id, created, content, usage) are the same.
func TestRealChat(t *testing.T) {
	real := loadReal(t, "chat")
	resp := &core.Response{
		ID: "chatcmpl-Ef2YMuGdqwgKVZW6Q1InrPJS2S1hR", Model: "gpt-4o-mini-2024-07-18", Created: time.Unix(1793449970, 0),
		Content: "Ok.", FinishReason: core.FinishStop, Usage: core.Usage{PromptTokens: 13, CompletionTokens: 2},
		Headers: map[string]string{"x-request-id": "req_923a1a13e4ca4419bd25f8ef60ef0666"},
	}
	w := core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, probeReq(false), resp, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(w.Body()); got != real.body {
		t.Errorf("body\n wwz  %q\n real %q", got, real.body)
	}
	checkHeaders(t, real, w.Headers(), append([]string{"openai-version"}, rateLimitHeaders...)...)
}

var obfuscationRe = regexp.MustCompile(`"obfuscation":"[0-9A-Za-z]+"`)

// TestRealChatStream: every chunk is the real one, obfuscation aside (a
// random string in both).
func TestRealChatStream(t *testing.T) {
	real := loadReal(t, "chat_stream")
	resp := &core.Response{
		ID: "chatcmpl-Ef2YNZkVXtXK9ZR7XlrVAAzqxhdyR", Model: "gpt-4o-mini-2024-07-18", Created: time.Unix(1793449971, 0),
		Headers: map[string]string{"x-request-id": "req_9da56283c24c4a7baedfdf7a6d9f6d5a"},
	}
	w := core.NewMemWriter()
	s := newDriver(t).StreamWriter(w, probeReq(true), nil)
	for _, err := range []error{
		s.Start(resp), s.TextDelta("Ok"), s.TextDelta("."),
		s.Finish(core.FinishStop, core.Usage{PromptTokens: 13, CompletionTokens: 2}, true),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := string(w.Body())
	if n := len(obfuscationRe.FindAllString(got, -1)); n != 5 {
		t.Errorf("%d chunks with obfuscation, want 5", n)
	}
	norm := func(s string) string { return obfuscationRe.ReplaceAllString(s, `"obfuscation":"<o>"`) }
	if norm(got) != norm(real.body) {
		t.Errorf("stream\n wwz  %s\n real %s", got, real.body)
	}
	checkHeaders(t, real, w.Headers(), append([]string{"openai-version"}, rateLimitHeaders...)...)
}

// TestRealErrorAuth: the 401 of the drift key, mask, text/plain and the
// x-error-json copy included.
func TestRealErrorAuth(t *testing.T) {
	real := loadReal(t, "error_auth")
	req := probeReq(false)
	req.Headers["authorization"] = "Bearer sk-invalid-wwz-drift"
	req.RequestID = "req_0867938ae9114affb4ff98837ebf514c"
	w := core.NewMemWriter()
	newDriver(t).WriteError(w, req, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	if w.StatusCode() != real.status || string(w.Body()) != real.body {
		t.Errorf("%d %q\n real %d %q", w.StatusCode(), w.Body(), real.status, real.body)
	}
	checkHeaders(t, real, w.Headers(), "x-error-json", "x-openai-authorization-error", "x-openai-ide-error-code", "x-request-id")
}

// TestRealErrorModel: the edge's 404, 4 spaces and a final newline.
func TestRealErrorModel(t *testing.T) {
	real := loadReal(t, "error_model")
	req := probeReq(false)
	req.Model = "wwz-drift-modelo-inexistente"
	w := core.NewMemWriter()
	newDriver(t).WriteError(w, req, &core.Error{Kind: core.ErrModelNotFound}, nil)
	if w.StatusCode() != real.status || string(w.Body()) != real.body {
		t.Errorf("%d %q\n real %d %q", w.StatusCode(), w.Body(), real.status, real.body)
	}
	checkHeaders(t, real, w.Headers())
	if id := w.Headers()["x-request-id"]; !regexp.MustCompile(`^req_[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("x-request-id %q, want req_ + 32 hex", id)
	}
}

// TestRealModels: the listing item of the probe model is the real one, in
// the real layout, and the headers are the listing's (no x-ratelimit-*).
func TestRealModels(t *testing.T) {
	real := loadReal(t, "models")
	start := strings.Index(real.body, "    {\n      \"id\": \"gpt-4o-mini\",")
	if start < 0 {
		t.Fatal("gpt-4o-mini not in the real listing")
	}
	end := strings.Index(real.body[start:], "\n    }") + start + len("\n    }")
	want := "{\n  \"object\": \"list\",\n  \"data\": [\n" + real.body[start:end] + "\n  ]\n}"
	m := &config.ModelConfig{ID: "gpt-4o-mini", Created: 1721172741, OwnedBy: "system", RetiresAt: "2027-01-14"}
	w := core.NewMemWriter()
	newDriver(t).ListModels(w, nil, []driver.ModelInfo{{Cfg: m}})
	if string(w.Body()) != want {
		t.Errorf("models\n wwz  %q\n real %q", w.Body(), want)
	}
	checkHeaders(t, real, w.Headers(), "openai-version")
	if id := w.Headers()["x-request-id"]; !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("x-request-id %q, want a UUID", id)
	}

	// Without retires_at the item carries shutdown_date: null, like the
	// real omni-moderation-latest (task 230); extras follow, and GetModel
	// answers one item in the same layout.
	start = strings.Index(real.body, "    {\n      \"id\": \"omni-moderation-latest\",")
	if start < 0 {
		t.Fatal("omni-moderation-latest not in the real listing")
	}
	end = strings.Index(real.body[start:], "\n    }") + start + len("\n    }")
	want = "{\n  \"object\": \"list\",\n  \"data\": [\n" + real.body[start:end] + "\n  ]\n}"
	m = &config.ModelConfig{ID: "omni-moderation-latest", Created: 1731689265, OwnedBy: "system"}
	w = core.NewMemWriter()
	newDriver(t).ListModels(w, nil, []driver.ModelInfo{{Cfg: m}})
	if string(w.Body()) != want {
		t.Errorf("models without retires_at\n wwz  %q\n real %q", w.Body(), want)
	}
	m = &config.ModelConfig{ID: "m", Created: 1, OwnedBy: "o", Extra: map[string]any{"z": 1, "a": "x"}}
	w = core.NewMemWriter()
	newDriver(t).GetModel(w, nil, &driver.ModelInfo{Cfg: m}, "m")
	if got := string(w.Body()); got != "{\n  \"id\": \"m\",\n  \"object\": \"model\",\n  \"created\": 1,\n  \"owned_by\": \"o\",\n  \"shutdown_date\": null,\n  \"a\": \"x\",\n  \"z\": 1\n}" {
		t.Errorf("GetModel %q", got)
	}
	m.Extra = map[string]any{"bad": make(chan int)}
	w = core.NewMemWriter()
	newDriver(t).GetModel(w, nil, &driver.ModelInfo{Cfg: m}, "m")
	if w.StatusCode() != 500 {
		t.Errorf("unencodable extra: %d %s", w.StatusCode(), w.Body())
	}
}

// TestDriverBasics: the protocol name, the reasoning_field option and an
// endpoint the driver does not parse.
func TestDriverBasics(t *testing.T) {
	for _, opts := range []map[string]string{{"reasoning_field": "reasoning_content"}, {"reasoning_field": "inline_tag"}, {"x": "1"}} {
		if _, err := driver.New("openai", opts); err == nil {
			t.Errorf("options %v accepted, want error (ADR 0046)", opts)
		}
	}
	d, err := driver.New("openai", map[string]string{"reasoning_field": "hidden"})
	if err != nil || d.Protocol() != "openai" {
		t.Fatalf("driver: %v", err)
	}
	w := core.NewMemWriter()
	resp := &core.Response{ID: "chatcmpl-x", Model: "m", Created: time.Unix(1, 0), Reasoning: "why", Content: "ok", FinishReason: core.FinishStop}
	if err := d.WriteResponse(w, probeReq(false), resp, nil); err != nil || strings.Contains(string(w.Body()), "why") {
		t.Errorf("reasoning_field option: %v %s", err, w.Body())
	}
	if _, e := d.ParseRequest(driver.KindEmbeddings, []byte(`{}`), driver.MapHeaders{}); e == nil || e.Kind != core.ErrNotFound {
		t.Errorf("unsupported kind: %v", e)
	}
}
