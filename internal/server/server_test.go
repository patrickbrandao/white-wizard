package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

const (
	latCfg = `
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`
	fastCfg = "server: {seed: 42}\n" + latCfg
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func newEngine(t testing.TB, yaml string) *engine.Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewMetrics(metrics.New(1000), nil)
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), m, quietLog)
	e.Recorder = engine.NewRecorder(100)
	return e
}

type harness struct {
	t   *testing.T
	s   *Server
	eng *engine.Engine
	ln  *fasthttputil.InmemoryListener
	cl  *fasthttp.Client
}

// start serves yaml on an in-memory listener. Connections dial from peer.
func start(t *testing.T, yaml string, peer net.Addr) *harness {
	t.Helper()
	eng := newEngine(t, yaml)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln := fasthttputil.NewInmemoryListener()
	go func() { _ = s.Serve(ln) }()
	h := &harness{t: t, s: s, eng: eng, ln: ln}
	h.cl = &fasthttp.Client{Dial: func(string) (net.Conn, error) { return h.dial(peer) }}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		noLeaks(t)
	})
	return h
}

func (h *harness) dial(peer net.Addr) (net.Conn, error) {
	if peer == nil {
		return h.ln.Dial()
	}
	return h.ln.DialWithLocalAddr(peer)
}

func (h *harness) do(method, path string, headers map[string]string, body string) *fasthttp.Response {
	h.t.Helper()
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(method)
	req.SetRequestURI("http://wwz" + path)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
	}
	resp := &fasthttp.Response{}
	if err := h.cl.DoTimeout(req, resp, 10*time.Second); err != nil {
		h.t.Fatal(err)
	}
	return resp
}

var (
	oaiAuth  = map[string]string{"Authorization": "Bearer wwz-key-01"}
	anthAuth = map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
)

// noLeaks fails when a goroutine of a request is still alive.
func noLeaks(t *testing.T) {
	t.Helper()
	markers := []string{"server.(*fastWriter)", "server.watchDisconnect", "engine.(*Engine).stream", "engine.(*Engine).Handle"}
	deadline := time.Now().Add(3 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		leak := ""
		for _, m := range markers {
			if strings.Contains(stacks, m) {
				leak = m
			}
		}
		if leak == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak (%s):\n%s", leak, stacks)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenAIChat(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body(), &out); err != nil || out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
		t.Fatalf("bad body (%v): %s", err, resp.Body())
	}
	if len(resp.Header.Peek("x-request-id")) == 0 {
		t.Fatalf("missing x-request-id: %s", resp.Header.String())
	}
}

