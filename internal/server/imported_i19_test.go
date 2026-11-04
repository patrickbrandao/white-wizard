package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// I19: faults de transporte nomeados (spec 017 I19, spec 009) seen by a real
// HTTP client over the fasthttp data plane: empty_response,
// malformed_chunk, random_data_then_close, connection_reset, corrupt_body.

func i19Cfg(kind string) string {
	return fastCfg + `
models:
  - {id: m-f, latency: fast, errors: {rate: 100, types: [` + kind + `]}}
`
}

const i19Body = `{"model":"m-f","messages":[{"role":"user","content":"hi"}]}`

// try sends one request and returns the response or the transport error.
func (h *harness) try(body string) (*fasthttp.Response, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod("POST")
	req.SetRequestURI("http://wwz/v1/chat/completions")
	for k, v := range oaiAuth {
		req.Header.Set(k, v)
	}
	req.Header.SetContentType("application/json")
	req.SetBodyString(body)
	resp := &fasthttp.Response{}
	err := h.cl.DoTimeout(req, resp, 5*time.Second)
	return resp, err
}

// TestI19EmptyResponse: I19 — 200 with an empty body.
func TestI19EmptyResponse(t *testing.T) {
	h := start(t, i19Cfg("empty_response"), nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, i19Body)
	if resp.StatusCode() != 200 || len(resp.Body()) != 0 {
		t.Fatalf("status %d body %q", resp.StatusCode(), resp.Body())
	}
}

// TestI19CorruptBody: I19 — 200 application/json whose body is not JSON.
func TestI19CorruptBody(t *testing.T) {
	h := start(t, i19Cfg("corrupt_body"), nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, i19Body)
	if resp.StatusCode() != 200 || len(resp.Body()) == 0 || json.Valid(resp.Body()) {
		t.Fatalf("status %d body %q", resp.StatusCode(), resp.Body())
	}
	if !strings.HasPrefix(string(resp.Header.ContentType()), "application/json") {
		t.Fatalf("content type %q", resp.Header.ContentType())
	}
}

// TestI19MalformedChunk: I19 — 200 text/event-stream with a broken data frame.
func TestI19MalformedChunk(t *testing.T) {
	h := start(t, i19Cfg("malformed_chunk"), nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, i19Body)
	if resp.StatusCode() != 200 || !strings.HasPrefix(string(resp.Header.ContentType()), "text/event-stream") {
		t.Fatalf("status %d ct %q", resp.StatusCode(), resp.Header.ContentType())
	}
	bad := false
	for _, l := range strings.Split(string(resp.Body()), "\n") {
		if strings.HasPrefix(l, "data: {") && !json.Valid([]byte(l[6:])) {
			bad = true
		}
	}
	if !bad {
		t.Fatalf("no malformed frame: %q", resp.Body())
	}
}

// TestI19RandomDataThenClose: I19 — garbage bytes then close: the client
// cannot parse an HTTP response.
func TestI19RandomDataThenClose(t *testing.T) {
	h := start(t, i19Cfg("random_data_then_close"), nil)
	if resp, err := h.try(i19Body); err == nil {
		t.Fatalf("want a transport error, got %d %q", resp.StatusCode(), resp.Body())
	}
}

// TestI19ConnectionReset: I19 — the connection closes with no response.
func TestI19ConnectionReset(t *testing.T) {
	h := start(t, i19Cfg("connection_reset"), nil)
	if resp, err := h.try(i19Body); err == nil {
		t.Fatalf("want a transport error, got %d %q", resp.StatusCode(), resp.Body())
	}
}

