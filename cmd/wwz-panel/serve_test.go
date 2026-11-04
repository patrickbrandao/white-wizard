package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// syncBuf is a bytes.Buffer safe for the logger and the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeWWZ is a wwz admin answering /health, /admin/requests and
// /admin/stats.
func fakeWWZ(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/admin/requests":
			_, _ = io.WriteString(w, `{"enabled":true,"requests":[]}`)
		case "/admin/stats":
			_, _ = io.WriteString(w, `{"pod":"wwz-1","store":"memory","totals":{"in_flight":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// panelEnv sets the variables of a panel with a fresh SQLite and returns
// the database path.
func panelEnv(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "panel.db")
	t.Setenv("WWZ_PANEL_DB", db)
	t.Setenv("WWZ_PANEL_LISTEN", "127.0.0.1:0")
	t.Setenv("WWZ_PANEL_WWZ_ADMIN_URL", fakeWWZ(t))
	t.Setenv("WWZ_PANEL_ADMIN_USER", "chefe")
	t.Setenv("WWZ_PANEL_ADMIN_PASSWORD", "senha-forte")
	return db
}

type served struct {
	code   int
	logs   *syncBuf
	cancel context.CancelFunc
	done   chan struct{}
	addr   string
}

// startServe runs `serve` in background until stop.
func startServe(t *testing.T, args []string, onReady func(net.Listener)) *served {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{logs: &syncBuf{}, cancel: cancel, done: make(chan struct{})}
	ready := make(chan string, 1)
	go func() {
		defer close(s.done)
		s.code = runServe(ctx, args, io.Discard, s.logs, func(ln net.Listener) {
			ready <- ln.Addr().String()
			if onReady != nil {
				onReady(ln)
			}
		})
		close(ready)
	}()
	addr, ok := <-ready
	if !ok {
		t.Fatalf("serve não subiu: %d %s", s.code, s.logs)
	}
	s.addr = addr
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *served) stop() int {
	s.cancel()
	<-s.done
	return s.code
}

func TestServeLifecycle(t *testing.T) {
	db := panelEnv(t)
	orig := purgeEvery
	purgeEvery = 20 * time.Millisecond
	t.Cleanup(func() { purgeEvery = orig })
	s := startServe(t, nil, nil)
	base := "http://" + s.addr

	resp, err := http.Get(base + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	var rd struct {
		Status  string            `json:"status"`
		Sources map[string]string `json:"sources"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rd)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || rd.Status != "ready" || rd.Sources["admin"] != "ok" || rd.Sources["valkey"] != "off" {
		t.Fatalf("ready: %d %+v", resp.StatusCode, rd)
	}
	// The bootstrap operator logs in.
	resp, err = http.Post(base+"/api/v1/login", "application/json", strings.NewReader(`{"username":"chefe","password":"senha-forte"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login do operador inicial: %d", resp.StatusCode)
	}
	if resp, err := http.Get(base + "/api/v1/overview"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("sem sessão: %v %v", err, resp)
	}
	checkScreens(t, base)
	time.Sleep(50 * time.Millisecond) // a few purge ticks
	if code := s.stop(); code != exitOK {
		t.Fatalf("saída: %d %s", code, s.logs)
	}
	logs := s.logs.String()
	for _, want := range []string{`"msg":"painel: no ar"`, `"msg":"painel: purga"`, `"msg":"painel: encerrado"`, `"msg":"painel: operador inicial criado"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log sem %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "senha-forte") {
		t.Fatal("senha dada no log")
	}

	// Second boot on the same file: bootstrap ignored, generated password
	// never logged; with Valkey configured (down) the consumer runs.
	t.Setenv("WWZ_PANEL_ADMIN_PASSWORD", "")
	t.Setenv("WWZ_PANEL_VALKEY_ADDR", "127.0.0.1:1")
	s = startServe(t, []string{"-listen", "127.0.0.1:0"}, nil)
	if code := s.stop(); code != exitOK || strings.Contains(s.logs.String(), "senha gerada") {
		t.Fatalf("segundo boot: %d %s", code, s.logs)
	}
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if n, _ := st.Users.Count(context.Background()); n != 1 {
		t.Fatalf("usuários: %d", n)
	}
}

func TestServeViaRun(t *testing.T) {
	panelEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	var errb syncBuf
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve"}, strings.NewReader(""), io.Discard, &errb) }()
	for i := 0; i < 500 && !strings.Contains(errb.String(), "no ar"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("run serve: %d %s", code, errb.String())
	}
}

// TestBinarySIGTERM runs the real main() in a child process: SIGTERM shuts
// the panel down gracefully with exit 0; version exits 0.
func TestBinarySIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sem SIGTERM no Windows")
	}
	panelEnv(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-executes the test binary itself
	cmd.Env = append(os.Environ(), "WWZ_PANEL_TEST_MAIN=1", "WWZ_PANEL_TEST_ARGS=serve")
	var logs syncBuf
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && !strings.Contains(logs.String(), "no ar"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || !strings.Contains(logs.String(), "painel: encerrado") {
			t.Fatalf("saída: %v %s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("não saiu depois do SIGTERM")
	}
	cmd = exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: idem
	cmd.Env = append(os.Environ(), "WWZ_PANEL_TEST_MAIN=1", "WWZ_PANEL_TEST_ARGS=dance")
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != exitUsage {
		t.Fatalf("comando desconhecido: %v", err)
	}
}

func TestServeFailures(t *testing.T) {
	ctx := context.Background()
	var errb bytes.Buffer
	if code := runServe(ctx, []string{"-h"}, io.Discard, &errb, nil); code != exitOK {
		t.Fatalf("-h: %d", code)
	}
	if code := runServe(ctx, []string{"-nope"}, io.Discard, &errb, nil); code != exitUsage {
		t.Fatalf("flag ruim: %d", code)
	}
	if code := runServe(ctx, []string{"extra"}, io.Discard, &errb, nil); code != exitUsage {
		t.Fatalf("argumento: %d", code)
	}
	db := panelEnv(t)
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "x")
	if code := runServe(ctx, nil, io.Discard, &errb, nil); code != exitError {
		t.Fatalf("config inválida: %d", code)
	}
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "")
	t.Setenv("WWZ_PANEL_DB", filepath.Join(t.TempDir(), "nao", "existe", "panel.db"))
	if code := runServe(ctx, nil, io.Discard, &errb, nil); code != exitError {
		t.Fatalf("banco inacessível: %d", code)
	}
	// Lock taken, but the file is not SQLite: the store does not open.
	garbage := filepath.Join(t.TempDir(), "panel.db")
	_ = os.WriteFile(garbage, []byte("não é sqlite, não é sqlite, não é sqlite"), 0o600)
	t.Setenv("WWZ_PANEL_DB", garbage)
	if code := runServe(ctx, nil, io.Discard, &errb, nil); code != exitError {
		t.Fatalf("banco inválido: %d", code)
	}
	// Invalid session key in settings: auth fails after the store opened.
	t.Setenv("WWZ_PANEL_DB", db)
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Settings.Set(ctx, store.SettingSessionKey, "curta")
	_ = st.Close()
	var logs syncBuf
	if code := runServe(ctx, nil, io.Discard, &logs, nil); code != exitError || !strings.Contains(logs.String(), "não subiu") {
		t.Fatalf("chave de sessão inválida: %d %s", code, logs.String())
	}
	// Address in use.
	panelEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if code := runServe(ctx, []string{"-listen", ln.Addr().String()}, io.Discard, &errb, nil); code != exitError {
		t.Fatalf("porta ocupada: %d", code)
	}
}

