package api

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
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/admin"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
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

// wwzCfg is the configuration of the in-process wwz (admin API).
const wwzCfg = `
server: {seed: 42}
admin: {debug: true}
keys:
  - {key: sk-test-01, label: test-01, account: acme, tier: gold}
  - {key: sk-test-02, label: test-02, limits: {rpm: 7}}
tiers:
  default: {limits: {rpm: 100}}
  gold: {limits: {rpm: 1000, tpm: 50000}}
`

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// wwz is a wwz control plane running in process (real internal/admin over
// a memory store).
type wwz struct {
	eng *engine.Engine
	srv *httptest.Server
}

func newWWZ(t *testing.T, st limits.Store) *wwz {
	t.Helper()
	cfg, err := config.Parse([]byte(wwzCfg))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.DiscardHandler)
	if st == nil {
		st = limits.NewMemory()
	}
	eng := engine.New(cfg, st, scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), quiet)
	// Cluster policies wired as in cmd/wwz (ADR 0025): cfg:policies goes on
	// top of every configuration and changes are watched.
	pol := &limits.PolicyApplier{Log: quiet}
	s, err := admin.New(admin.Options{Engine: eng, Token: adminToken, Logger: quiet, Override: pol.Override})
	if err != nil {
		t.Fatal(err)
	}
	s.OnReload(pol.Committed)
	pol.Current, pol.Apply = s.Config, s.Apply
	ps := st.(limits.PolicySource) //nolint:forcetypeassert // memory and valkey stores
	p, perr := ps.Policies(ctx)
	pol.Offer(p, perr)
	ps.WatchPolicies(pol.Offer)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &wwz{eng: eng, srv: srv}
}

type envOpts struct {
	adminURL string       // "" = in-process wwz
	wwzStore limits.Store // store of the in-process wwz (nil = memory)
	prom     http.Handler
	valkey   *source.Valkey
	consumer bool
	poller   bool
}

type env struct {
	t     *testing.T
	st    *store.Store
	auth  *auth.Service
	api   *API
	h     http.Handler
	wwz   *wwz
	clk   *clock
	logs  *bytes.Buffer
	admin *source.Admin
}

func newEnv(t *testing.T, mods ...func(*envOpts)) *env {
	t.Helper()
	var o envOpts
	for _, m := range mods {
		m(&o)
	}
	e := &env{t: t, clk: &clock{t: time.Now().UTC().Truncate(time.Millisecond)}, logs: &bytes.Buffer{}}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "panel.db"), store.WithClock(e.clk.now))
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
	e.auth, err = auth.New(ctx, auth.Options{Store: st, Config: cfg, Logger: log, Now: e.clk.now, Hasher: fastHasher})
	if err != nil {
		t.Fatal(err)
	}
	if o.adminURL == "" {
		e.wwz = newWWZ(t, o.wwzStore)
		o.adminURL = e.wwz.srv.URL
	}
	e.admin = source.NewAdmin(o.adminURL, adminToken, &http.Client{Timeout: 2 * time.Second})
	opts := Options{Store: st, Auth: e.auth, Admin: e.admin, Valkey: o.valkey, Logger: log, Now: e.clk.now, PingTimeout: time.Second}
	if o.prom != nil {
		ps := httptest.NewServer(o.prom)
		t.Cleanup(ps.Close)
		opts.Prom = source.NewProm(ps.URL, nil)
	}
	if o.consumer && o.valkey != nil {
		opts.Consumer = source.NewConsumer(o.valkey, st.Events, source.ConsumerOptions{})
	}
	if o.poller {
		opts.Poller = source.NewRequestPoller(e.admin, st.Events, source.PollerOptions{})
	}
	e.api, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.h = e.api.Handler()
	return e
}

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %d %s: %v", r.code, r.body, err)
	}
}

func (r resp) problem(t *testing.T) auth.Problem {
	t.Helper()
	var p auth.Problem
	r.decode(t, &p)
	return p
}

// cred is how a request authenticates.
type cred struct {
	bearer string
	cookie *http.Cookie
	csrf   string
}

var operator = cred{bearer: apiToken}

func (e *env) do(method, target, body string, c cred) resp {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "192.0.2.7:4321"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.csrf != "" {
		req.Header.Set(auth.CSRFHeader, c.csrf)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return resp{code: w.Code, hdr: w.Header(), body: w.Body.Bytes()}
}

// login opens a session for name and returns the cookie and CSRF token.
func (e *env) login(name, pw string) cred {
	e.t.Helper()
	r := e.do("POST", "/api/v1/login", `{"username":"`+name+`","password":"`+pw+`"}`, cred{})
	if r.code != http.StatusOK {
		e.t.Fatalf("login %s: %d %s", name, r.code, r.body)
	}
	var me auth.Me
	r.decode(e.t, &me)
	for _, c := range (&http.Response{Header: r.hdr}).Cookies() {
		if c.Name == auth.SessionCookie {
			return cred{cookie: c, csrf: me.CSRF}
		}
	}
	e.t.Fatal("login sem cookie")
	return cred{}
}

// insertEvents stores events in the panel SQLite.
func (e *env) insertEvents(evs ...store.Event) {
	e.t.Helper()
	if _, err := e.st.Events.InsertBatch(ctx, evs); err != nil {
		e.t.Fatal(err)
	}
}

// promServer answers /api/v1/query and /api/v1/query_range with the JSON of
// the first rule whose substring is in the query (none = empty vector or
// matrix).
type promRule struct {
	match string
	body  string
	rng   bool // applies to query_range (false = query)
}

func promHandler(rules ...promRule) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		isRange := strings.HasSuffix(r.URL.Path, "query_range")
		for _, rule := range rules {
			if rule.rng == isRange && strings.Contains(q, rule.match) {
				_, _ = io.WriteString(w, rule.body)
				return
			}
		}
		if isRange {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	})
}

func vector(samples ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
}

func sample(labels string, v string) string {
	return `{"metric":{` + labels + `},"value":[1700000000,"` + v + `"]}`
}

func matrix(series ...string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(series, ",") + `]}}`
}

func q(v ...string) string {
	u := url.Values{}
	for i := 0; i+1 < len(v); i += 2 {
		u.Set(v[i], v[i+1])
	}
	return u.Encode()
}

// deadURL is an address nobody listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	return s.URL
}

// failingAdmin answers every call with status and a Problem.
func failingAdmin(t *testing.T, status int) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"x","message":"falhou","details":["d1"]}`)
	}))
	t.Cleanup(s.Close)
	return s.URL
}