// TestI19ServerSurvivesFaults: I19 — after the faults the server still
// answers normal requests.
func TestI19ServerSurvivesFaults(t *testing.T) {
	h := start(t, i19Cfg("connection_reset")+`  - {id: m-ok, latency: fast, errors: none}
`, nil)
	for i := 0; i < 3; i++ {
		_, _ = h.try(i19Body)
	}
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, `{"model":"m-ok","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d after faults", resp.StatusCode())
	}
}

const i19AnthBody = `{"model":"m-f","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`

// TestI19CorruptBodyAnthropic: I19 — on the Anthropic route the corrupt body
// imitates a Messages object, not an OpenAI chat completion.
func TestI19CorruptBodyAnthropic(t *testing.T) {
	h := start(t, i19Cfg("corrupt_body"), nil)
	resp := h.do("POST", "/v1/messages", anthAuth, i19AnthBody)
	b := string(resp.Body())
	if resp.StatusCode() != 200 || json.Valid(resp.Body()) || strings.Contains(b, "chatcmpl") || !strings.Contains(b, `"type":"message"`) {
		t.Fatalf("status %d body %q", resp.StatusCode(), b)
	}
}

// TestI19MalformedChunkAnthropic: I19 — on the Anthropic route the broken
// frame is an Anthropic content_block_delta event.
func TestI19MalformedChunkAnthropic(t *testing.T) {
	h := start(t, i19Cfg("malformed_chunk"), nil)
	resp := h.do("POST", "/v1/messages", anthAuth, i19AnthBody)
	b := string(resp.Body())
	if resp.StatusCode() != 200 || strings.Contains(b, "chatcmpl") || strings.Contains(b, "[DONE]") || !strings.Contains(b, "event: content_block_delta") {
		t.Fatalf("status %d body %q", resp.StatusCode(), b)
	}
}

// customRouteDriver wraps a built-in driver and serves the Messages dialect
// on a route that does not end in "/messages" (task 098).
type customRouteDriver struct{ driver.Driver }

func (customRouteDriver) Routes() []driver.Route {
	return []driver.Route{
		{Method: "POST", Path: "/v1/claude", Kind: driver.KindMessages},
		{Method: "POST", Path: "/v1/gpt", Kind: driver.KindChat},
	}
}

func init() {
	driver.Register("wwz-test-custom-route", func(opts map[string]string) (driver.Driver, error) {
		d, err := driver.New("opencode", opts)
		if err != nil {
			return nil, err
		}
		return customRouteDriver{d}, nil
	})
}

const i19CustomMount = `drivers:
  - {name: wwz-test-custom-route, prefix: /gw}
  - {name: opencode}
`

// ocAuth carries no anthropic-version: the dialect must come from the route.
var ocAuth = map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}

// TestI19DialectFromRouteNotPath: corrupt_body and malformed_chunk follow
// the dialect of the matched route (task 098). A Messages route mounted at
// /gw/v1/claude (no "/messages" suffix, no anthropic-version) answers in
// the Anthropic shape; an OpenAI chat route answers in the OpenAI shape even
// when the client sends anthropic-version.
func TestI19DialectFromRouteNotPath(t *testing.T) {
	anthHdr := map[string]string{"Authorization": "Bearer wwz-key-01", "anthropic-version": "2023-06-01", "x-opencode-session": "ses_1"}
	cases := []struct {
		name, kind, path string
		hdr              map[string]string
		body             string
		anthropic        bool
	}{
		{"custom messages corrupt", "corrupt_body", "/gw/v1/claude", ocAuth, i19AnthBody, true},
		{"custom messages malformed", "malformed_chunk", "/gw/v1/claude", ocAuth, i19AnthBody, true},
		{"custom chat corrupt", "corrupt_body", "/gw/v1/gpt", anthHdr, i19Body, false},
		{"custom chat malformed", "malformed_chunk", "/gw/v1/gpt", anthHdr, i19Body, false},
		{"opencode chat corrupt", "corrupt_body", "/zen/go/v1/chat/completions", anthHdr, i19Body, false},
		{"opencode chat malformed", "malformed_chunk", "/zen/go/v1/chat/completions", anthHdr, i19Body, false},
		{"opencode messages malformed", "malformed_chunk", "/zen/go/v1/messages", ocAuth, i19AnthBody, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, i19Cfg(c.kind)+i19CustomMount, nil)
			resp := h.do("POST", c.path, c.hdr, c.body)
			b := string(resp.Body())
			if resp.StatusCode() != 200 {
				t.Fatalf("status %d body %q", resp.StatusCode(), b)
			}
			isAnth := strings.Contains(b, "msg_wwz") || strings.Contains(b, "event: content_block_delta")
			isOAI := strings.Contains(b, "chatcmpl")
			if isAnth != c.anthropic || isOAI == c.anthropic {
				t.Fatalf("want anthropic=%v, body %q", c.anthropic, b)
			}
		})
	}
}

// TestAnthropicDialectFallback: routes without a dialect of their own (models
// listing, usage) follow the driver protocol.
func TestAnthropicDialectFallback(t *testing.T) {
	anth, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !anthropicDialect(driver.KindModels, anth) || anthropicDialect(driver.KindModels, oc) {
		t.Fatal("models kind must follow the driver protocol")
	}
	if !anthropicDialect(driver.KindCountTokens, oc) || anthropicDialect(driver.KindResponses, anth) {
		t.Fatal("dialect kinds must win over the driver protocol")
	}
}
