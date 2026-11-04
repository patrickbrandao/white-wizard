package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/patrickbrandao/white-wizard/internal/admin"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/top"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

const topToken = "top-secret"

// startWWZAdmin serves a data plane and its control plane over one engine and
// returns both base URLs.
func startWWZAdmin(t *testing.T) (dataURL, adminURL string) {
	t.Helper()
	c, err := config.Parse([]byte(wwzCfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	eng := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), log)
	s, err := server.New(eng, server.Options{Logger: log})
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
	})
	a, err := admin.New(admin.Options{Engine: eng, Token: topToken, Logger: log, PodName: "wwz-test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return "http://" + ln.Addr().String(), srv.URL
}

func chat(t *testing.T, dataURL string, n int) {
	t.Helper()
	for range n {
		body := `{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}]}`
		req, _ := http.NewRequest(http.MethodPost, dataURL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer wwz-key-01")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// TestTopOnceRealAdmin: --once --output json against a real control plane;
// the same instance given twice sums its counters.
func TestTopOnceRealAdmin(t *testing.T) {
	dataURL, adminURL := startWWZAdmin(t)
	chat(t, dataURL, 3)
	t.Setenv("WWZ_ADMIN_TOKEN", topToken)
	code, out, errOut := runArgs(t, context.Background(), "top", "--once", "--output", "json", "--interval", "100ms", "--admin", adminURL, "--admin", adminURL+"/")
	if code != exitOK {
		t.Fatalf("%d %s", code, errOut)
	}
	var f top.Frame
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatal(err, out)
	}
	if f.Kind != "top" || f.Version != 1 || f.IntervalMS != 100 || len(f.Instances) != 2 || !f.Instances[0].OK || f.Instances[0].Pod != "wwz-test" {
		t.Fatalf("%+v", f)
	}
	if f.Totals.Requests != 6 || f.Totals.RPS == nil || *f.Totals.RPS != 0 || len(f.ByModel) != 1 || f.ByModel[0].Model != "wwz-fast" || f.ByModel[0].Requests != 6 {
		t.Fatalf("%s", out)
	}
	// Text frame.
	code, out, _ = runArgs(t, context.Background(), "top", "--once", "--interval", "100ms", "--admin", adminURL, "--sort", "e2e", "--rows", "1")
	if code != exitOK || !strings.Contains(out, "pod wwz-test") || !strings.Contains(out, "(sort: e2e)") || strings.Contains(out, "\x1b") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestTopTokenRefused(t *testing.T) {
	_, adminURL := startWWZAdmin(t)
	code, _, errOut := runArgs(t, context.Background(), "top", "--once", "--interval", "100ms", "--admin", adminURL, "--token", "wrong")
	if code != exitError || !strings.Contains(errOut, "token recusado por "+adminURL) {
		t.Fatalf("%d %q", code, errOut)
	}
	t.Setenv("WWZ_ADMIN_TOKEN", topToken)
	code, _, errOut = runArgs(t, context.Background(), "top", "--once", "--interval", "100ms", "--admin", adminURL, "--token", "")
	if code != exitError || !strings.Contains(errOut, "token recusado") {
		t.Fatalf("explicit empty token must win over the env: %d %q", code, errOut)
	}
}

func TestTopNoInstance(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	code, _, errOut := runArgs(t, context.Background(), "top", "--once", "--interval", "100ms", "--admin", dead.URL, "--timeout", "500ms")
	if code != exitError || !strings.Contains(errOut, "nenhuma instância respondeu") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestTopUsage(t *testing.T) {
	for _, args := range [][]string{
		{"extra"},
		{"--interval", "50ms"},
		{"--timeout", "0s"},
		{"--output", "csv", "--once"},
		{"--output", "json"},
		{"--sort", "name"},
		{"--rows", "0"},
		{"--admin", "ftp://x"},
		{"--admin", "http://"},
		{"--bogus"},
	} {
		code, _, errOut := runArgs(t, context.Background(), append([]string{"top"}, args...)...)
		if code != exitUsage || !strings.Contains(errOut, "wwz-load top: ") {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
	if code, _, errOut := runArgs(t, context.Background(), "top", "-h"); code != exitOK || !strings.Contains(errOut, "-admin") {
		t.Fatalf("-h: %d %q", code, errOut)
	}
	o, err := parseTop(nil, io.Discard)
	if err != nil || len(o.admins) != 1 || o.admins[0] != defaultTopAdmin || o.interval != time.Second || o.timeout != 2*time.Second || o.rows != 10 || o.sort != "requests" {
		t.Fatalf("%+v %v", o, err)
	}
	if o.admins.String() != defaultTopAdmin {
		t.Fatal(o.admins.String())
	}
}

// TestTopStream: stdout not a TTY prints one frame per interval until the
// signal.
func TestTopStream(t *testing.T) {
	_, adminURL := startWWZAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	code, out, errOut := runArgs(t, ctx, "top", "--interval", "100ms", "--admin", adminURL, "--token", topToken)
	if code != exitOK || strings.Count(out, "wwz-load top ") < 2 || !strings.Contains(out, "\n\nwwz-load top ") || strings.Contains(out, "\x1b") {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
}

// fakeTerm is a TTY for the interactive path.
type fakeTerm struct {
	keys     io.Reader
	rawErr   error
	restored bool
	width    func() int
}

func (f *fakeTerm) MakeRaw() (func() error, error) {
	if f.rawErr != nil {
		return nil, f.rawErr
	}
	return func() error { f.restored = true; return nil }, nil
}

func (f *fakeTerm) Width() int      { return f.width() }
func (f *fakeTerm) Keys() io.Reader { return f.keys }

func withTerminal(t *testing.T, ft *fakeTerm) {
	t.Helper()
	old := openTerminal
	openTerminal = func(io.Writer) (terminal, bool) { return ft, true }
	t.Cleanup(func() { openTerminal = old })
}

func TestTopInteractive(t *testing.T) {
	_, adminURL := startWWZAdmin(t)
	ft := &fakeTerm{keys: strings.NewReader("spq"), width: func() int { return 60 }}
	withTerminal(t, ft)
	code, out, errOut := runArgs(t, context.Background(), "top", "--admin", adminURL, "--token", topToken)
	if code != exitOK || !ft.restored {
		t.Fatalf("%d %q restored=%v", code, errOut, ft.restored)
	}
	if !strings.HasPrefix(out, ansiAltScreenOn) || !strings.HasSuffix(out, ansiAltScreenOff) || !strings.Contains(out, "[PAUSED]") || !strings.Contains(out, "\r\n") {
		t.Fatalf("%q", out)
	}
	for _, l := range strings.Split(out, "\r\n") {
		if len([]rune(l)) > 60+len(ansiAltScreenOn)+8 {
			t.Fatalf("line wider than the terminal: %q", l)
		}
	}
	// --once on a TTY: one plain frame cut to the width.
	ft.width = func() int { return 30 }
	code, out, _ = runArgs(t, context.Background(), "top", "--once", "--interval", "100ms", "--admin", adminURL, "--token", topToken)
	if code != exitOK || strings.Contains(out, "\x1b") || len([]rune(strings.SplitN(out, "\n", 2)[0])) != 30 {
		t.Fatalf("%d %q", code, out)
	}
	// Raw mode fails.
	ft.rawErr = errors.New("no tty")
	code, _, errOut = runArgs(t, context.Background(), "top", "--admin", adminURL, "--token", topToken)
	if code != exitError || !strings.Contains(errOut, "raw mode: no tty") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// TestTopRestoreOnPanic: a panic inside the TUI still restores the terminal.
func TestTopRestoreOnPanic(t *testing.T) {
	_, adminURL := startWWZAdmin(t)
	ft := &fakeTerm{keys: strings.NewReader(""), width: func() int { panic("boom") }}
	withTerminal(t, ft)
	var out bytes.Buffer
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recover: %v", r)
			}
		}()
		runTop(context.Background(), []string{"--admin", adminURL, "--token", topToken}, &out, io.Discard)
	}()
	if !ft.restored || !strings.HasSuffix(out.String(), ansiAltScreenOff) {
		t.Fatalf("restored=%v %q", ft.restored, out.String())
	}
}

// TestTopSignalRestores: SIGTERM (ctx) ends the TUI with exit 0 and the
// terminal restored.
func TestTopSignalRestores(t *testing.T) {
	_, adminURL := startWWZAdmin(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ft := &fakeTerm{keys: pr, width: func() int { return 80 }}
	withTerminal(t, ft)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	code, out, _ := runArgs(t, ctx, "top", "--interval", "100ms", "--admin", adminURL, "--token", topToken)
	if code != exitOK || !ft.restored || !strings.HasSuffix(out, ansiAltScreenOff) {
		t.Fatalf("%d restored=%v", code, ft.restored)
	}
}

// TestOSTerminal: the real terminal over a pipe (not a TTY).
func TestOSTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if _, ok := openTerminal(w); ok {
		t.Fatal("pipe is not a TTY")
	}
	if _, ok := openTerminal(&bytes.Buffer{}); ok {
		t.Fatal("buffer is not a TTY")
	}
	ot := osTerminal{in: r, out: w}
	if _, err := ot.MakeRaw(); err == nil {
		t.Fatal("raw mode on a pipe")
	}
	if ot.Width() != defaultTopWidth || ot.Keys() != io.Reader(r) {
		t.Fatal("width/keys")
	}
}

// TestOSTerminalTTY: the success paths through the term seams.
func TestOSTerminalTTY(t *testing.T) {
	origRaw, origRestore, origSize := termMakeRaw, termRestore, termGetSize
	defer func() { termMakeRaw, termRestore, termGetSize = origRaw, origRestore, origSize }()
	restored := false
	termMakeRaw = func(int) (*term.State, error) { return &term.State{}, nil }
	termRestore = func(int, *term.State) error { restored = true; return nil }
	termGetSize = func(int) (int, int, error) { return 123, 40, nil }
	ot := osTerminal{in: os.Stdin, out: os.Stdout}
	restore, err := ot.MakeRaw()
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil || !restored {
		t.Fatal("restore not called")
	}
	if ot.Width() != 123 {
		t.Fatal("width")
	}
	orig := termIsTerminal
	defer func() { termIsTerminal = orig }()
	termIsTerminal = func(int) bool { return true }
	if tt, ok := openTerminal(os.Stdout); !ok || tt == nil {
		t.Fatal("TTY not opened")
	}
}
