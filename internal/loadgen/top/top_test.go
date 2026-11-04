package top

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// reply is one response of a fake control plane.
type reply struct {
	status int
	body   string
}

func statsReply(st Stats) reply {
	b, _ := json.Marshal(st)
	return reply{http.StatusOK, string(b)}
}

// fakeAdmin answers GET /admin/stats with the replies in order (the last one
// repeats) and checks the bearer.
func fakeAdmin(t *testing.T, token string, replies ...reply) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/stats" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		rp := replies[min(i, len(replies)-1)]
		i++
		mu.Unlock()
		w.WriteHeader(rp.status)
		_, _ = io.WriteString(w, rp.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clock returns t0, t0+1s, t0+2s, ... on each call.
func clock() func() time.Time {
	var mu sync.Mutex
	t := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := t
		t = t.Add(time.Second)
		return now
	}
}

func noSleep(context.Context, time.Duration) bool { return true }

func cfg(admins ...string) Config {
	return Config{Admins: admins, Token: "tok", Interval: time.Second, Timeout: time.Second, Rows: 10, Now: clock(), Sleep: noSleep}
}

func statsA(req, errs int64, kind int64) Stats {
	return Stats{
		UptimeS: 3600, Pod: "wwz-1", Store: "memory",
		Totals: StatsTotals{
			Requests: req, InFlight: 3, StreamsOpen: 2, ErrorsInjected: kind, RateLimited: req / 10, Blocked: 1,
			ByStatus: map[string]int64{"200": req - errs, "500": errs}, ErrorsByKind: map[string]int64{"overloaded": kind},
		},
		ByModel: []ModelStats{
			{Driver: "openai", Model: "wwz-fast", Requests: req, Errors: errs, TTFTP50MS: 10, TTFTP99MS: 50, E2EP50MS: 100, E2EP99MS: 400},
			{Driver: "anthropic", Model: "wwz-slow", Requests: req / 2, TTFTP50MS: 30, TTFTP99MS: 90, E2EP50MS: 300, E2EP99MS: 900},
		},
		ByKey: []KeyStats{{Key: "wwz-key-01", Status: "active", Requests: req, SpendUSD: 0.5}},
	}
}

func statsB(req int64) Stats {
	return Stats{
		UptimeS: 60, Pod: "wwz-2", Store: "memory",
		Totals: StatsTotals{
			Requests: req, InFlight: 1, StreamsOpen: 1, RateLimited: 0,
			ByStatus: map[string]int64{"200": req}, ErrorsByKind: map[string]int64{"timeout": 2},
		},
		ByModel: []ModelStats{{Driver: "openai", Model: "wwz-fast", Requests: req, TTFTP50MS: 20, TTFTP99MS: 40, E2EP50MS: 150, E2EP99MS: 300}},
		ByKey:   []KeyStats{{Key: "wwz-key-01", Requests: req, SpendUSD: 0.25}, {Key: "wwz-key-02", Status: "blocked", Requests: 1}},
	}
}

// TestOnceGoldenJSON: two fake control planes, summed counters, rates from
// two samples one second apart, max of the percentiles.
func TestOnceGoldenJSON(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 10, 4)), statsReply(statsA(160, 13, 6)))
	b := fakeAdmin(t, "tok", statsReply(statsB(50)), statsReply(statsB(40))) // B restarted: rate 0
	c := cfg(a.URL+"/", b.URL)
	m := New(c)
	var out bytes.Buffer
	if err := m.Once(context.Background(), &out, true, 0); err != nil {
		t.Fatal(err)
	}
	got := strings.NewReplacer(a.URL, "http://admin-a", b.URL, "http://admin-b").Replace(out.String())
	golden := "testdata/once.golden.json"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch:\n%s", got)
	}
	var f Frame
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if f.Totals.Requests != 200 || *f.Totals.RPS != 60 || f.ByModel[0].TTFTP50MS != 20 || f.ByModel[0].E2EP99MS != 400 {
		t.Fatalf("frame: %+v", f)
	}
	if *f.Totals.ErrorsByKind["timeout"].PerS != 0 || *f.Totals.ErrorsByKind["overloaded"].PerS != 2 {
		t.Fatalf("kinds: %+v", f.Totals.ErrorsByKind)
	}
}

