package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// wwzEnv lists every WWZ_* variable the command or config.ApplyEnv reads;
// tests blank them so the developer's environment does not leak in.
var wwzEnv = []string{
	"WWZ_CONFIG", "WWZ_PORT", "WWZ_LISTEN", "WWZ_ADMIN_PORT", "WWZ_ADMIN_LISTEN",
	"WWZ_ADMIN_TOKEN", "WWZ_SEED", "WWZ_AUTH", "WWZ_STORE", "WWZ_VALKEY_ADDR",
	"WWZ_VALKEY_PASSWORD", "WWZ_LOG_LEVEL", "WWZ_REUSEPORT", "WWZ_SCENARIOS_DIR",
	"WWZ_VALIDATE_ON_LOAD", "POD_NAME", "POD_IP",
}

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range wwzEnv {
		t.Setenv(k, "")
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type instance struct {
	t      *testing.T
	a      addrs
	cancel context.CancelFunc
	done   chan int
	logs   *syncBuffer
}

// startServe runs `wwz serve` in-process on free ports and waits until both
// planes listen. The instance is shut down at the end of the test.
func startServe(t *testing.T, args ...string) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	in := &instance{t: t, cancel: cancel, done: make(chan int, 1), logs: &syncBuffer{}}
	readyc := make(chan addrs, 1)
	args = append([]string{"-port", "0", "-admin-port", "0", "-log-format", "text", "-shutdown-timeout", "5s"}, args...)
	go func() {
		in.done <- runServe(ctx, args, io.Discard, in.logs, func(a addrs) { readyc <- a })
	}()
	select {
	case in.a = <-readyc:
	case code := <-in.done:
		cancel()
		t.Fatalf("serve exited with %d before listening:\n%s", code, in.logs)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatalf("serve did not start:\n%s", in.logs)
	}
	t.Cleanup(func() { in.stop() })
	return in
}

// stop cancels the context (SIGTERM equivalent) and returns the exit code.
func (in *instance) stop() int {
	in.t.Helper()
	in.cancel()
	select {
	case code := <-in.done:
		in.done <- code // keep it for later calls
		return code
	case <-time.After(10 * time.Second):
		in.t.Fatalf("serve did not stop:\n%s", in.logs)
		return -1
	}
}

// url builds a loopback URL for a bound address such as "[::]:1234".
func url(addr, path string) string {
	_, port, _ := net.SplitHostPort(addr)
	return "http://127.0.0.1:" + port + path
}

func do(t *testing.T, method, u string, headers map[string]string, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

var (
	oaiAuth  = map[string]string{"Authorization": "Bearer wwz-key-01", "Content-Type": "application/json"}
	anthAuth = map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "Content-Type": "application/json"}
)

