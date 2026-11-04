package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	// The default mounts are the drivers that declare one (config.DriverDecl).
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

const testToken = "t0k3n"

const fastCfg = `
server: {seed: 42}
admin: {debug: true}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newEngine(t *testing.T, doc string) *engine.Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewMetrics(metrics.New(1000), nil)
	return engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), m, quiet)
}

func newServer(t *testing.T, doc string, mod ...func(*Options)) *Server {
	t.Helper()
	opts := Options{Engine: newEngine(t, doc), Token: testToken, Logger: quiet}
	for _, f := range mod {
		f(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func do(t *testing.T, s *Server, method, target, ctype, body string, token ...string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	tok := testToken
	if len(token) > 0 {
		tok = token[0]
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return resp{w.Code, w.Header(), w.Body.Bytes()}
}

func want(t *testing.T, r resp, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d: %s", r.code, code, r.body)
	}
}

// chat sends one OpenAI chat request through the engine.
func chat(t *testing.T, e *engine.Engine, headers map[string]string) *core.MemWriter {
	t.Helper()
	drv, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kind driver.Kind
	for _, rt := range drv.Routes() {
		if rt.Path == "/v1/chat/completions" {
			kind = rt.Kind
		}
	}
	h := driver.MapHeaders{"authorization": "Bearer wwz-key-01"}
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	w := core.NewMemWriter()
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: "/v1/chat/completions", Headers: h, Body: []byte(`{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`), ClientIP: "10.1.2.3"}, w)
	return w
}

func TestNewRequiresEngine(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error without engine")
	}
}

func TestAdminRequiresBearerToken(t *testing.T) {
	s := newServer(t, fastCfg)
	r := do(t, s, "GET", "/admin/config", "", "", "")
	want(t, r, 401)
	if r.hdr.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate: %q", r.hdr.Get("WWW-Authenticate"))
	}
	var p Problem
	r.json(t, &p)
	if p.Error != "unauthorized" || p.Message == "" {
		t.Fatalf("problem: %+v", p)
	}
	want(t, do(t, s, "GET", "/admin/config", "", "", "wrong"), 401)
	want(t, do(t, s, "GET", "/admin/nope", "", "", ""), 401)
	want(t, do(t, s, "GET", "/admin/config", "", ""), 200)
	want(t, do(t, s, "GET", "/admin/stats", "", ""), 200)
	// Scheme is case-insensitive.
	req := httptest.NewRequest("GET", "/admin/config", nil)
	req.Header.Set("Authorization", "bearer "+testToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("lower-case bearer: %d", w.Code)
	}
	req.Header.Set("Authorization", "Basic abc")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("basic auth: %d", w.Code)
	}
	// Open routes.
	for _, p := range []string{"/health", "/health/ready", "/metrics", "/dashboard"} {
		want(t, do(t, s, "GET", p, "", "", ""), 200)
	}
	want(t, do(t, s, "GET", "/nope", "", "", ""), 404)
	if !s.TokenConfigured() {
		t.Fatal("token configured")
	}
}

func TestTokenFromEnvAndConfig(t *testing.T) {
	t.Setenv(TokenEnv, "envtok")
	s := newServer(t, fastCfg, func(o *Options) { o.Token = "" })
	want(t, do(t, s, "GET", "/admin/config", "", "", "envtok"), 200)
	want(t, do(t, s, "GET", "/admin/config", "", ""), 401)

	t.Setenv(TokenEnv, "")
	s = newServer(t, "admin: {token: cfgtok}\n", func(o *Options) { o.Token = "" })
	want(t, do(t, s, "GET", "/admin/config", "", "", "cfgtok"), 200)
}

func TestNoTokenAllowsOnlyLoopback(t *testing.T) {
	t.Setenv(TokenEnv, "")
	s := newServer(t, fastCfg, func(o *Options) { o.Token = "" })
	if s.TokenConfigured() {
		t.Fatal("no token expected")
	}
	for addr, code := range map[string]int{"127.0.0.1:5000": 200, "[::1]:5000": 200, "10.0.0.8:5000": 401, "garbage": 401} {
		req := httptest.NewRequest("GET", "/admin/config", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != code {
			t.Errorf("%s: %d want %d", addr, w.Code, code)
		}
	}
	if addr, restricted := s.ListenAddr(); addr != "127.0.0.1:9000" || !restricted {
		t.Fatalf("listen: %s %v", addr, restricted)
	}
}

func TestListenAddr(t *testing.T) {
	cases := []struct {
		in, tok, out string
		restricted   bool
	}{
		{":9000", "x", ":9000", false},
		{":9000", "", "127.0.0.1:9000", true},
		{"0.0.0.0:9100", "", "127.0.0.1:9100", true},
		{"127.0.0.1:9000", "", "127.0.0.1:9000", true},
		{"localhost:9000", "", "localhost:9000", true},
		{"9000", "", "127.0.0.1:9000", true},
	}
	for _, c := range cases {
		out, r := ListenAddr(c.in, c.tok)
		if out != c.out || r != c.restricted {
			t.Errorf("ListenAddr(%q,%q) = %q,%v", c.in, c.tok, out, r)
		}
	}
}

func TestHealthReadyAndDashboard(t *testing.T) {
	s := newServer(t, "server: {startup_duration: 1h}\n")
	r := do(t, s, "GET", "/health/ready", "", "", "")
	want(t, r, 503)
	if r.hdr.Get("Retry-After") != "3600" {
		t.Fatalf("Retry-After: %q", r.hdr.Get("Retry-After"))
	}
	var h Health
	r.json(t, &h)
	if h.Status != "starting" {
		t.Fatalf("status %q", h.Status)
	}
	r = do(t, s, "GET", "/health", "", "", "")
	want(t, r, 200)
	r.json(t, &h)
	if h.Status != "ok" {
		t.Fatalf("health %q", h.Status)
	}

	s = newServer(t, fastCfg)
	r = do(t, s, "GET", "/health/ready", "", "", "")
	want(t, r, 200)
	r.json(t, &h)
	if h.Status != "ready" {
		t.Fatalf("ready %q", h.Status)
	}
	s.SetReady(false)
	r = do(t, s, "GET", "/health/ready", "", "", "")
	want(t, r, 503)
	if r.hdr.Get("Retry-After") != "0" {
		t.Fatalf("Retry-After: %q", r.hdr.Get("Retry-After"))
	}
	s.SetReady(true)
	want(t, do(t, s, "GET", "/health/ready", "", "", ""), 200)

	r = do(t, s, "GET", "/dashboard", "", "", "")
	if !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/html") || !strings.Contains(string(r.body), "/admin/stats") {
		t.Fatalf("dashboard: %s", r.hdr.Get("Content-Type"))
	}
}

func TestMetricsExposition(t *testing.T) {
	s := newServer(t, fastCfg)
	chat(t, s.eng, nil)
	r := do(t, s, "GET", "/metrics", "", "", "")
	want(t, r, 200)
	if !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/plain; version=0.0.4") || !strings.Contains(string(r.body), "wwz_requests_total{") {
		t.Fatalf("metrics: %s", r.body)
	}
}

func TestGetConfigMasksSecrets(t *testing.T) {
	t.Setenv(TokenEnv, "")
	s := newServer(t, `
admin: {token: secret-admin}
store: {password: secret-store}
keys:
  - {key: sk-real, label: ci}
`, func(o *Options) { o.Token = "" })
	r := do(t, s, "GET", "/admin/config", "", "", "secret-admin")
	want(t, r, 200)
	if strings.Contains(string(r.body), "secret-") || strings.Contains(string(r.body), "sk-real") {
		t.Fatalf("secret leaked: %s", r.body)
	}
	var doc map[string]any
	r.json(t, &doc)
	if doc["admin"].(map[string]any)["token"] != masked || doc["keys"].([]any)[0].(map[string]any)["key"] != masked {
		t.Fatalf("not masked: %v", doc)
	}
	if doc["server"].(map[string]any)["read_timeout"] != "1m0s" {
		t.Fatalf("durations must be Go strings: %v", doc["server"])
	}
	// Sending the masked values back keeps the real secrets.
	r = do(t, s, "POST", "/admin/config", "application/json", `{"admin":{"token":"***"},"store":{"password":"***"},"keys":{"ci":{"key":"***","tier":"default"}}}`, "secret-admin")
	want(t, r, 200)
	c := s.Config()
	if c.Admin.Token != "secret-admin" || c.Store.Password != "secret-store" || c.FindKey("sk-real") == nil {
		t.Fatalf("secrets changed: %+v %+v", c.Admin, c.Keys)
	}
}

func TestPatchByName(t *testing.T) {
	s := newServer(t, fastCfg)
	var seen atomic.Int32
	s.OnReload(func(c *config.Config) {
		if c != s.eng.Config() {
			t.Error("observer must see the effective config")
		}
		seen.Add(1)
	})
	before := s.Config()
	r := do(t, s, "POST", "/admin/config", "application/merge-patch+json", `{
		"models": {"wwz-fast": {"errors": {"rate": 30}}, "new-model": {"latency": "fast"}, "wwz-flaky": null},
		"keys": {"test-01": {"status": "suspended"}, "extra": {"key": "k-extra"}},
		"drivers": {"anthropic": {"prefix": "/anthropic"}},
		"server": {"max_rps": 77, "reuseport": true},
		"vars": {"a": "b"}
	}`)
	want(t, r, 200)
	var applied struct {
		Applied bool           `json:"applied"`
		Source  string         `json:"source"`
		Config  map[string]any `json:"config"`
	}
	r.json(t, &applied)
	if !applied.Applied || applied.Source != "patch" || applied.Config["server"] == nil {
		t.Fatalf("applied: %+v", applied)
	}
	c := s.Config()
	if c == before {
		t.Fatal("config not swapped")
	}
	if seen.Load() != 1 {
		t.Fatalf("observer calls: %d", seen.Load())
	}
	m := c.FindModel("wwz-fast")
	ep, err := c.ErrorsFor(m)
	if err != nil || ep.Rate != 30 {
		t.Fatalf("errors rate: %+v %v", ep, err)
	}
	if c.FindModel("new-model") == nil || c.FindModel("wwz-flaky") != nil {
		t.Fatalf("models: %+v", c.Models)
	}
	if c.Models[0].ID != "wwz-fast" || c.Models[1].ID != "wwz-realistic" || c.Models[2].ID != "new-model" {
		t.Fatalf("order: %v %v %v", c.Models[0].ID, c.Models[1].ID, c.Models[2].ID)
	}
	if k := c.FindKey("wwz-key-01"); k == nil || k.Status != "suspended" {
		t.Fatalf("key: %+v", k)
	}
	if k := c.FindKey("k-extra"); k == nil || k.Label != "extra" {
		t.Fatalf("new key: %+v", k)
	}
	if c.Drivers[1].Name != "anthropic" || c.Drivers[1].Prefix != "/anthropic" {
		t.Fatalf("drivers: %+v", c.Drivers)
	}
	if c.Server.MaxRPS != 77 || !c.Server.Reuseport || c.Vars["a"] != "b" {
		t.Fatalf("server: %+v", c.Server)
	}
	// The mounted driver is now addressed by its prefix; new drivers by name or prefix.
	want(t, do(t, s, "POST", "/admin/config", "application/json", `{"drivers": {"/anthropic": {"options": {"require_version": "true"}}, "/or": {"name": "openrouter"}, "opencode": null}}`), 200)
	c = s.Config()
	if c.Drivers[1].Options["require_version"] != "true" || len(c.Drivers) != 4 || c.Drivers[3].Prefix != "/or" {
		t.Fatalf("drivers by mount: %+v", c.Drivers)
	}
	// A list sent as an array replaces the whole list; null removes a key.
	want(t, do(t, s, "POST", "/admin/config", "", `{"models": [{"id": "only"}], "vars": null}`), 200)
	c = s.Config()
	if len(c.Models) != 1 || c.Models[0].ID != "only" || c.Vars != nil {
		t.Fatalf("replace: %+v %v", c.Models, c.Vars)
	}
	// Floats stay floats.
	want(t, do(t, s, "POST", "/admin/config", "", `{"store": {"event_sample": 0.25}}`), 200)
	if s.Config().Store.EventSample != 0.25 {
		t.Fatalf("event_sample: %v", s.Config().Store.EventSample)
	}
}

func TestInvalidPatchKeepsCurrentConfig(t *testing.T) {
	s := newServer(t, fastCfg, func(o *Options) {
		o.Check = func(c *config.Config) error {
			if c.Server.MaxRPS == 13 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
	})
	var calls atomic.Int32
	s.OnReload(func(*config.Config) { calls.Add(1) })
	before := s.Config()
	bodies := []string{
		`{"models": {"wwz-fast": {"latency": "nope"}}}`,
		`{"server": {"auth": "bogus"}}`,
		`{"server": {"unknown_field": 1}}`,
		`{"models": {"wwz-fast": 3}}`,
		`{"server": {"max_rps": 13}}`,
		`[1,2]`,
		`{"a":1} {"b":2}`,
		`{not json`,
	}
	for _, b := range bodies {
		r := do(t, s, "POST", "/admin/config", "application/json", b)
		want(t, r, 400)
		var p Problem
		r.json(t, &p)
		if p.Error != "invalid" || len(p.Details) == 0 {
			t.Fatalf("problem for %s: %+v", b, p)
		}
		if s.Config() != before {
			t.Fatalf("config replaced by invalid patch %s", b)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("observers called on failure")
	}
	want(t, do(t, s, "POST", "/admin/config", "text/plain", `{"server":{}}`), 415)
	// Duplicate labels cannot be addressed by name.
	s = newServer(t, "keys:\n  - {key: a, label: dup}\n  - {key: b, label: dup}\n")
	want(t, do(t, s, "POST", "/admin/config", "", `{"keys": {"dup": {"tier": "x"}}}`), 400)
	want(t, do(t, s, "POST", "/admin/config", "", `{"server": {"max_rps": 5}}`), 200)
}

func TestEmptyPostReloadsFileAndSIGHUP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wwz.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("models:\n  - id: from-file\n")
	var overridden atomic.Int32
	s := newServer(t, fastCfg, func(o *Options) {
		o.ConfigPath = path
		o.Override = func(c *config.Config) { c.Server.Seed = 99; overridden.Add(1) }
	})
	reloaded := make(chan *config.Config, 4)
	s.OnReload(func(c *config.Config) { reloaded <- c })

	r := do(t, s, "POST", "/admin/config", "", "")
	want(t, r, 200)
	var applied ConfigApplied
	r.json(t, &applied)
	if applied.Source != "file" || s.Config().FindModel("from-file") == nil || s.Config().Server.Seed != 99 {
		t.Fatalf("reload: %+v", applied.Source)
	}
	<-reloaded

	before := s.Config()
	write("models:\n  - id: x\n    latency: missing-profile\n")
	want(t, do(t, s, "POST", "/admin/config", "", "  \n"), 400)
	if s.Config() != before {
		t.Fatal("invalid file replaced the configuration")
	}

	// SIGHUP re-reads the file.
	write("models:\n  - id: via-sighup\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.WatchSIGHUP(ctx)
	time.Sleep(20 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-reloaded:
		if c.FindModel("via-sighup") == nil {
			t.Fatalf("sighup config: %+v", c.Models)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SIGHUP did not reload")
	}
	// A failing SIGHUP reload keeps the configuration.
	write("server: {auth: bogus}\n")
	before = s.Config()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if s.Config() != before {
		t.Fatal("failed SIGHUP reload replaced the configuration")
	}

	// Without a file there is nothing to reload.
	s = newServer(t, fastCfg)
	want(t, do(t, s, "POST", "/admin/config", "", ""), 400)
	if _, err := s.Reload(); err == nil {
		t.Fatal("reload without file must fail")
	}
	// Custom loader.
	s = newServer(t, fastCfg, func(o *Options) {
		o.Load = func() (*config.Config, error) { return config.Parse([]byte("models:\n  - id: loaded\n")) }
	})
	if c, err := s.Reload(); err != nil || c.FindModel("loaded") == nil {
		t.Fatalf("custom load: %v", err)
	}
	if err := s.Apply(nil); err == nil {
		t.Fatal("Apply(nil) must fail")
	}
}
