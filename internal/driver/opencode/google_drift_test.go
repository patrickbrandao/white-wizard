package opencode_test

import (
	"bufio"
	"bytes"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The real answers of the Gemini route of OpenCode Go recorded by the
// first drift (task 156, internal/drift/testdata/real/opencode-google/):
// the translated reasoning counted in thoughtsTokenCount and the bodiless
// 404 of the missing models listing (task 297).

// driftProbeBody is the drift's chat probe (spec 017): "Responda só: ok"
// with the 256 tokens of the opencode probes.
const driftProbeBody = `{"contents":[{"role":"user","parts":[{"text":"Responda só: ok"}]}],"generationConfig":{"maxOutputTokens":256}}`

// loadDrift reads <probe>-real.txt ("HTTP <status>", "Name: value" lines,
// a blank line and the body) as a fixture of the request path, keeping
// only the headers the shape fixtures keep.
func loadDrift(t *testing.T, probe, path string) shapeFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "drift", "testdata", "real", "opencode-google", probe+"-real.txt"))
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := bytes.Cut(data, []byte("\n\n"))
	var fx shapeFixture
	fx.Name = probe
	fx.Request.Path = path
	fx.Request.Body = driftProbeBody
	fx.Request.Headers = map[string]string{"x-goog-api-key": "<key>", "x-opencode-session": "<session>"}
	fx.Response.Body = string(body)
	fx.Response.Headers = map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Scan()
	if fx.Response.Status, err = strconv.Atoi(strings.TrimPrefix(sc.Text(), "HTTP ")); err != nil {
		t.Fatal(err)
	}
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ": ")
		switch k = strings.ToLower(k); {
		case k == "x-opencode-log-id":
			fx.Response.Headers[k] = "<log_id>"
		case relevant[k] || k == "content-length":
			fx.Response.Headers[k] = v
		}
	}
	return fx
}

// TestRealDriftGoogleThoughts: the translation reports the reasoning in
// thoughtsTokenCount (candidatesTokenCount without it, the total and
// opencode.tokens.output with it), whole and streamed, byte for byte.
func TestRealDriftGoogleThoughts(t *testing.T) {
	d := newDriver(t)
	const base = "/inference/go/google/v1beta/models/deepseek-v4.1-flash"
	fx := loadDrift(t, "chat", base+":generateContent")
	delete(fx.Response.Headers, "content-length") // the wwz always sends it
	checkGoogleBody(t, d, fx)
	checkGoogleStream(t, d, loadDrift(t, "chat_stream", base+":streamGenerateContent?alt=sse"))

	// Reasoning beyond the completion is clamped; none leaves the field
	// out (gen_gpt_model).
	h := driver.MapHeaders{"x-wwz-path": base + ":generateContent"}
	for _, c := range []struct {
		u    core.Usage
		want string
	}{
		{core.Usage{PromptTokens: 5, CompletionTokens: 4, ReasoningTokens: 9}, `"candidatesTokenCount":0,"totalTokenCount":9,"thoughtsTokenCount":4,`},
		{core.Usage{PromptTokens: 5, CompletionTokens: 4}, `"candidatesTokenCount":4,"totalTokenCount":9,"cost"`},
	} {
		w := core.NewMemWriter()
		_ = d.WriteResponse(w, &core.Request{Headers: h}, &core.Response{ID: "x", Model: "deepseek-v4.1-flash", Usage: c.u}, nil)
		if !strings.Contains(string(w.Body()), c.want) {
			t.Errorf("%+v: %s", c.u, w.Body())
		}
	}
}

// TestRealDriftGoogleRouteMiss: a route the gateway does not serve is a
// 404 without a body before the key, with x-opencode-log-id under
// /inference only (conferred on 2026-10-31).
func TestRealDriftGoogleRouteMiss(t *testing.T) {
	d := newDriver(t)
	rm, ok := driver.As[driver.RouteMisser](d)
	if !ok {
		t.Fatal("opencode is not a RouteMisser")
	}
	fx := loadDrift(t, "models", "/inference/go/google/v1beta/models")
	for _, c := range []struct {
		miss driver.RouteMiss
		auth *core.Error
		log  bool
	}{
		{driver.RouteMiss{Method: "GET", Path: fx.Request.Path}, nil, true},
		{driver.RouteMiss{Method: "GET", Path: "/inference/go/v1/chat/completions", Allow: []string{"POST"}, Kind: driver.KindChat}, &core.Error{Kind: core.ErrInvalidAPIKey}, true},
		{driver.RouteMiss{Method: "GET", Path: "/zen/go/v1/chat/completions", Allow: []string{"POST"}, Kind: driver.KindChat}, nil, false},
		{driver.RouteMiss{Method: "POST", Path: "/zen/v1/nope"}, nil, false},
	} {
		w := core.NewMemWriter()
		rm.WriteRouteMiss(w, &core.Request{}, c.miss, c.auth)
		if w.StatusCode() != fx.Response.Status || len(w.Body()) != 0 || w.Headers()["Content-Length"] != "0" {
			t.Errorf("%+v: %d %q %v", c.miss, w.StatusCode(), w.Body(), w.Headers())
		}
		if got := w.Headers()["x-opencode-log-id"]; (got != "") != c.log || (c.log && !reLogID.MatchString(got)) {
			t.Errorf("%+v: log id %q", c.miss, got)
		}
	}
	if fx.Response.Headers["x-opencode-log-id"] != "<log_id>" || fx.Response.Headers["content-length"] != "0" {
		t.Errorf("real: %v", fx.Response.Headers)
	}
}

// TestRouteMissSitePage: outside the APIs the real answers the site's HTML
// 404 page (task 371), echoing the escaped path, without x-opencode-log-id.
func TestRouteMissSitePage(t *testing.T) {
	rm, _ := driver.As[driver.RouteMisser](newDriver(t))
	for _, p := range []string{"/zen/nope", "/zen/go/openai/v1/chat/completions", "/v1/chat/completions", `/x"><b>`} {
		w := core.NewMemWriter()
		rm.WriteRouteMiss(w, &core.Request{}, driver.RouteMiss{Method: "GET", Path: p}, nil)
		body := string(w.Body())
		if w.StatusCode() != 404 || !strings.HasPrefix(body, "<!DOCTYPE html>") || w.Headers()["Content-Type"] != "text/html" ||
			w.Headers()["x-opencode-log-id"] != "" || strings.Contains(body, "{{path}}") ||
			!strings.Contains(body, `data-start-url="`+html.EscapeString(p)+`"`) {
			t.Errorf("%s: %d %.80q %v", p, w.StatusCode(), body, w.Headers())
		}
	}
}
