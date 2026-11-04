package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/tools"
)

const testToken = "s3cr3t"

// newTestServer serves a fresh executor (seed 1, no real waiting).
func newTestServer(t *testing.T, token string, opts ...tools.Option) *httptest.Server {
	t.Helper()
	cfg := tools.DefaultConfig()
	cfg.Seed = 1
	ex, err := tools.New(cfg, append([]tools.Option{tools.WithSleep(noSleep)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(ex, token).handler())
	t.Cleanup(ts.Close)
	return ts
}

type req struct {
	method, path, body string
	header             map[string]string
	noAuth             bool
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func do(t *testing.T, ts *httptest.Server, r req) resp {
	t.Helper()
	hr, err := http.NewRequestWithContext(context.Background(), r.method, ts.URL+r.path, strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	if !r.noAuth {
		hr.Header.Set("Authorization", "Bearer "+testToken)
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func errType(t *testing.T, b []byte) string {
	t.Helper()
	var e errorBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil || e.Error.Message == "" {
		t.Fatalf("not an error envelope: %v %s", err, b)
	}
	return e.Error.Type
}

func createSession(t *testing.T, ts *httptest.Server, body string, h map[string]string) sessionResponse {
	t.Helper()
	r := do(t, ts, req{method: "POST", path: "/v1/sessions", body: body, header: h})
	if r.code != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.code, r.body)
	}
	var s sessionResponse
	if err := json.Unmarshal(r.body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestOpenAPIRoutesAndCodes walks cmd/wwz-tools/openapi.yaml: every
// operation is routed, every case answers a status declared for its
// operation, and every declared status is exercised.
func TestOpenAPIRoutesAndCodes(t *testing.T) {
	b, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{} // "METHOD /path STATUS"
	ops := 0
	for p, m := range doc.Paths {
		for method, op := range m {
			ops++
			for code := range op.Responses {
				declared[strings.ToUpper(method)+" "+p+" "+code] = true
			}
		}
	}
	if ops != 6 {
		t.Fatalf("contract has %d operations", ops)
	}

	ts := newTestServer(t, testToken)
	sid := createSession(t, ts, `{"profile":"coder"}`, nil).Session
	call := func(body string, h map[string]string) req {
		return req{method: "POST", path: "/v1/call", body: body, header: h}
	}
	cases := []struct {
		op string
		r  req
		// want is the expected status.
		want int
	}{
		{"GET /healthz", req{method: "GET", path: "/healthz", noAuth: true}, 200},
		{"GET /metrics", req{method: "GET", path: "/metrics"}, 200},
		{"GET /metrics", req{method: "GET", path: "/metrics", noAuth: true}, 401},
		{"GET /v1/tools", req{method: "GET", path: "/v1/tools?profile=coder&format=openai"}, 200},
		{"GET /v1/tools", req{method: "GET", path: "/v1/tools?format=xml"}, 400},
		{"GET /v1/tools", req{method: "GET", path: "/v1/tools", noAuth: true}, 401},
		{"POST /v1/sessions", req{method: "POST", path: "/v1/sessions"}, 201},
		{"POST /v1/sessions", req{method: "POST", path: "/v1/sessions", body: `{"profile":"dev"}`}, 400},
		{"POST /v1/sessions", req{method: "POST", path: "/v1/sessions", noAuth: true}, 401},
		{"DELETE /v1/sessions/{id}", req{method: "DELETE", path: "/v1/sessions/" + sid}, 204},
		{"DELETE /v1/sessions/{id}", req{method: "DELETE", path: "/v1/sessions/" + sid}, 404},
		{"DELETE /v1/sessions/{id}", req{method: "DELETE", path: "/v1/sessions/x", noAuth: true}, 401},
		{"POST /v1/call", call(`{"session":"s1","name":"list_dir"}`, nil), 200},
		{"POST /v1/call", call(`{"session":"s1"}`, nil), 400},
		{"POST /v1/call", req{method: "POST", path: "/v1/call", body: `{}`, noAuth: true}, 401},
		{"POST /v1/call", call(`{"session":"s1","name":"rm_rf"}`, nil), 404},
		{"POST /v1/call", call(`{"session":"s1","name":"list_dir"}`, map[string]string{"x-wwz-tools-outcome": "timeout"}), 504},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		r := do(t, ts, c.r)
		if r.code != c.want {
			t.Errorf("%s %s: got %d want %d (%s)", c.r.method, c.r.path, r.code, c.want, r.body)
			continue
		}
		key := c.op + " " + strconv.Itoa(r.code)
		if !declared[key] {
			t.Errorf("%s: status not declared in openapi.yaml", key)
		}
		seen[key] = true
		if r.code >= 400 {
			if errType(t, r.body); r.code == 404 && strings.Contains(string(r.body), "no route") {
				t.Errorf("%s: not routed", c.op)
			}
		}
	}
	for key := range declared {
		if !seen[key] {
			t.Errorf("declared but not exercised: %s", key)
		}
	}
}

func TestAuth(t *testing.T) {
	ts := newTestServer(t, testToken)
	r := do(t, ts, req{method: "GET", path: "/v1/tools", noAuth: true, header: map[string]string{"Authorization": "Bearer errado"}})
	if r.code != 401 || r.header.Get("WWW-Authenticate") != "Bearer" || errType(t, r.body) != "unauthorized" {
		t.Fatalf("wrong token: %d %v %s", r.code, r.header, r.body)
	}
	if r := do(t, ts, req{method: "GET", path: "/healthz", noAuth: true}); r.code != 200 || string(r.body) != "ok" {
		t.Fatalf("healthz: %d %q", r.code, r.body)
	}
	open := newTestServer(t, "")
	if r := do(t, open, req{method: "GET", path: "/metrics", noAuth: true}); r.code != 200 {
		t.Fatalf("no token configured: %d", r.code)
	}
	if r := do(t, open, req{method: "GET", path: "/nada"}); r.code != 404 || errType(t, r.body) != "not_found" {
		t.Fatalf("unknown route: %d %s", r.code, r.body)
	}
}

func TestListToolsHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	r := do(t, ts, req{method: "GET", path: "/v1/tools"})
	var raw []tools.Tool
	if r.code != 200 || r.header.Get("Content-Type") != "application/json" || json.Unmarshal(r.body, &raw) != nil || len(raw) != 8 {
		t.Fatalf("raw: %d %s", r.code, r.body)
	}
	names := make([]string, len(raw))
	for i, x := range raw {
		names[i] = x.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("not lexicographic: %v", names)
	}
	if r := do(t, ts, req{method: "GET", path: "/v1/tools?profile=chat"}); strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("chat: %s", r.body)
	}
	if r := do(t, ts, req{method: "GET", path: "/v1/tools?profile=dev"}); r.code != 400 || errType(t, r.body) != "invalid_request" {
		t.Fatalf("profile: %d %s", r.code, r.body)
	}
}

func TestSessionsHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	s := createSession(t, ts, `{"profile":"coder","seed":42}`, nil)
	if s.Profile != tools.ProfileCoder || s.Seed != 42 || len(s.Files) < 12 || !sort.StringsAreSorted(s.Files) || !slices.Contains(s.Files, "README.md") || !strings.HasPrefix(s.Session, "ses_") {
		t.Fatalf("session: %+v", s)
	}
	// x-wwz-seed wins over the body seed; the same seed gives the same tree.
	h := createSession(t, ts, `{"profile":"coder","seed":7}`, map[string]string{"x-wwz-seed": "42"})
	if h.Seed != 42 || !slices.Equal(h.Files, s.Files) {
		t.Fatalf("header seed: %+v", h)
	}
	// Empty body: profile all and the server seed (1).
	if d := createSession(t, ts, "", nil); d.Profile != tools.ProfileAll || d.Seed != 1 {
		t.Fatalf("defaults: %+v", d)
	}
	if d := createSession(t, ts, `{}`, map[string]string{"x-wwz-seed": "0"}); d.Seed < 1 {
		t.Fatalf("effective seed: %+v", d)
	}
	for _, c := range []struct {
		body string
		h    map[string]string
	}{
		{`{"profile":"coder","seed":-1}`, nil},
		{`{"seed":"x"}`, nil},
		{`{"profile":"coder","extra":1}`, nil},
		{`{} {}`, nil},
		{`{`, nil},
		{`{}`, map[string]string{"x-wwz-seed": "abc"}},
		{`{}`, map[string]string{"x-wwz-seed": "-3"}},
		{`{"profile":"dev"}`, nil},
	} {
		r := do(t, ts, req{method: "POST", path: "/v1/sessions", body: c.body, header: c.h})
		if r.code != 400 || errType(t, r.body) != "invalid_request" {
			t.Errorf("%q %v: %d %s", c.body, c.h, r.code, r.body)
		}
	}
	if r := do(t, ts, req{method: "DELETE", path: "/v1/sessions/" + s.Session}); r.code != 204 || len(r.body) != 0 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r := do(t, ts, req{method: "DELETE", path: "/v1/sessions/" + s.Session}); r.code != 404 || errType(t, r.body) != "not_found" {
		t.Fatalf("delete again: %d %s", r.code, r.body)
	}
}

func decodeCall(t *testing.T, r resp) callResult {
	t.Helper()
	if r.code != 200 {
		t.Fatalf("call: %d %s", r.code, r.body)
	}
	var c callResult
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	post := func(body string, h map[string]string) resp {
		return do(t, ts, req{method: "POST", path: "/v1/call", body: body, header: h})
	}
	c := decodeCall(t, post(`{"session":"a","name":"read_file","arguments":{"path":"README.md"},"tool_call_id":"call_1"}`, nil))
	if c.Outcome != tools.OutcomeSuccess || c.IsError || c.ToolCallID != "call_1" || c.Content == "" {
		t.Fatalf("object args: %+v", c)
	}
	c2 := decodeCall(t, post(`{"session":"a2","name":"read_file","arguments":"{\"path\":\"README.md\"}"}`, nil))
	if c2.Content != c.Content || c2.ToolCallID != "" {
		t.Fatalf("string args: %+v", c2)
	}
	if c := decodeCall(t, post(`{"session":"a","name":"read_file","arguments":"not json"}`, nil)); c.Outcome != tools.OutcomeNatural || !c.IsError {
		t.Fatalf("bad string args: %+v", c)
	}
	if c := decodeCall(t, post(`{"session":"a","name":"list_dir"}`, nil)); c.Outcome != tools.OutcomeSuccess {
		t.Fatalf("absent args: %+v", c)
	}
	for _, o := range []tools.Outcome{tools.OutcomeError, tools.OutcomeInvalidPayload, tools.OutcomeSuccess} {
		c := decodeCall(t, post(`{"session":"a","name":"list_dir"}`, map[string]string{"x-wwz-tools-outcome": string(o)}))
		if c.Outcome != o || c.IsError != (o == tools.OutcomeError) {
			t.Fatalf("forced %s: %+v", o, c)
		}
	}
	r := post(`{"session":"a","name":"list_dir"}`, map[string]string{"x-wwz-tools-outcome": "timeout"})
	if r.code != 504 || errType(t, r.body) != "timeout" || !strings.Contains(string(r.body), "tool timed out after 30s") {
		t.Fatalf("timeout: %d %s", r.code, r.body)
	}
	// Tool outside the session profile → 404.
	sid := createSession(t, ts, `{"profile":"coder"}`, nil).Session
	if r := post(`{"session":"`+sid+`","name":"web_search","arguments":{"query":"x"}}`, nil); r.code != 404 || errType(t, r.body) != "not_found" {
		t.Fatalf("out of profile: %d %s", r.code, r.body)
	}
	// x-wwz-seed seeds the session created by the call.
	h := map[string]string{"x-wwz-seed": "42"}
	a := decodeCall(t, post(`{"session":"seeded-1","name":"read_file","arguments":{"path":"README.md"}}`, h))
	b := decodeCall(t, post(`{"session":"seeded-2","name":"read_file","arguments":{"path":"README.md"}}`, h))
	if a.Content != b.Content {
		t.Fatalf("seeded sessions differ: %+v %+v", a, b)
	}

	big := `{"session":"a","name":"write_file","arguments":{"path":"x","content":"` + strings.Repeat("a", maxBody) + `"}}`
	for _, c := range []struct {
		body string
		h    map[string]string
	}{
		{big, nil},
		{``, nil},
		{`{"session":"a","name":"list_dir","arguments":42}`, nil},
		{`{"session":"a","name":"list_dir","arguments":[1]}`, nil},
		{`{"session":"a","name":"list_dir","arguments":true}`, nil},
		{`{"session":"a","name":"list_dir","arguments":null}`, nil},
		{`{"session":"a","name":"list_dir","extra":1}`, nil},
		{`{"name":"list_dir"}`, nil},
		{`{"session":"a b","name":"list_dir"}`, nil},
		{`{"session":"` + strings.Repeat("s", 129) + `","name":"list_dir"}`, nil},
		{`{"session":"a","name":""}`, nil},
		{`{"session":"a","name":"list_dir"}`, map[string]string{"x-wwz-tools-outcome": "natural_error"}},
		{`{"session":"a","name":"list_dir"}`, map[string]string{"x-wwz-seed": "x"}},
	} {
		r := post(c.body, c.h)
		if r.code != 400 || errType(t, r.body) != "invalid_request" {
			t.Errorf("%.80q %v: %d %.200s", c.body, c.h, r.code, r.body)
		}
	}
}

func TestMetricsHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	createSession(t, ts, `{}`, nil)
	do(t, ts, req{method: "POST", path: "/v1/call", body: `{"session":"m","name":"list_dir"}`})
	do(t, ts, req{method: "POST", path: "/v1/call", body: `{"session":"m","name":"list_dir"}`, header: map[string]string{"x-wwz-tools-outcome": "timeout"}})
	r := do(t, ts, req{method: "GET", path: "/metrics"})
	body := string(r.body)
	for _, want := range []string{
		`wwz_tools_calls_total{tool="list_dir",outcome="success"} 1`,
		`wwz_tools_calls_total{tool="list_dir",outcome="timeout"} 1`,
		`wwz_tools_call_duration_seconds_count{tool="list_dir"} 2`,
		"wwz_tools_sessions 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", r.header.Get("Content-Type"))
	}
}

// A client that disconnects during the simulated wait gets nothing written.
func TestCallClientGone(t *testing.T) {
	cfg := tools.DefaultConfig()
	ex, _ := tools.New(cfg, tools.WithSleep(noSleep))
	h := newServer(ex, "").handler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/call", strings.NewReader(`{"session":"a","name":"list_dir"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Fatalf("wrote %d %v %q", w.Code, w.Header(), w.Body)
	}
}