func TestServeStops(t *testing.T) {
	// The HTTP server dies (listener closed): exit 1.
	panelEnv(t)
	s := startServe(t, nil, func(ln net.Listener) { _ = ln.Close() })
	<-s.done
	if s.code != exitError || !strings.Contains(s.logs.String(), "servidor HTTP parou") {
		t.Fatalf("servidor parado: %d %s", s.code, s.logs)
	}

	// Shutdown timing out on a connection that never sends a request.
	orig := shutdownTimeout
	shutdownTimeout = 10 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = orig })
	s = startServe(t, nil, nil)
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(20 * time.Millisecond)
	if code := s.stop(); code != exitOK || !strings.Contains(s.logs.String(), "painel: encerramento") {
		t.Fatalf("shutdown com conexão presa: %d %s", code, s.logs)
	}
}

func TestPurgeErrors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	var logs syncBuf
	p := &panel{cfg: &config.Config{RetentionDays: 7}, log: slog.New(slog.NewJSONHandler(&logs, nil)), st: st}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	p.purge(cctx)
	if logs.String() != "" {
		t.Fatalf("purga cancelada logou: %s", logs.String())
	}
	p.purge(ctx)
	if !strings.Contains(logs.String(), `"level":"WARN","msg":"painel: purga"`) {
		t.Fatalf("purga falhando: %s", logs.String())
	}
}

// checkScreens walks the HTML side mounted by serve (task 050): security
// headers, the login page, the form login of the bootstrap operator, the
// overview and a static asset with long cache.
func checkScreens(t *testing.T, base string) {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(c *http.Client, path string, cookies ...*http.Cookie) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b)
	}
	resp, _ := get(noRedirect, "/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("tela sem sessão: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body := get(http.DefaultClient, "/login")
	if resp.StatusCode != 200 || resp.Header.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		!strings.Contains(body, `action="/api/v1/login"`) {
		t.Fatalf("login: %d %v", resp.StatusCode, resp.Header)
	}
	i := strings.Index(body, `src="/static/htmx.min.`)
	if i < 0 {
		t.Fatalf("login sem htmx: %s", body)
	}
	asset := body[i+len(`src="`):]
	asset = asset[:strings.IndexByte(asset, '"')]
	if resp, _ := get(http.DefaultClient, asset); resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("static %s: %d %v", asset, resp.StatusCode, resp.Header)
	}
	resp, err := noRedirect.PostForm(base+"/api/v1/login", url.Values{"username": {"chefe"}, "password": {"senha-forte"}, "next": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("login por formulário: %d", resp.StatusCode)
	}
	resp, body = get(http.DefaultClient, "/", resp.Cookies()...)
	if resp.StatusCode != 200 || !strings.Contains(body, "chefe (operator)") || !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Fatalf("visão geral: %d %s", resp.StatusCode, body)
	}
}