func TestServeDefaultConfigAnswersOpenAIAndAnthropic(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WWZ_ADMIN_TOKEN", "s3cret")
	in := startServe(t, "-seed", "42")

	t.Run("openai", func(t *testing.T) {
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`)
		var out struct {
			Object  string `json:"object"`
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil || out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
			t.Fatalf("status %d (%v): %s", code, err, body)
		}
	})
	t.Run("openai stream", func(t *testing.T) {
		code, h, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
		if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") ||
			!strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
			t.Fatalf("status %d ct %q:\n%s", code, h.Get("Content-Type"), body)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/messages"), anthAuth, `{"model":"wwz-fast","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`)
		var out struct {
			Type    string            `json:"type"`
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil || out.Type != "message" || out.Role != "assistant" || len(out.Content) == 0 {
			t.Fatalf("status %d (%v): %s", code, err, body)
		}
	})
	t.Run("anthropic stream", func(t *testing.T) {
		code, h, body := do(t, "POST", url(in.a.Data, "/v1/messages"), anthAuth, `{"model":"wwz-fast","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
		if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d:\n%s", code, body)
		}
		for _, ev := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
			if !strings.Contains(body, ev) {
				t.Fatalf("missing %q:\n%s", ev, body)
			}
		}
	})
	t.Run("admin requires token", func(t *testing.T) {
		if code, h, body := do(t, "GET", url(in.a.Admin, "/admin/config"), nil, ""); code != 401 || h.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("no token: status %d: %s", code, body)
		}
		if code, _, body := do(t, "GET", url(in.a.Admin, "/admin/config"), map[string]string{"Authorization": "Bearer wrong"}, ""); code != 401 {
			t.Fatalf("wrong token: status %d: %s", code, body)
		}
		code, _, body := do(t, "GET", url(in.a.Admin, "/admin/config"), map[string]string{"Authorization": "Bearer s3cret"}, "")
		if code != 200 || !strings.Contains(body, `"seed"`) {
			t.Fatalf("token: status %d: %s", code, body)
		}
	})
	t.Run("health", func(t *testing.T) {
		if code, _, body := do(t, "GET", url(in.a.Admin, "/health/ready"), nil, ""); code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
	})

	if code := in.stop(); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, in.logs)
	}
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(url(in.a.Data, ""), "http://"), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("data plane still accepting after shutdown")
	}
	if !strings.Contains(in.logs.String(), "wwz stopped") {
		t.Fatalf("missing shutdown log:\n%s", in.logs)
	}
}

func TestServeWithoutTokenBindsAdminToLoopback(t *testing.T) {
	cleanEnv(t)
	in := startServe(t)
	host, _, _ := net.SplitHostPort(in.a.Admin)
	if host != "127.0.0.1" {
		t.Fatalf("admin bound to %q, want 127.0.0.1", host)
	}
	if !strings.Contains(in.logs.String(), "loopback only") {
		t.Fatalf("missing warning:\n%s", in.logs)
	}
	if code, _, body := do(t, "GET", url(in.a.Admin, "/admin/config"), nil, ""); code != 200 {
		t.Fatalf("loopback without token: status %d: %s", code, body)
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

const reloadBase = `
server: {seed: 7}
models:
  - id: before-reload
    protocols: [openai, anthropic]
    latency: fast
`

func TestServeReloadFromFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WWZ_ADMIN_TOKEN", "tok")
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, reloadBase)
	t.Setenv("WWZ_CONFIG", path) // env selects the file
	in := startServe(t)
	auth := map[string]string{"Authorization": "Bearer tok"}
	chat := func(model string) int {
		code, _, _ := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return code
	}
	if c := chat("before-reload"); c != 200 {
		t.Fatalf("before reload: %d", c)
	}

	writeFile(t, path, strings.ReplaceAll(reloadBase, "before-reload", "after-reload"))
	if code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, ""); code != 200 {
		t.Fatalf("reload: status %d: %s", code, body)
	}
	if c := chat("after-reload"); c != 200 {
		t.Fatalf("after reload: %d", c)
	}
	if c := chat("before-reload"); c != 404 {
		t.Fatalf("old model after reload: %d", c)
	}

	// Changes that need a restart are refused and the current config stays.
	writeFile(t, path, reloadBase+"store: {kind: valkey, addr: [\"127.0.0.1:6379\"]}\n")
	code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, "")
	if code < 400 || !strings.Contains(body, "store.kind") {
		t.Fatalf("restart-only change: status %d: %s", code, body)
	}
	// An invalid file is refused too.
	writeFile(t, path, "server: {bogus: 1}\n")
	if code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, ""); code < 400 {
		t.Fatalf("invalid file: status %d: %s", code, body)
	}
	if c := chat("after-reload"); c != 200 {
		t.Fatalf("config lost after failed reloads: %d", c)
	}

	// Flags and env are re-applied on reload: the admin port given as a
	// flag survives a patch that tries to move it.
	code, _, body = do(t, "POST", url(in.a.Admin, "/admin/config"), map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/json"}, `{"server":{"seed":99}}`)
	if code != 200 {
		t.Fatalf("patch: status %d: %s", code, body)
	}
	if code, _, body := do(t, "GET", url(in.a.Admin, "/admin/config"), auth, ""); code != 200 || !strings.Contains(body, `"seed":99`) {
		t.Fatalf("patched config: status %d: %s", code, body)
	}
}

func TestServeLoadsScenarios(t *testing.T) {
	cleanEnv(t)
	in := startServe(t, "-scenarios-dir", filepath.Join("..", "..", "scenarios"))
	code, _, body := do(t, "GET", url(in.a.Admin, "/admin/scenarios"), nil, "")
	if code != 200 || !strings.Contains(body, "file-agent-flow") {
		t.Fatalf("status %d: %s", code, body)
	}
}

func TestServeLoadsScenarioFiles(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	abs, err := filepath.Abs(filepath.Join("..", "..", "scenarios", "retry-on-error.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	writeFile(t, bad, "name: [\n")
	cfg := filepath.Join(dir, "wwz.yaml")
	writeFile(t, cfg, "scenarios:\n  files: [\""+abs+"\", \""+bad+"\", \"/nonexistent.yaml\"]\n")
	in := startServe(t, "-config", cfg)
	code, _, body := do(t, "GET", url(in.a.Admin, "/admin/scenarios"), nil, "")
	if code != 200 || !strings.Contains(body, "retry-on-error") {
		t.Fatalf("status %d: %s", code, body)
	}
	if logs := in.logs.String(); !strings.Contains(logs, "bad.yaml") || !strings.Contains(logs, "nonexistent.yaml") {
		t.Fatalf("invalid files not reported:\n%s", logs)
	}
}

// unreachableScenario parses (serve without the flag loads it) but fails the
// wwz validate checks.
const unreachableScenario = "name: unreachable\nsteps:\n  - id: first\n    content: hi\n  - id: second\n    expect: {turn: 2}\n"

func TestServeValidateOnLoadBoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), unreachableScenario)
	repo := filepath.Join("..", "..", "scenarios")
	cases := []struct {
		name string
		env  string
		args []string
		ok   bool
		msg  string
	}{
		{"without flag the file is loaded", "", []string{"-scenarios-dir", dir}, true, ""},
		{"flag aborts the boot", "", []string{"-validate-on-load", "-scenarios-dir", dir}, false, "unreachable"},
		{"env 1 aborts the boot", "1", []string{"-scenarios-dir", dir}, false, "a.yaml"},
		{"env true aborts the boot", "TRUE", []string{"-scenarios-dir", dir}, false, "validate-on-load"},
		{"flag false wins over env", "1", []string{"-validate-on-load=false", "-scenarios-dir", dir}, true, ""},
		{"env other value is off", "yes", []string{"-scenarios-dir", dir}, true, ""},
		{"missing dir aborts the boot", "", []string{"-validate-on-load", "-scenarios-dir", filepath.Join(dir, "nope")}, false, "scenarios:"},
		{"valid scenarios boot", "1", []string{"-scenarios-dir", repo}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("WWZ_VALIDATE_ON_LOAD", tc.env)
			if tc.ok {
				in := startServe(t, tc.args...)
				if code := in.stop(); code != exitOK {
					t.Fatalf("exit %d:\n%s", code, in.logs)
				}
				return
			}
			var out, errb syncBuffer
			code := runServe(context.Background(), tc.args, &out, &errb, func(addrs) { t.Error("must not start") })
			if code != exitError || !strings.Contains(errb.String(), tc.msg) {
				t.Fatalf("code %d, stderr:\n%s", code, errb.String())
			}
		})
	}
}

func TestServeValidateOnLoadReload(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WWZ_ADMIN_TOKEN", "tok")
	dir := t.TempDir()
	scen := filepath.Join(dir, "scenarios")
	if err := os.Mkdir(scen, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(scen, "good.yaml"), "name: good\nsteps:\n  - id: s\n    content: hi\n")
	path := filepath.Join(dir, "wwz.yaml")
	base := reloadBase + "scenarios: {dir: \"" + scen + "\"}\n"
	writeFile(t, path, base)
	in := startServe(t, "-config", path, "-validate-on-load")
	auth := map[string]string{"Authorization": "Bearer tok"}
	chat := func(model string) int {
		code, _, _ := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return code
	}

	// A new model plus an invalid scenario: the whole reload is refused.
	writeFile(t, path, strings.ReplaceAll(base, "before-reload", "after-reload"))
	writeFile(t, filepath.Join(scen, "bad.yaml"), unreachableScenario)
	code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, "")
	if code < 400 || !strings.Contains(body, "unreachable") {
		t.Fatalf("reload with invalid scenario: status %d: %s", code, body)
	}
	if c := chat("before-reload"); c != 200 {
		t.Fatalf("previous config lost: %d", c)
	}
	if c := chat("after-reload"); c != 404 {
		t.Fatalf("refused config applied: %d", c)
	}
	if code, _, body := do(t, "GET", url(in.a.Admin, "/admin/scenarios"), auth, ""); code != 200 || !strings.Contains(body, "good") || strings.Contains(body, `"unreachable"`) {
		t.Fatalf("scenarios after refused reload: status %d: %s", code, body)
	}

	// Fixing the scenario lets the same reload through.
	if err := os.Remove(filepath.Join(scen, "bad.yaml")); err != nil {
		t.Fatal(err)
	}
	if code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, ""); code != 200 {
		t.Fatalf("reload after fix: status %d: %s", code, body)
	}
	if c := chat("after-reload"); c != 200 {
		t.Fatalf("after reload: %d", c)
	}
	// A restart-only change is still refused by restartCheck first.
	writeFile(t, path, base+"store: {kind: valkey, addr: [\"127.0.0.1:6379\"]}\n")
	if code, _, body := do(t, "POST", url(in.a.Admin, "/admin/config"), auth, ""); code < 400 || !strings.Contains(body, "store.kind") {
		t.Fatalf("restart-only change: status %d: %s", code, body)
	}
}

func TestServeStartupErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want int
		msg  string
	}{
		{"missing config file", nil, []string{"-config", "/nonexistent/wwz.yaml"}, exitError, "nonexistent"},
		{"invalid config", nil, []string{"-auth", "bogus"}, exitError, "auth"},
		{"unknown store kind", map[string]string{"WWZ_STORE": "bogus"}, nil, exitError, "store.kind"},
		{"bad admin port env", map[string]string{"WWZ_ADMIN_PORT": "nope"}, nil, exitUsage, "WWZ_ADMIN_PORT"},
		{"listen and port", nil, []string{"-listen", ":1", "-port", "2"}, exitUsage, "mutually exclusive"},
		{"admin listen and port", nil, []string{"-admin-listen", ":1", "-admin-port", "2"}, exitUsage, "mutually exclusive"},
		{"port out of range", nil, []string{"-port", "70000"}, exitUsage, "out of range"},
		{"negative timeout", nil, []string{"-shutdown-timeout", "-1s"}, exitUsage, "negative"},
		{"unknown flag", nil, []string{"-nope"}, exitUsage, "nope"},
		{"extra argument", nil, []string{"extra"}, exitUsage, "extra"},
		{"port in use", nil, nil, exitError, "listen"},
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			args := tc.args
			if tc.name == "port in use" {
				args = []string{"-listen", busy.Addr().String()}
			}
			var out, errb syncBuffer
			code := runServe(context.Background(), args, &out, &errb, func(addrs) { t.Error("must not start") })
			if code != tc.want || !strings.Contains(errb.String(), tc.msg) {
				t.Fatalf("code %d (want %d), stderr:\n%s", code, tc.want, errb.String())
			}
		})
	}
}

func TestPrecedenceFlagEnvYAMLDefault(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, "server: {listen: \":2222\", seed: 5}\nadmin: {listen: \":3333\"}\n")
	load := func(args ...string) *config.Config {
		t.Helper()
		f, err := parseServeFlags(append([]string{"-config", path}, args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		port, err := envPort(envAdminPort)
		if err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(f.config)
		if err != nil {
			t.Fatal(err)
		}
		f.override(port)(c)
		return c
	}

	// YAML over default.
	c := load()
	if c.Server.Listen != ":2222" || c.Admin.Listen != ":3333" || c.Server.Seed != 5 || c.Server.Auth != "keys" {
		t.Fatalf("yaml: %+v %+v", c.Server.Listen, c.Admin.Listen)
	}
	// Env over YAML.
	t.Setenv("WWZ_PORT", "4444")
	t.Setenv("WWZ_ADMIN_PORT", "5555")
	t.Setenv("WWZ_SEED", "6")
	c = load()
	if c.Server.Listen != ":4444" || c.Admin.Listen != ":5555" || c.Server.Seed != 6 {
		t.Fatalf("env: %q %q %d", c.Server.Listen, c.Admin.Listen, c.Server.Seed)
	}
	// Flag over env.
	c = load("-port", "7777", "-admin-port", "8888", "-seed", "9", "-auth", "none", "-log-level", "debug", "-log-format", "text", "-scenarios-dir", "/x")
	if c.Server.Listen != ":7777" || c.Admin.Listen != ":8888" || c.Server.Seed != 9 || c.Server.Auth != "none" ||
		c.Logging.Level != "debug" || c.Logging.Format != "text" || c.Scenarios.Dir != "/x" {
		t.Fatalf("flags: %+v", c)
	}
	c = load("-listen", "127.0.0.1:1", "-admin-listen", "127.0.0.1:2")
	if c.Server.Listen != "127.0.0.1:1" || c.Admin.Listen != "127.0.0.1:2" {
		t.Fatalf("listen flags: %q %q", c.Server.Listen, c.Admin.Listen)
	}
	// -seed 0 set explicitly still wins (0 = random).
	if c = load("-seed", "0"); c.Server.Seed != 0 {
		t.Fatalf("explicit zero seed: %d", c.Server.Seed)
	}
	// WWZ_CONFIG is used only when -config is absent.
	t.Setenv("WWZ_CONFIG", "/from/env.yaml")
	f, err := parseServeFlags(nil, io.Discard)
	if err != nil || f.config != "/from/env.yaml" {
		t.Fatalf("WWZ_CONFIG: %v %q", err, f.config)
	}
	if f, _ = parseServeFlags([]string{"-config", path}, io.Discard); f.config != path {
		t.Fatalf("-config over WWZ_CONFIG: %q", f.config)
	}
}

func TestRestartCheck(t *testing.T) {
	base, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	check := restartCheck(base)
	same, _ := config.Parse(nil)
	if err := check(same); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	moved, _ := config.Parse([]byte("server: {listen: \":1\"}\nadmin: {listen: \":2\"}\n"))
	if err := check(moved); err == nil || !strings.Contains(err.Error(), "server.listen") || !strings.Contains(err.Error(), "admin.listen") {
		t.Fatalf("moved: %v", err)
	}
	bad, _ := config.Parse(nil)
	bad.Drivers = append(bad.Drivers, config.DriverMount{Name: "nope", Prefix: "/x"})
	if err := check(bad); err == nil {
		t.Fatal("unknown driver accepted")
	}
}

func TestNewLogger(t *testing.T) {
	var b bytes.Buffer
	c := &config.Config{}
	c.Logging.Level, c.Logging.Format = "warn", "json"
	l := newLogger(c, &b)
	l.Info("hidden")
	l.Warn("shown")
	if s := b.String(); strings.Contains(s, "hidden") || !strings.Contains(s, `"msg":"shown"`) {
		t.Fatalf("json logger: %s", s)
	}
	b.Reset()
	c.Logging.Level, c.Logging.Format = "garbage", "text"
	l = newLogger(c, &b)
	l.Debug("hidden")
	l.Info("shown")
	if s := b.String(); strings.Contains(s, "hidden") || !strings.Contains(s, "msg=shown") {
		t.Fatalf("text logger: %s", s)
	}
}

func TestNewStoreUnknownKind(t *testing.T) {
	c := &config.Config{Store: config.StoreConfig{Kind: "bogus"}}
	if _, err := newStore(context.Background(), c, "", nil, nil); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v", err)
	}
}

// Valkey unreachable at boot: wwz serves anyway (store.on_down: local) and
// exposes the open circuit.
func TestServeValkeyDownAtBoot(t *testing.T) {
	cleanEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	t.Setenv("WWZ_VALKEY_ADDR", closed)
	in := startServe(t)
	code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("data plane with Valkey down: %d %s", code, body)
	}
	if _, _, m := do(t, "GET", url(in.a.Admin, "/metrics"), nil, ""); !strings.Contains(m, "wwz_store_circuit_open 1") {
		t.Fatalf("metrics without the open circuit:\n%s", m)
	}
	if !strings.Contains(in.logs.String(), "circuit open") || !strings.Contains(in.logs.String(), "store=valkey") {
		t.Fatalf("logs:\n%s", in.logs)
	}
}

// Two wwz instances with WWZ_STORE=valkey share a key's rpm limit.
func TestServeValkeySharedLimit(t *testing.T) {
	if os.Getenv("WWZ_TEST_VALKEY") == "" {
		t.Skip("Valkey de teste ausente: suba com `docker compose up -d valkey` e rode `make verify-quick`")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 {
			t.Fatalf("WWZ_TEST_VALKEY_DB=%q: tests never use database 0", s)
		}
		db = n
	}
	// go test runs packages in parallel and internal/limits FLUSHes the
	// test database before every subtest: this package uses the one below.
	db--
	if db <= 0 {
		t.Skip("no spare test database below WWZ_TEST_VALKEY_DB")
	}
	old := valkeyDB
	valkeyDB = db
	t.Cleanup(func() { valkeyDB = old })
	v, err := limits.NewValkey(context.Background(), config.StoreConfig{Addr: []string{addr}, ReadTimeout: time.Second}, limits.ValkeyOptions{DB: db, Heartbeat: -1})
	if err != nil {
		t.Fatal(err)
	}
	c := v.Client()
	if err := c.Do(context.Background(), c.B().Arbitrary("FLUSHDB").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	_ = v.Close()

	cleanEnv(t)
	t.Setenv("WWZ_STORE", "valkey")
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 3}
store: {read_timeout: 1s}
keys:
  - {key: wwz-shared-01, label: shared, limits: {rpm: 4}}
`)
	a := startServe(t, "-config", path)
	b := startServe(t, "-config", path)
	auth := map[string]string{"Authorization": "Bearer wwz-shared-01", "Content-Type": "application/json"}
	ok, limited := 0, 0
	for i := range 10 {
		in := a
		if i%2 == 1 {
			in = b
		}
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), auth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
		switch code {
		case 200:
			ok++
		case 429:
			limited++
		default:
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	if ok != 4 || limited != 6 {
		t.Fatalf("two instances: %d ok, %d limited; want the shared 4 and 6", ok, limited)
	}
}
