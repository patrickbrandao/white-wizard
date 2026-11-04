package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// routeFixture is a real route miss recorded on 2026-10-31 (task 235):
// unknown paths and wrong methods, with a valid key (<key>), an invalid
// one (sent as is) or none. Header values that vary per call are
// <request_id>, <uuid> and <ms>.
type routeFixture struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Request  struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

var (
	reRouteAnthReq = regexp.MustCompile(`^req_011C[0-9A-Za-z]{20}$`)
	reRouteOAIReq  = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	reRouteUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	reRouteBodyReq = regexp.MustCompile(`req_011C[0-9A-Za-z]{20}`)
)

// routeHeaders are the headers the fixtures keep; the wwz answer must
// carry exactly those of them the real sent.
var routeHeaders = []string{
	"content-type", "request-id", "x-request-id", "x-should-retry", "allow", "cache-control", "expires",
	"referrer-policy", "x-frame-options", "anthropic-organization-id", "anthropic-workspace-id",
	"openai-version", "openai-processing-ms", "openai-organization", "openai-project",
}

// TestRealFixturesRouteMiss replays testdata/real/<provider>/routes/ with
// the provider's driver at the root: status, the kept headers and the
// body byte for byte.
func TestRealFixturesRouteMiss(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "*", "routes", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	harnesses := map[string]*harness{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx routeFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		h := harnesses[fx.Provider]
		if h == nil {
			h = start(t, fixtureCfg+"keys: [{key: wwz-key-01}]\ndrivers: [{name: "+fx.Provider+"}]\n", nil)
			harnesses[fx.Provider] = h
		}
		t.Run(fx.Provider+"/"+fx.Name, func(t *testing.T) {
			if fx.Provider == "openai" && fx.Name == "embeddings_get" {
				t.Skip("/v1/embeddings is outside the MVP (ADR 0054): an unknown route in wwz")
			}
			h.t = t
			defer func(parent *testing.T) { h.t = parent }(h.t)
			hdr := map[string]string{}
			for k, v := range fx.Request.Headers {
				hdr[k] = strings.ReplaceAll(v, "<key>", "wwz-key-01")
			}
			resp := h.do(fx.Request.Method, fx.Request.Path, hdr, "")
			body := reRouteBodyReq.ReplaceAllString(string(resp.Body()), "<request_id>")
			if resp.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d: %s", resp.StatusCode(), fx.Response.Status, body)
			}
			if body != fx.Response.Body {
				t.Errorf("body\n wwz  %q\n real %q", body, fx.Response.Body)
			}
			checkRouteHeaders(t, fx, resp)
		})
	}
}

func checkRouteHeaders(t *testing.T, fx routeFixture, resp *fasthttp.Response) {
	t.Helper()
	for _, k := range routeHeaders {
		got := string(resp.Header.Peek(k))
		if k == "content-type" && !strings.Contains(strings.ToLower(resp.Header.String()), "content-type:") {
			got = "" // the client reports its default when none was sent
		}
		want, ok := fx.Response.Headers[k]
		switch {
		case !ok && got != "":
			t.Errorf("header %s = %q, not in the real answer", k, got)
		case !ok:
		case got == "":
			t.Errorf("header %s missing (real %q)", k, want)
		case want == "<request_id>" && !reRouteAnthReq.MatchString(got) && !reRouteOAIReq.MatchString(got),
			want == "<uuid>" && !reRouteUUID.MatchString(got),
			want == "<ms>" && strings.Trim(got, "0123456789") != "":
			t.Errorf("header %s = %q, not in the real format %s", k, got, want)
		case !strings.HasPrefix(want, "<") && got != want:
			t.Errorf("header %s = %q, real %q", k, got, want)
		}
	}
}

// TestOpenAIChatListAuth: GET /v1/chat/completions checks the key as the
// real does (task 315): no key and a wrong key are the same 401 as on the
// POST route, and the account headers follow the key.
func TestOpenAIChatListAuth(t *testing.T) {
	h := start(t, fixtureCfg+"keys: [{key: wwz-key-01, account: acme, project: p1}]\ndrivers: [{name: openai}]\n", nil)
	for _, auth := range []map[string]string{nil, {"authorization": "Bearer sk-bad"}} {
		get := h.do("GET", "/v1/chat/completions", auth, "")
		post := h.do("POST", "/v1/chat/completions", auth, `{}`)
		if get.StatusCode() != 401 || get.StatusCode() != post.StatusCode() {
			t.Fatalf("auth %v: GET %d, POST %d", auth, get.StatusCode(), post.StatusCode())
		}
		if string(get.Body()) != string(post.Body()) {
			t.Errorf("auth %v: GET body %q, POST %q", auth, get.Body(), post.Body())
		}
	}
	ok := h.do("GET", "/v1/chat/completions", map[string]string{"authorization": "Bearer wwz-key-01"}, "")
	if ok.StatusCode() != 200 || string(ok.Header.Peek("openai-organization")) != "acme" {
		t.Fatalf("%d org=%q", ok.StatusCode(), ok.Header.Peek("openai-organization"))
	}
}