// TestFirstFrameNull: the first frame has no rates (null / —).
func TestFirstFrameNull(t *testing.T) {
	a := fakeAdmin(t, "", statsReply(statsA(100, 10, 4)))
	c := cfg(a.URL)
	c.Token = ""
	c.Sleep = func(context.Context, time.Duration) bool { return false }
	var out bytes.Buffer
	if err := New(c).Once(context.Background(), &out, true, 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"rps": null`, `"errors_per_s": null`, `"per_s": null`, `"error_pct": null`} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("%s missing:\n%s", s, out.String())
		}
	}
	out.Reset()
	if err := New(c).Once(context.Background(), &out, false, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "req/s —") || !strings.Contains(out.String(), "pod wwz-1") || !strings.Contains(out.String(), "uptime 1h0m0s") {
		t.Fatal(out.String())
	}
}

func TestAuthError(t *testing.T) {
	a := fakeAdmin(t, "right", statsReply(statsA(1, 0, 0)))
	ok := fakeAdmin(t, "", statsReply(statsA(1, 0, 0)))
	c := cfg(ok.URL, a.URL)
	c.Token = "wrong"
	err := New(c).Once(context.Background(), io.Discard, true, 0)
	var ae *AuthError
	if !errors.As(err, &ae) || err.Error() != "token recusado por "+a.URL {
		t.Fatalf("%v", err)
	}
	// 401 on the second sample.
	b := fakeAdmin(t, "", statsReply(statsA(1, 0, 0)), reply{http.StatusUnauthorized, ""})
	c = cfg(b.URL)
	if err := New(c).Once(context.Background(), io.Discard, true, 0); !errors.As(err, &ae) {
		t.Fatalf("%v", err)
	}
}

func TestNoInstance(t *testing.T) {
	bad := fakeAdmin(t, "", reply{http.StatusInternalServerError, ""})
	junk := fakeAdmin(t, "", reply{http.StatusOK, "{"})
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := cfg(bad.URL, junk.URL, dead.URL, "http://bad\x7f")
	c.Timeout = 0
	err := New(c).Once(context.Background(), io.Discard, true, 0)
	if !errors.Is(err, ErrNoInstance) || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "decode:") {
		t.Fatalf("%v", err)
	}
}

// TestPartial: a failing instance is shown as unavailable and the frame goes
// on with the others; an instance without a previous sample adds no rate.
func TestPartial(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 0, 0)), statsReply(statsA(110, 0, 0)))
	b := fakeAdmin(t, "tok", reply{http.StatusBadGateway, ""}, statsReply(statsB(500)))
	m := New(cfg(a.URL, b.URL))
	f, err := m.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.Instances[1].OK || f.Instances[1].Error != "HTTP 502" {
		t.Fatalf("%+v", f.Instances)
	}
	txt := Render(f, 0, "")
	if !strings.Contains(txt, "unavailable: "+b.URL+": HTTP 502") || !strings.Contains(txt, "instances 1/2") {
		t.Fatal(txt)
	}
	f, err = m.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *f.Totals.RPS != 10 || f.Totals.Requests != 610 || f.Store != "memory" || strings.Contains(Render(f, 0, ""), " pod ") {
		t.Fatalf("%+v", f.Totals)
	}
}

// TestSharedStoreKeys: with valkey every instance reports the cluster-wide
// key usage, so the merge takes the maximum.
func TestSharedStoreKeys(t *testing.T) {
	st := func(req int64, spend float64, pod string) Stats {
		return Stats{Store: "valkey", Pod: pod, ByKey: []KeyStats{{Key: "k", Status: "active", Requests: req, SpendUSD: spend}}}
	}
	a := fakeAdmin(t, "tok", statsReply(st(10, 1, "a")), statsReply(st(20, 2, "a")))
	b := fakeAdmin(t, "tok", statsReply(st(11, 1.5, "b")), statsReply(st(30, 3, "b")))
	m := New(cfg(a.URL, b.URL))
	if _, err := m.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, err := m.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := f.ByKey[0]
	if k.Requests != 30 || k.SpendUSD != 3 || *k.RPS != 19 || f.Store != "valkey" {
		t.Fatalf("%+v", k)
	}
}

func TestMixedStoresAndSameInstant(t *testing.T) {
	a := Stats{Store: "memory"}
	b := Stats{Store: "valkey"}
	at := time.Unix(100, 0)
	last := map[string]point{"a": {at: at, st: &a}}
	f := build(last, []result{{admin: "a", st: &a}, {admin: "b", st: &b}}, at, Config{Sort: "requests"})
	if f.Store != "memory,valkey" || f.Totals.RPS != nil {
		t.Fatalf("%+v", f)
	}
	// Ties: keys by name, error kinds by name; status taken from the
	// first instance that has one.
	x := Stats{Store: "memory", Totals: StatsTotals{ErrorsByKind: map[string]int64{"b": 1, "a": 1}}, ByKey: []KeyStats{{Key: "k2", Requests: 1}, {Key: "k1", Requests: 1}}}
	y := Stats{Store: "memory", ByKey: []KeyStats{{Key: "k2", Status: "banned"}}}
	f = build(nil, []result{{admin: "x", st: &x}, {admin: "y", st: &y}}, at, Config{})
	if f.ByKey[0].Key != "k1" || f.ByKey[1].Status != "banned" || f.ByKey[1].Requests != 1 {
		t.Fatalf("%+v", f.ByKey)
	}
	if txt := Render(f, 0, ""); !strings.Contains(txt, "a           1      —\nb           1      —") {
		t.Fatal(txt)
	}
}

func TestSort(t *testing.T) {
	r := func(v float64) *float64 { return &v }
	rows := []ModelRow{
		{Driver: "b", Model: "x", Requests: 5, RPS: r(1), ErrorPct: r(50), TTFTP99MS: 1, E2EP99MS: 9},
		{Driver: "a", Model: "y", Requests: 9, RPS: r(3), ErrorPct: r(10), TTFTP99MS: 7, E2EP99MS: 2},
		{Driver: "a", Model: "x", Requests: 9, RPS: r(3), ErrorPct: r(10), TTFTP99MS: 7, E2EP99MS: 2},
	}
	order := func() string {
		var s []string
		for _, m := range rows {
			s = append(s, m.Driver+m.Model)
		}
		return strings.Join(s, ",")
	}
	for sort, want := range map[string]string{"requests": "ax,ay,bx", "errors": "bx,ax,ay", "ttft": "ax,ay,bx", "e2e": "bx,ax,ay"} {
		sortModels(rows, sort)
		if order() != want {
			t.Fatalf("%s: %s", sort, order())
		}
	}
	rows = []ModelRow{{Model: "z"}, {Model: "e", Requests: 4, errors: 1}, {Model: "r", Requests: 2}}
	sortModels(rows, "errors")
	if order() != "e,r,z" {
		t.Fatal(order())
	}
	sortModels(rows, "requests")
	if order() != "e,r,z" {
		t.Fatal(order())
	}
	if nextSort("e2e") != "requests" || nextSort("bogus") != "requests" || nextSort("requests") != "errors" {
		t.Fatal("nextSort")
	}
	if !ValidSort("ttft") || ValidSort("x") {
		t.Fatal("ValidSort")
	}
}

func TestRenderRowsWidthFooter(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 10, 4)))
	c := cfg(a.URL)
	c.Rows = 1
	f, err := New(c).Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	txt := Render(f, 20, "footer line")
	for _, l := range strings.Split(strings.TrimSuffix(txt, "\n"), "\n") {
		if len([]rune(l)) > 20 {
			t.Fatalf("line too long: %q", l)
		}
	}
	if !strings.HasSuffix(txt, "\nfooter line\n") {
		t.Fatal(txt)
	}
	full := Render(f, 0, "")
	if strings.Contains(full, "wwz-slow") || !strings.Contains(full, "wwz-fast") || !strings.Contains(full, "status 200=90 500=10") {
		t.Fatal(full)
	}
	var out bytes.Buffer
	if err := WriteJSON(&out, f); err != nil || strings.Contains(out.String(), "wwz-slow") {
		t.Fatal(err, out.String())
	}
	f.ByModel[0].TTFTP50MS = math.NaN()
	if err := WriteJSON(io.Discard, f); err == nil {
		t.Fatal("NaN must fail")
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("write failed")
	}
	w.after--
	return len(p), nil
}

func TestWriteErrors(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(1, 0, 0)))
	if err := New(cfg(a.URL)).Once(context.Background(), &failWriter{}, false, 0); err == nil {
		t.Fatal("text write")
	}
	if err := New(cfg(a.URL)).Once(context.Background(), &failWriter{}, true, 0); err == nil {
		t.Fatal("json write")
	}
	if err := New(cfg(a.URL)).Stream(context.Background(), &failWriter{}, 0); err == nil {
		t.Fatal("stream write")
	}
}

func TestStream(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 0, 0)), statsReply(statsA(105, 0, 0)), statsReply(statsA(115, 0, 0)))
	c := cfg(a.URL)
	n := 0
	c.Sleep = func(context.Context, time.Duration) bool { n++; return n < 3 }
	var out bytes.Buffer
	if err := New(c).Stream(context.Background(), &out, 0); err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(out.String(), "\n\nwwz-load top")
	if len(frames) != 3 || !strings.Contains(frames[1], "req/s 5.0") || !strings.Contains(frames[2], "req/s 10.0") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("%d frames:\n%s", len(frames), out.String())
	}
	// First sample fails / later 401.
	if err := New(cfg("http://bad\x7f")).Stream(context.Background(), io.Discard, 0); !errors.Is(err, ErrNoInstance) {
		t.Fatal(err)
	}
	b := fakeAdmin(t, "", statsReply(statsA(1, 0, 0)), reply{http.StatusUnauthorized, ""})
	if err := New(cfg(b.URL)).Stream(context.Background(), io.Discard, 0); err == nil {
		t.Fatal("401 later")
	}
}

func TestSleep(t *testing.T) {
	if !sleep(context.Background(), time.Millisecond) {
		t.Fatal("timer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleep(ctx, time.Hour) {
		t.Fatal("ctx")
	}
	m := New(Config{})
	if m.cfg.Client != http.DefaultClient || m.cfg.Now == nil || m.cfg.Sleep == nil || m.cfg.Sort != "requests" {
		t.Fatalf("%+v", m.cfg)
	}
}

// screen collects the interactive output.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *screen) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.String(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("%q not drawn:\n%s", sub, s.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *screen) frames() int { return strings.Count(s.String(), ansiHome+ansiClear) }

// TestInteractiveKeys drives the key loop with simulated input.
func TestInteractiveKeys(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 0, 0)), statsReply(statsA(150, 0, 0)))
	c := cfg(a.URL)
	c.Interval = 5 * time.Millisecond
	c.Now = time.Now
	m := New(c)
	pr, pw := io.Pipe()
	scr := &screen{}
	done := make(chan error, 1)
	go func() {
		done <- m.Interactive(context.Background(), Screen{Keys: pr, Out: scr, Width: func() int { return 120 }})
	}()
	scr.waitFor(t, "sort: requests  p pause")
	for deadline := time.Now().Add(5 * time.Second); scr.frames() < 2; time.Sleep(2 * time.Millisecond) { // a tick redrew
		if time.Now().After(deadline) {
			t.Fatal("no tick")
		}
	}
	_, _ = pw.Write([]byte("s"))
	scr.waitFor(t, "(sort: errors)")
	_, _ = pw.Write([]byte("p"))
	scr.waitFor(t, "[PAUSED]")
	n := scr.frames()
	_, _ = pw.Write([]byte("x")) // ignored, no redraw
	time.Sleep(30 * time.Millisecond)
	if scr.frames() != n {
		t.Fatalf("redrew while paused: %d → %d", n, scr.frames())
	}
	_, _ = pw.Write([]byte("p"))
	for deadline := time.Now().Add(5 * time.Second); scr.frames() <= n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no redraw after resume")
		}
	}
	if !strings.Contains(scr.String(), "\r\n") {
		t.Fatal("raw mode needs \\r\\n")
	}
	_, _ = pw.Write([]byte("q"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveExits(t *testing.T) {
	a := fakeAdmin(t, "tok", statsReply(statsA(100, 0, 0)))
	width := func() int { return 80 }
	run := func(ctx context.Context, keys io.Reader, out io.Writer, c Config) error {
		return New(c).Interactive(ctx, Screen{Keys: keys, Out: out, Width: width})
	}
	c := cfg(a.URL)
	c.Interval = time.Hour
	// Ctrl-C.
	if err := run(context.Background(), strings.NewReader("\x03"), io.Discard, c); err != nil {
		t.Fatal(err)
	}
	// EOF on the keys.
	if err := run(context.Background(), strings.NewReader(""), io.Discard, c); err != nil {
		t.Fatal(err)
	}
	// Signal (ctx).
	ctx, cancel := context.WithCancel(context.Background())
	pr, _ := io.Pipe()
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := run(ctx, pr, io.Discard, c); err != nil {
		t.Fatal(err)
	}
	// Draw fails.
	if err := run(context.Background(), pr, &failWriter{}, c); err == nil {
		t.Fatal("draw error")
	}
	// First sample fails.
	if err := run(context.Background(), pr, io.Discard, cfg("http://bad\x7f")); !errors.Is(err, ErrNoInstance) {
		t.Fatal(err)
	}
	// 401 on a tick.
	b := fakeAdmin(t, "", statsReply(statsA(1, 0, 0)), reply{http.StatusUnauthorized, ""})
	c = cfg(b.URL)
	c.Interval = 5 * time.Millisecond
	var ae *AuthError
	if err := run(context.Background(), pr, io.Discard, c); !errors.As(err, &ae) {
		t.Fatal(err)
	}
}

func TestDelta(t *testing.T) {
	if delta(10, 4, 2) != 3 || delta(3, 4, 1) != 0 {
		t.Fatal("delta")
	}
}