func TestOpenAIChatStream(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	body := string(resp.Body())
	if resp.StatusCode() != 200 || !strings.HasPrefix(string(resp.Header.ContentType()), "text/event-stream") {
		t.Fatalf("status %d ct %q: %s", resp.StatusCode(), resp.Header.ContentType(), body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("bad stream:\n%s", body)
	}
}

func TestAnthropicMessages(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("POST", "/v1/messages", anthAuth, `{"model":"wwz-fast","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Body(), &out); err != nil || out.Type != "message" || out.Role != "assistant" || len(out.Content) == 0 {
		t.Fatalf("bad body (%v): %s", err, resp.Body())
	}
	if len(resp.Header.Peek("request-id")) == 0 {
		t.Fatalf("missing request-id: %s", resp.Header.String())
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("POST", "/v1/messages", anthAuth, `{"model":"wwz-fast","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	body := string(resp.Body())
	if resp.StatusCode() != 200 || !strings.HasPrefix(string(resp.Header.ContentType()), "text/event-stream") {
		t.Fatalf("status %d: %s", resp.StatusCode(), body)
	}
	for _, ev := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, ev) {
			t.Fatalf("missing %q:\n%s", ev, body)
		}
	}
}

func TestAuthErrorInProviderFormat(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("POST", "/v1/messages", map[string]string{"x-api-key": "nope", "anthropic-version": "2023-06-01"}, `{"model":"wwz-fast","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 401 || !strings.Contains(string(resp.Body()), `"authentication_error"`) {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
}

func TestSniffSharedModelsPath(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("GET", "/v1/models", oaiAuth, "")
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"object": "list"`) {
		t.Fatalf("openai list: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("GET", "/v1/models", anthAuth, "")
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"has_more"`) {
		t.Fatalf("anthropic list: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("GET", "/v1/models/wwz-fast", anthAuth, "")
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"type":"model"`) {
		t.Fatalf("anthropic model: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("GET", "/v1/models/nope", oaiAuth, "")
	if resp.StatusCode() != 404 {
		t.Fatalf("unknown model: %d %s", resp.StatusCode(), resp.Body())
	}
}

func TestMountPrefixes(t *testing.T) {
	h := start(t, fastCfg+`
drivers:
  - {name: openai, prefix: /openai/}
  - {name: anthropic, prefix: /anthropic}
`, nil)
	resp := h.do("POST", "/openai/v1/chat/completions", oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"chat.completion"`) {
		t.Fatalf("prefixed openai: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("POST", "/anthropic/v1/messages", anthAuth, `{"model":"wwz-fast","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"type":"message"`) {
		t.Fatalf("prefixed anthropic: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("GET", "/anthropic/v1/models", oaiAuth, "") // no sniffing under a prefix
	if resp.StatusCode() != 200 || !strings.Contains(string(resp.Body()), `"has_more"`) {
		t.Fatalf("prefixed anthropic models: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("POST", "/v1/chat/completions", oaiAuth, `{}`)
	if resp.StatusCode() != 404 || len(resp.Body()) != 0 { // OpenAI's unknown path (task 235)
		t.Fatalf("root must be unmounted: %d %s", resp.StatusCode(), resp.Body())
	}
	resp = h.do("DELETE", "/openai/v1/chat/completions", oaiAuth, "")
	if resp.StatusCode() != 404 {
		t.Fatalf("wrong method: %d %s", resp.StatusCode(), resp.Body())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		t.Fatalf("405 body is not JSON: %s", resp.Body())
	}
}

func TestXWWZPathIsMountRelative(t *testing.T) {
	cfg, err := config.Parse([]byte(fastCfg + `
drivers:
  - {name: opencode, prefix: /opencode}
  - {name: openai}
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := buildRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.lookup("POST", "/opencode/zen/go/v1/chat/completions", driver.MapHeaders{})
	if m == nil || m.drv.Name() != "opencode" || m.path != "/zen/go/v1/chat/completions" {
		t.Fatalf("opencode match: %+v", m)
	}
	m, _ = r.lookup("GET", "/v1/models/a/b", driver.MapHeaders{"authorization": "Bearer x"})
	if m == nil || m.drv.Name() != "openai" || m.param != "a/b" || m.path != "/v1/models/a/b" || m.kind != driver.KindModel {
		t.Fatalf("wildcard match: %+v", m)
	}
	if m, other := r.lookup("GET", "/v1/models/", driver.MapHeaders{}); m != nil || other {
		t.Fatalf("empty id must not match: %+v %v", m, other)
	}

	// End to end: the client cannot spoof x-wwz-path; the opencode Go plan
	// is chosen from the mount-relative path.
	h := start(t, fastCfg+`
drivers:
  - {name: opencode, prefix: /opencode}
`, nil)
	resp := h.do("POST", "/opencode/zen/go/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1", "x-wwz-path": "/zen/v1/x"}, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 200 {
		t.Fatalf("opencode: %d %s", resp.StatusCode(), resp.Body())
	}
}

func TestBadConfigRejected(t *testing.T) {
	cfg, err := config.Parse([]byte(fastCfg))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Drivers = []config.DriverMount{{Name: "nope"}}
	if err := CheckConfig(cfg); err == nil {
		t.Fatal("unknown driver accepted")
	}
	cfg.Drivers = []config.DriverMount{{Name: "openai", Prefix: "x"}}
	if err := CheckConfig(cfg); err == nil {
		t.Fatal("relative prefix accepted")
	}
	cfg.Drivers = []config.DriverMount{{Name: "openai"}}
	cfg.Server.TrustedProxies = []string{"not-an-ip"}
	if err := CheckConfig(cfg); err == nil {
		t.Fatal("bad trusted proxy accepted")
	}
	if err := CheckConfig(nil); err == nil {
		t.Fatal("nil config accepted")
	}
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil engine accepted")
	}
}

func TestReloadFollowsEngineConfig(t *testing.T) {
	h := start(t, fastCfg, nil)
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`
	if resp := h.do("POST", "/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 200 {
		t.Fatalf("before reload: %d", resp.StatusCode())
	}
	next, err := config.Parse([]byte(fastCfg + "drivers: [{name: openai, prefix: /oai}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	h.eng.SetConfig(next)
	if resp := h.do("POST", "/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 404 {
		t.Fatalf("old route still served: %d", resp.StatusCode())
	}
	if resp := h.do("POST", "/oai/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 200 {
		t.Fatalf("new route: %d %s", resp.StatusCode(), resp.Body())
	}
	if h.s.Config() != next {
		t.Fatal("Config() is not the reloaded config")
	}

	// A config the data plane cannot serve keeps the previous routes.
	bad, _ := config.Parse([]byte(fastCfg))
	bad.Drivers = []config.DriverMount{{Name: "nope"}}
	h.eng.SetConfig(bad)
	for i := 0; i < 2; i++ {
		if resp := h.do("POST", "/oai/v1/chat/completions", oaiAuth, body); resp.StatusCode() != 200 {
			t.Fatalf("bad reload broke routes: %d", resp.StatusCode())
		}
	}

	// Update swaps the whole engine.
	eng2 := newEngine(t, fastCfg+"drivers: [{name: anthropic, prefix: /a}]\n")
	if err := h.s.Update(eng2); err != nil {
		t.Fatal(err)
	}
	if h.s.Engine() != eng2 {
		t.Fatal("engine not swapped")
	}
	if resp := h.do("POST", "/a/v1/messages", anthAuth, `{"model":"wwz-fast","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode() != 200 {
		t.Fatalf("after Update: %d %s", resp.StatusCode(), resp.Body())
	}
	eng3 := newEngine(t, fastCfg)
	eng3.SetConfig(bad)
	if err := h.s.Update(eng3); err == nil || h.s.Engine() != eng2 {
		t.Fatal("Update accepted an engine with a bad config")
	}
	if err := h.s.Update(nil); err == nil {
		t.Fatal("Update accepted nil")
	}
}

func TestMaxRPS(t *testing.T) {
	h := start(t, "server: {seed: 42, max_rps: 2}\n"+latCfg, nil)
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`
	var ok, rejected int
	var last *fasthttp.Response
	for i := 0; i < 6; i++ {
		resp := h.do("POST", "/v1/chat/completions", oaiAuth, body)
		switch resp.StatusCode() {
		case 200:
			ok++
		default:
			rejected++
			last = resp
		}
	}
	if ok != 2 || rejected != 4 {
		t.Fatalf("ok=%d rejected=%d", ok, rejected)
	}
	if last.StatusCode() != 503 || len(last.Header.Peek("Retry-After")) == 0 || !strings.Contains(string(last.Body()), `"error"`) {
		t.Fatalf("rejection: %d %s %s", last.StatusCode(), last.Header.String(), last.Body())
	}
	if n := h.eng.M.RateLimited.Value("openai", "server_max_rps"); n != 4 {
		t.Fatalf("metric = %d", n)
	}
}

// TestMaxRPSDialects: server.max_rps rejects in the dialect of the driver
// that owns the route, with Retry-After of at least 1 s (spec 010).
func TestMaxRPSDialects(t *testing.T) {
	cfg := "server: {seed: 42, max_rps: 1}\n" + latCfg + `
models:
  - {id: wwz-fast, protocols: [openai, anthropic, gemini], drivers: [openai, anthropic, gemini], latency: fast}
drivers:
  - {name: openai}
  - {name: anthropic, prefix: /anthropic}
  - {name: gemini, prefix: /gemini}
`
	gem := map[string]string{"x-goog-api-key": "wwz-key-01"}
	for _, c := range []struct {
		name, path, body string
		hdr              map[string]string
		status           int
		has              string
	}{
		{"anthropic", "/anthropic/v1/messages", `{"model":"wwz-fast","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, anthAuth, 529, `"overloaded_error"`},
		{"gemini", "/gemini/v1beta/models/wwz-fast:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, gem, 503, `"UNAVAILABLE"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, cfg, nil)
			if resp := h.do("POST", c.path, c.hdr, c.body); resp.StatusCode() != 200 {
				t.Fatalf("first request: %d %s", resp.StatusCode(), resp.Body())
			}
			resp := h.do("POST", c.path, c.hdr, c.body)
			if resp.StatusCode() != c.status || !strings.Contains(string(resp.Body()), c.has) {
				t.Fatalf("rejection: %d %s", resp.StatusCode(), resp.Body())
			}
			if n, err := strconv.Atoi(string(resp.Header.Peek("Retry-After"))); err != nil || n < 1 {
				t.Fatalf("Retry-After = %q", resp.Header.Peek("Retry-After"))
			}
			if n := h.eng.M.RateLimited.Value(c.name, "server_max_rps"); n != 1 {
				t.Fatalf("metric = %d", n)
			}
		})
	}
}

func TestGCRA(t *testing.T) {
	var g gcra
	now := time.Unix(1000, 0)
	if ok, _ := g.allow(0, now); !ok {
		t.Fatal("0 = unlimited")
	}
	for i := 0; i < 10; i++ {
		if ok, _ := g.allow(10, now); !ok {
			t.Fatalf("burst request %d rejected", i)
		}
	}
	ok, retry := g.allow(10, now)
	if ok || retry <= 0 || retry > 100*time.Millisecond {
		t.Fatalf("11th: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := g.allow(10, now.Add(100*time.Millisecond)); !ok {
		t.Fatal("token not refilled after 1/rps")
	}
	var huge gcra
	if ok, _ := huge.allow(2_000_000_000, now); !ok {
		t.Fatal("very high rate")
	}
}

func TestClientIPFromTrustedProxy(t *testing.T) {
	h := start(t, fastCfg, &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 4000})
	hdr := map[string]string{"Authorization": "Bearer wwz-key-01", "X-Forwarded-For": "8.8.8.8, 10.0.0.9"}
	if resp := h.do("POST", "/v1/chat/completions", hdr, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode() != 200 {
		t.Fatalf("status %d", resp.StatusCode())
	}
	recs := h.eng.Recorder.Last(1, nil)
	if len(recs) != 1 || recs[0].IP != "8.8.8.8" {
		t.Fatalf("records: %+v", recs)
	}

	// An untrusted peer cannot spoof the address.
	h2 := start(t, fastCfg, &net.TCPAddr{IP: net.ParseIP("8.8.4.4"), Port: 4000})
	if resp := h2.do("POST", "/v1/chat/completions", hdr, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode() != 200 {
		t.Fatalf("status %d", resp.StatusCode())
	}
	if recs := h2.eng.Recorder.Last(1, nil); len(recs) != 1 || recs[0].IP != "8.8.4.4" {
		t.Fatalf("records: %+v", recs)
	}
}

func TestReadHeadersJoinsRepeats(t *testing.T) {
	var rh fasthttp.RequestHeader
	rh.Add("X-Forwarded-For", "1.1.1.1")
	rh.Add("x-forwarded-for", "2.2.2.2")
	rh.Set("Authorization", "Bearer k")
	h := readHeaders(&rh)
	if h["x-forwarded-for"] != "1.1.1.1,2.2.2.2" || h.Get("authorization") != "Bearer k" {
		t.Fatalf("%v", h)
	}
}

// rawRequest writes an HTTP/1.1 request on c.
func rawRequest(t *testing.T, c net.Conn, path, body string, headers map[string]string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: wwz\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, len(body))
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
}

func TestStreamClientDisconnectCancels(t *testing.T) {
	for _, tc := range []struct {
		drv, path, body string
		hdr             map[string]string
	}{
		{"openai", "/v1/chat/completions", `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-delay-ms": "100"}},
		{"anthropic", "/v1/messages", `{"model":"wwz-fast","max_tokens":4000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "x-wwz-delay-ms": "100"}},
	} {
		t.Run(tc.drv, func(t *testing.T) {
			h := start(t, fastCfg, nil)
			c, err := h.ln.Dial()
			if err != nil {
				t.Fatal(err)
			}
			rawRequest(t, c, tc.path, tc.body, tc.hdr)
			br := bufio.NewReader(c)
			for { // wait for the first event: the stream is running
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(line, "data: ") {
					break
				}
			}
			if h.eng.M.Streams.Value(tc.drv) != 1 {
				t.Fatalf("streams open = %d", h.eng.M.Streams.Value(tc.drv))
			}
			_ = c.Close()
			// At 100 ms per token the full stream takes seconds; the
			// engine must stop well before that.
			waitFor(t, "stream cancelled", 2*time.Second, func() bool {
				return h.eng.M.Streams.Value(tc.drv) == 0 && h.eng.M.InFlight.Value(tc.drv) == 0
			})
		})
	}
}

func TestNonStreamClientDisconnectCancels(t *testing.T) {
	if !watchSupported {
		t.Skip("disconnect watcher not supported on this platform")
	}
	eng := newEngine(t, fastCfg)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		noLeaks(t)
	})
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rawRequest(t, c, "/v1/chat/completions", `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-delay-ms": "5000"})
	waitFor(t, "request in flight", 2*time.Second, func() bool { return eng.M.InFlight.Value("openai") == 1 })
	_ = c.Close()
	waitFor(t, "request cancelled", 2*time.Second, func() bool { return eng.M.InFlight.Value("openai") == 0 })

	// Keep-alive still works after a watched request: the watcher restores
	// the read deadline and leaves no bytes consumed.
	cl := &fasthttp.Client{}
	for i := 0; i < 3; i++ {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.Header.SetMethod("POST")
		req.SetRequestURI("http://" + ln.Addr().String() + "/v1/chat/completions")
		req.Header.Set("Authorization", "Bearer wwz-key-01")
		req.SetBodyString(`{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
		if err := cl.DoTimeout(req, resp, 5*time.Second); err != nil || resp.StatusCode() != 200 {
			t.Fatalf("keep-alive request %d: %v %d", i, err, resp.StatusCode())
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	cl.CloseIdleConnections()
}

func TestListenReuseport(t *testing.T) {
	eng := newEngine(t, "server: {seed: 42, reuseport: true, auth: none}\n"+latCfg)
	s, err := New(eng, Options{Logger: quietLog, Listeners: 3})
	if err != nil {
		t.Fatal(err)
	}
	lns, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := 1
	if reuseportSupported {
		want = 3
	}
	if len(lns) != want {
		t.Fatalf("listeners = %d, want %d", len(lns), want)
	}
	addr := lns[0].Addr().String()
	for _, ln := range lns {
		if ln.Addr().String() != addr {
			t.Fatalf("listeners on different addresses: %s vs %s", ln.Addr(), addr)
		}
		go func(ln net.Listener) { _ = s.Serve(ln) }(ln)
	}
	cl := &fasthttp.Client{}
	for i := 0; i < 6; i++ {
		code, body, err := cl.Get(nil, "http://"+addr+"/v1/models")
		if err != nil || code != 200 || !strings.Contains(string(body), `"object": "list"`) {
			t.Fatalf("GET /v1/models: %v %d %s", err, code, body)
		}
	}
	cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestListenAndServe(t *testing.T) {
	eng := newEngine(t, fastCfg)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	// A busy address fails fast.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := s.ListenAndServe(busy.Addr().String()); err == nil {
		t.Fatal("listening on a busy port succeeded")
	}
	// Default address comes from server.listen.
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	_ = free.Close()
	s.Config().Server.Listen = addr
	errc := make(chan error, 1)
	go func() { errc <- s.ListenAndServe("") }()
	cl := &fasthttp.Client{}
	waitFor(t, "server up", 2*time.Second, func() bool {
		code, _, err := cl.Get(nil, "http://"+addr+"/nope")
		return err == nil && code == 404
	})
	cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("ListenAndServe after Shutdown: %v", err)
	}
}

func TestShutdownCancelsLongStreams(t *testing.T) {
	h := start(t, fastCfg, nil)
	c, err := h.ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rawRequest(t, c, "/v1/chat/completions", `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-delay-ms": "200"})
	waitFor(t, "stream open", 2*time.Second, func() bool { return h.eng.M.Streams.Value("openai") == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.s.Shutdown(ctx); err == nil {
		t.Fatal("graceful shutdown should time out with a long stream open")
	}
	waitFor(t, "stream aborted", 2*time.Second, func() bool { return h.eng.M.Streams.Value("openai") == 0 })
}
