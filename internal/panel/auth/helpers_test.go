package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

var ctx = context.Background()

// fastHasher keeps the tests quick; Verify reads the parameters from the
// hash, so production parameters are exercised in hash_test.go.
var fastHasher = Argon2Hasher{Memory: 64, Time: 1, Threads: 1}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// failRand makes randRead fail from call number n (0-based) on.
func failRand(t *testing.T, n int) {
	t.Helper()
	orig := randRead
	calls := 0
	randRead = func(b []byte) (int, error) {
		calls++
		if calls > n {
			return 0, errors.New("sem entropia")
		}
		return orig(b)
	}
	t.Cleanup(func() { randRead = orig })
}

type env struct {
	t    *testing.T
	st   *store.Store
	path string
	clk  *clock
	cfg  *config.Config
	svc  *Service
	logs *bytes.Buffer
	opts Options
}

func baseConfig() *config.Config {
	return &config.Config{
		PublicURL: "http://painel.test",
		Session:   config.Session{TTL: time.Hour},
		APIToken:  "tok-123",
		OIDC:      config.OIDC{UsernameClaim: "email", RoleClaim: "groups", DefaultRole: config.RoleViewer},
	}
}

func newEnv(t *testing.T, mut func(*config.Config, *Options)) *env {
	t.Helper()
	e := &env{t: t, clk: &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}, cfg: baseConfig(), logs: &bytes.Buffer{}}
	e.path = filepath.Join(t.TempDir(), "panel.db")
	st, err := store.Open(ctx, e.path, store.WithClock(e.clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.opts = Options{
		Store: st, Config: e.cfg, Now: e.clk.now, Hasher: fastHasher,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
	}
	if mut != nil {
		mut(e.cfg, &e.opts)
	}
	svc, err := New(ctx, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

// sqlExec runs statements on a separate connection to the panel database
// (triggers that inject failures, direct updates).
func (e *env) sqlExec(q string) {
	e.t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+e.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(q); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user(name, role, password string) store.User {
	e.t.Helper()
	h, err := fastHasher.Hash(password)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := e.st.Users.Create(ctx, store.User{Name: name, Role: role, Source: store.SourceYAML, PasswordHash: h})
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func jsonReq(method, target string, body any) *http.Request {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func formReq(target string, v url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func cookieOf(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %s ausente (status %d, corpo %s)", name, rec.Code, rec.Body)
	return nil
}

// login does a JSON login and returns the session cookie and Me.
func (e *env) login(name, password string) (*http.Cookie, Me) {
	e.t.Helper()
	rec := serve(e.svc.LoginHandler(), jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{name, password}))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body)
	}
	var me Me
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		e.t.Fatal(err)
	}
	return cookieOf(e.t, rec, SessionCookie), me
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) Problem {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("corpo não é Problem: %s", rec.Body)
	}
	return p
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, status, rec.Body)
	}
	if p := problemOf(t, rec); p.Error != code {
		t.Fatalf("error = %q, want %q", p.Error, code)
	}
}

// ok is a handler that echoes the identity of the context.
var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	id, found := FromContext(r.Context())
	if !found {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	writeJSON(w, http.StatusOK, id.Me())
})

type httpRec = httptest.ResponseRecorder

func newRec() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func httpGet(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }

func decodeJSON(res *http.Response, v any) error { return json.NewDecoder(res.Body).Decode(v) }

func httpMultipart(t *testing.T, target string, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}