// TestRouteMissMounts: a route miss under a mount prefix goes to the
// driver mounted there, with the path relative to the mount; a path no
// mount covers is OpenAI's 404; shared root paths go to the sniffed
// driver.
func TestRouteMissMounts(t *testing.T) {
	h := start(t, fixtureCfg+`keys: [{key: wwz-key-01}]
drivers:
  - {name: openai}
  - {name: anthropic}
  - {name: anthropic, prefix: /anthropic}
`, nil)
	anth := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	oai := map[string]string{"authorization": "Bearer wwz-key-01"}
	cases := []struct {
		method, path string
		h            map[string]string
		status       int
		body         string
	}{
		{"GET", "/anthropic/v1/nope", anth, 404, `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":null}`},
		{"GET", "/anthropic/nope", anth, 404, ""},
		{"DELETE", "/anthropic/v1/models", anth, 405, `{"type":"error","error":{"type":"invalid_request_error","message":"Method Not Allowed"},"request_id":"<request_id>"}`},
		{"GET", "/v1/nope", anth, 404, `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":null}`},
		{"GET", "/v1/nope", oai, 404, ""},
		{"DELETE", "/v1/models/gpt-4o-mini", oai, 405, "{\n  \"error\": {\n    \"message\": \"Invalid method for URL (DELETE /v1/models/gpt-4o-mini)\",\n    \"type\": \"invalid_request_error\",\n    \"param\": null,\n    \"code\": null\n  }\n}"},
		{"DELETE", "/anthropic/v1/models/claude-haiku-4-5", anth, 405, `{"type":"error","error":{"type":"invalid_request_error","message":"Method Not Allowed"},"request_id":"<request_id>"}`},
	}
	for _, c := range cases {
		resp := h.do(c.method, c.path, c.h, "")
		body := reRouteBodyReq.ReplaceAllString(string(resp.Body()), "<request_id>")
		if resp.StatusCode() != c.status || body != c.body {
			t.Errorf("%s %s: %d %q", c.method, c.path, resp.StatusCode(), body)
		}
	}
	if got := string(h.do("DELETE", "/anthropic/v1/models/claude-haiku-4-5", anth, "").Header.Peek("allow")); got != "GET,HEAD" {
		t.Errorf("allow %q", got)
	}
}

// TestRouteMissWithoutMount: no mount covers the path.
func TestRouteMissWithoutMount(t *testing.T) {
	cfg, err := config.Parse([]byte(fixtureCfg + "drivers: [{name: anthropic, prefix: /anthropic}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := buildRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, miss, ok := r.miss("GET", "/elsewhere", driver.MapHeaders{}); ok || miss.Path != "/elsewhere" {
		t.Fatalf("%v %+v", ok, miss)
	}
	h := start(t, fixtureCfg+"drivers: [{name: anthropic, prefix: /anthropic}]\n", nil)
	if resp := h.do("GET", "/elsewhere", nil, ""); resp.StatusCode() != 404 || len(resp.Body()) != 0 {
		t.Fatalf("%d %s", resp.StatusCode(), resp.Body())
	}
}

// TestAnthropicModelsAccountHeaders: /v1/models and /v1/models/{id} carry
// the account ids of the key, the same as its Messages answers (task 235).
func TestAnthropicModelsAccountHeaders(t *testing.T) {
	h := start(t, fixtureCfg+`keys: [{key: wwz-key-01, account: acme, project: p1}]
drivers: [{name: anthropic}]
`, nil)
	hdr := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	chat := h.do("POST", "/v1/messages", hdr, `{"model":"claude-haiku-4-5","max_tokens":5,"messages":[{"role":"user","content":"oi"}]}`)
	if chat.StatusCode() != 200 {
		t.Fatalf("messages: %d %s", chat.StatusCode(), chat.Body())
	}
	for _, path := range []string{"/v1/models", "/v1/models/claude-haiku-4-5", "/v1/models/nope"} {
		resp := h.do("GET", path, hdr, "")
		for _, k := range []string{"anthropic-organization-id", "anthropic-workspace-id"} {
			if got, want := string(resp.Header.Peek(k)), string(chat.Header.Peek(k)); got == "" || got != want {
				t.Errorf("%s: %s = %q, messages %q", path, k, got, want)
			}
		}
	}
}