func TestWriteErrInternal(t *testing.T) {
	w := httptest.NewRecorder()
	writeErr(w, errors.New("boom"))
	if w.Code != 500 || errType(t, w.Body.Bytes()) != "internal" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// --- runServe ---

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureListen records the address of the next listener of runServe.
func captureListen(t *testing.T) <-chan string {
	t.Helper()
	orig := listen
	t.Cleanup(func() { listen = orig })
	addrs := make(chan string, 1)
	listen = func(network, addr string) (net.Listener, error) {
		ln, err := orig(network, addr)
		if err == nil {
			addrs <- ln.Addr().String()
		}
		return ln, err
	}
	return addrs
}

func TestServeLifecycle(t *testing.T) {
	t.Setenv("WWZ_TOOLS_TOKEN", testToken)
	t.Setenv("WWZ_TOOLS_SEED", "")
	t.Setenv("WWZ_TOOLS_LISTEN", "")
	addrs := captureListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	var logs syncBuf
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, []string{"--listen", "127.0.0.1:0", "--seed", "9"}, &logs) }()
	addr := <-addrs
	ts := &httptest.Server{URL: "http://" + addr, Config: &http.Server{}}
	if r := do(t, ts, req{method: "GET", path: "/healthz", noAuth: true}); r.code != 200 {
		t.Fatalf("healthz: %d", r.code)
	}
	if r := do(t, ts, req{method: "GET", path: "/v1/tools", noAuth: true}); r.code != 401 {
		t.Fatalf("auth: %d", r.code)
	}
	if s := createSession(t, ts, `{}`, nil); s.Seed != 9 {
		t.Fatalf("--seed: %+v", s)
	}
	cancel()
	if code := <-done; code != exitOK || !strings.Contains(logs.String(), "encerrado") {
		t.Fatalf("exit %d logs %s", code, logs.String())
	}
}

