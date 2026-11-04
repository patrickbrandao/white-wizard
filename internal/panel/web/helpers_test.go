package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/admin"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/panel/api"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

var ctx = context.Background()

var fastHasher = auth.Argon2Hasher{Memory: 64, Time: 1, Threads: 1}

const (
	adminToken = "adm-tok"
	apiToken   = "api-tok"
	opPass     = "op-pass"
	viPass     = "vi-pass"
)

const wwzCfg = `
server: {seed: 42}
admin: {debug: true}
keys:
  - {key: sk-test-01, label: test-01, account: acme, tier: gold}
tiers:
  default: {limits: {rpm: 100}}
  gold: {limits: {rpm: 1000, rpd: 5000}}
`

// newWWZ runs a real wwz control plane (internal/admin over a memory store).
func newWWZ(t *testing.T) string {
	t.Helper()
	cfg, err := config.Parse([]byte(wwzCfg))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.DiscardHandler)
	eng := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), quiet)
	s, err := admin.New(admin.Options{Engine: eng, Token: adminToken, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

type envOpts struct {
	adminURL string // "" = in-process wwz
	prom     http.Handler
	oidc     bool
	valkey   *source.Valkey
}

type env struct {
	t     *testing.T
	st    *store.Store
	h     http.Handler
	logs  *bytes.Buffer
	admin *source.Admin
}

func newEnv(t *testing.T, mods ...func(*envOpts)) *env {
	t.Helper()
	var o envOpts
	for _, m := range mods {
		m(&o)
	}
	e := &env{t: t, logs: &bytes.Buffer{}}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	for _, u := range []struct{ name, role, pw string }{{"op", store.RoleOperator, opPass}, {"vi", store.RoleViewer, viPass}} {
		h, _ := fastHasher.Hash(u.pw)
		if _, err := st.Users.Create(ctx, store.User{Name: u.name, Role: u.role, Source: store.SourceYAML, PasswordHash: h}); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &panelcfg.Config{PublicURL: "http://painel.test", APIToken: apiToken, Session: panelcfg.Session{TTL: time.Hour}}
	au, err := auth.New(ctx, auth.Options{Store: st, Config: cfg, Logger: log, Hasher: fastHasher})
	if err != nil {
		t.Fatal(err)
	}
	if o.adminURL == "" {
		o.adminURL = newWWZ(t)
	}
	e.admin = source.NewAdmin(o.adminURL, adminToken, &http.Client{Timeout: 2 * time.Second})
	opts := api.Options{Store: st, Auth: au, Admin: e.admin, Valkey: o.valkey, Logger: log, PingTimeout: time.Second}
	if o.prom != nil {
		ps := httptest.NewServer(o.prom)
		t.Cleanup(ps.Close)
		opts.Prom = source.NewProm(ps.URL, nil)
	}
	a, err := api.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	New(Options{API: a, Auth: au, OIDC: o.oidc, Logger: log}).Register(mux)
	e.h = SecurityHeaders(mux)
	return e
}

type resp struct {
	code int
	hdr  http.Header
	body string
}

// cred is how a request authenticates.
type cred struct {
	bearer string
	cookie *http.Cookie
	csrf   string
}

type reqOpt func(*http.Request)

// hx marks the request as made by HTMX.
func hx(r *http.Request) { r.Header.Set("HX-Request", "true") }

func (e *env) do(method, target string, form url.Values, c cred, opts ...reqOpt) resp {
	e.t.Helper()
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "192.0.2.7:4321"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return resp{code: w.Code, hdr: w.Header(), body: w.Body.String()}
}

// login posts the login form and returns the session cookie and its CSRF
// token (read from /api/v1/me).
func (e *env) login(name, pw string) cred {
	e.t.Helper()
	r := e.do("POST", "/api/v1/login", url.Values{"username": {name}, "password": {pw}, "next": {"/usage"}}, cred{})
	if r.code != http.StatusSeeOther || r.hdr.Get("Location") != "/usage" {
		e.t.Fatalf("login %s: %d %v", name, r.code, r.hdr)
	}
	var c cred
	for _, ck := range (&http.Response{Header: r.hdr}).Cookies() {
		if ck.Name == auth.SessionCookie {
			c.cookie = ck
		}
	}
	me := e.do("GET", "/api/v1/me", nil, c)
	var m auth.Me
	if err := json.Unmarshal([]byte(me.body), &m); err != nil || m.CSRF == "" {
		e.t.Fatalf("me: %d %s", me.code, me.body)
	}
	c.csrf = m.CSRF
	return c
}

// has fails when body lacks any of the fragments (golden of the essential
// HTML).
func has(t *testing.T, what, body string, frags ...string) {
	t.Helper()
	for _, f := range frags {
		if !strings.Contains(body, f) {
			t.Errorf("%s: falta %q em:\n%s", what, f, body)
		}
	}
}

func hasNot(t *testing.T, what, body string, frags ...string) {
	t.Helper()
	for _, f := range frags {
		if strings.Contains(body, f) {
			t.Errorf("%s: não devia ter %q em:\n%s", what, f, body)
		}
	}
}

// deadURL is an address nobody listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	return s.URL
}

// promHandler answers PromQL queries by the first matching substring.
func promHandler(rules map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		for _, m := range []string{"status=~", "wwz_requests_total", "wwz_tokens_total", "wwz_requests_in_flight", "wwz_ttft_seconds_bucket", "wwz_e2e_seconds_bucket", "wwz_errors_injected_total", "wwz_rate_limited_total"} {
			if body, ok := rules[m]; ok && strings.Contains(q, m) {
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`+body+`]}}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	})
}

func sample(labels, v string) string {
	return `{"metric":{` + labels + `},"value":[1700000000,"` + v + `"]}`
}