// Shutdown that does not finish within shutdownTimeout closes the
// connections (a call blocked in a 30 s timeout).
func TestServeForcedShutdown(t *testing.T) {
	t.Setenv("WWZ_TOOLS_TOKEN", "")
	addrs := captureListen(t)
	origOpts, origTimeout := execOptions, shutdownTimeout
	t.Cleanup(func() { execOptions, shutdownTimeout = origOpts, origTimeout })
	shutdownTimeout = 20 * time.Millisecond
	inFlight := make(chan struct{}, 1)
	execOptions = []tools.Option{tools.WithSleep(func(ctx context.Context, d time.Duration) error {
		if d >= time.Second {
			inFlight <- struct{}{}
			<-ctx.Done()
		}
		return ctx.Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, []string{"--listen", "127.0.0.1:0"}, io.Discard) }()
	addr := <-addrs
	go func() {
		r, err := http.NewRequestWithContext(context.Background(), "POST", "http://"+addr+"/v1/call", strings.NewReader(`{"session":"a","name":"list_dir"}`))
		if err != nil {
			return
		}
		r.Header.Set("x-wwz-tools-outcome", "timeout")
		if res, err := http.DefaultClient.Do(r); err == nil {
			_ = res.Body.Close()
		}
	}()
	<-inFlight
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept boom") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestServeFailures(t *testing.T) {
	t.Setenv("WWZ_TOOLS_SEED", "")
	t.Setenv("WWZ_TOOLS_LISTEN", "")
	ctx := context.Background()
	for _, args := range [][]string{{"--bogus"}, {"extra"}, {"--seed", "x"}} {
		if code := runServe(ctx, args, io.Discard); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
	if code := runServe(ctx, []string{"-h"}, io.Discard); code != exitOK {
		t.Fatalf("-h: %d", code)
	}
	// Port in use → 1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var logs syncBuf
	if code := runServe(ctx, []string{"--listen", ln.Addr().String()}, &logs); code != exitError || !strings.Contains(logs.String(), "listen") {
		t.Fatalf("port in use: %d %s", code, logs.String())
	}
	// Invalid configuration → 1.
	t.Setenv("WWZ_TOOLS_SEED", "abc")
	if code := runServe(ctx, nil, io.Discard); code != exitError {
		t.Fatalf("bad env: %d", code)
	}
	t.Setenv("WWZ_TOOLS_SEED", "")
	// Serve failing (listener broken) → 1.
	orig := listen
	t.Cleanup(func() { listen = orig })
	listen = func(string, string) (net.Listener, error) { return failingListener{}, nil }
	if code := runServe(ctx, nil, io.Discard); code != exitError {
		t.Fatalf("serve failure: %d", code)
	}
}

// run dispatches serve (through the full command line).
func TestRunServeDispatch(t *testing.T) {
	if code, _, _ := runCmd(t, "serve", "--bogus"); code != exitUsage {
		t.Fatalf("serve --bogus: %d", code)
	}
}
