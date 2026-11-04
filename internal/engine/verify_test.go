package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// ccFixture reads a capture of Claude Code 2.1.308 (spec 020).
func ccFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "claude-code", "2.1.308", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawInput is the Input the data plane builds for a raw request to drv:
// the lower-cased headers and, as the server stores it for a driver that
// asks, the raw head.
func rawInput(t *testing.T, drv driver.Driver, raw []byte) Input {
	t.Helper()
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("no end of head")
	}
	line, block, _ := strings.Cut(string(raw[:end+4]), "\r\n")
	parts := strings.SplitN(line, " ", 3)
	h := driver.MapHeaders{}
	for _, hd := range (driver.RawHead{Block: block}).Headers() {
		h[strings.ToLower(hd.Name)] = hd.Value
	}
	if driver.WantsRawHead(drv) {
		driver.SetRawHead(h, []byte(parts[0]), []byte(parts[1]), []byte(parts[2]), []byte(block))
	}
	path, _, _ := strings.Cut(parts[1], "?")
	kind := driver.KindMessages
	if strings.HasSuffix(path, "/count_tokens") {
		kind = driver.KindCountTokens
	}
	return Input{Driver: drv, Kind: kind, Method: parts[0], Path: path, Headers: h, Body: raw[end+4:], ClientIP: "10.1.2.3"}
}

// headerValue is the value of name in the head of raw.
func headerValue(t *testing.T, raw []byte, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(string(raw), "\r\n"+name+": ")
	if !ok {
		t.Fatalf("no %s", name)
	}
	v, _, _ := strings.Cut(rest, "\r\n")
	return v
}

// strictEngine serves the anthropic driver with client_verification (off
// or claude-code) and the API key of capture 004 limited to one request
// a minute; the log goes to logBuf.
func strictEngine(t *testing.T, verification string, logBuf *bytes.Buffer) (*Engine, driver.Driver) {
	t.Helper()
	key := headerValue(t, ccFixture(t, "004-sdk-cli-apikey-turn.raw"), "x-api-key")
	cfg, err := config.Parse([]byte(`
server: {auth: keys, seed: 7}
drivers: [{name: anthropic, options: {client_verification: ` + verification + `}}]
keys: [{key: ` + key + `, label: cc, limits: {rpm: 1}}]
profiles:
  latency: {zero: {}}
models:
  - {id: claude-opus-5-5, latency: zero}
`))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e := New(cfg, limits.NewMemory(), scenario.NewRegistry(), NewMetrics(metrics.New(1000), nil), log)
	e.Recorder = NewRecorder(10)
	drv, err := driver.New("anthropic", map[string]string{"client_verification": verification})
	if err != nil {
		t.Fatal(err)
	}
	return e, drv
}

// TestClientVerificationRefusal: with client_verification: claude-code a
// forged request gets the driver's 403 forged_request_error at once,
// before the credential and the limits: three refusals leave the key's
// rpm of 1 untouched, so the capture itself still gets its 200 and only
// the next one the 429. The refusal counts in wwz_requests_total and
// wwz_client_verification_failures_total, goes to the log with the
// reason and detail, to /admin/requests and to the event stream as an
// error with fields.reason and no subject (spec 013, spec 020).
func TestClientVerificationRefusal(t *testing.T) {
	var logBuf bytes.Buffer
	e, drv := strictEngine(t, "claude-code", &logBuf)
	e.SetPodName("pod-7")
	ctx := context.Background()
	raw := ccFixture(t, "004-sdk-cli-apikey-turn.raw")
	forged := bytes.Replace(raw, []byte("?beta=true"), nil, 1)
	for range 3 {
		in := rawInput(t, drv, forged)
		in.Headers["x-test-id"] = "t-forged"
		w := core.NewMemWriter()
		e.Handle(ctx, in, w)
		var env struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(w.Body(), &env); err != nil {
			t.Fatal(err)
		}
		hd := w.Headers()
		if w.StatusCode() != 403 || env.Type != "error" || env.Error.Type != "forged_request_error" ||
			!strings.HasPrefix(env.Error.Message, "Client verification failed: this request does not match Claude Code 2.1.308; the client is forging Claude Code requests (query: ") ||
			!strings.HasPrefix(env.RequestID, "req_") || hd["request-id"] != env.RequestID {
			t.Fatalf("%d %v %s", w.StatusCode(), hd, w.Body())
		}
		if hd["x-wwz-client-verification"] != "failed; reason=query" || hd["x-should-retry"] != "false" || hd["x-wwz-pod"] != "pod-7" {
			t.Errorf("headers %v", hd)
		}
	}
	if n := e.M.ClientVerificationFailures.Value("anthropic", "query"); n != 3 {
		t.Errorf("wwz_client_verification_failures_total = %d", n)
	}
	if n := e.M.Requests.Value("anthropic", "", "messages", "403"); n != 3 {
		t.Errorf("wwz_requests_total 403 = %d", n)
	}
	for _, want := range []string{"error=forged_request", "client_verification_reason=query", `client_verification_detail="expected`, "status=403", "endpoint=messages"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log without %s:\n%s", want, logBuf.String())
		}
	}
	recs := e.Recorder.Last(10, nil)
	if len(recs) != 3 || recs[0].Status != 403 || recs[0].Error != "forged_request" || recs[0].TestID != "t-forged" || recs[0].IP != "10.1.2.3" || recs[0].Key != "" {
		t.Errorf("records %+v", recs)
	}
	evs, err := e.Store.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("events %+v", evs)
	}
	ev := evs[0]
	if ev.Type != "error" || ev.Error != "forged_request" || ev.Status != 403 || ev.Driver != "anthropic" || ev.Endpoint != "messages" ||
		ev.TestID != "t-forged" || ev.Fields["reason"] != "query" || ev.Fields["denied"] != nil || ev.Key != "" || ev.Entity != nil {
		t.Errorf("event %+v", ev)
	}
	// Nothing was counted: the capture gets the key's one request.
	for i, want := range []int{200, 429} {
		w := core.NewMemWriter()
		e.Handle(ctx, rawInput(t, drv, raw), w)
		if w.StatusCode() != want {
			t.Fatalf("capture %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	// count_tokens is verified too, under its own endpoint.
	count := bytes.Replace(ccFixture(t, "014-sdk-cli-apikey-count-tokens.raw"), []byte("X-Stainless-OS: MacOS"), []byte("X-Stainless-OS: Plan9"), 1)
	w := core.NewMemWriter()
	e.Handle(ctx, rawInput(t, drv, count), w)
	if w.StatusCode() != 403 || w.Headers()["x-wwz-client-verification"] != "failed; reason=header_value" {
		t.Errorf("count_tokens: %d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
	if e.M.ClientVerificationFailures.Value("anthropic", "header_value") != 1 || e.M.Requests.Value("anthropic", "", "count_tokens", "403") != 1 {
		t.Error("count_tokens refusal not counted")
	}
}

// TestClientVerificationOff: with off (the default) the driver asks for
// no raw head and verifies nothing: the capture and its forged copy are
// served as before.
func TestClientVerificationOff(t *testing.T) {
	var logBuf bytes.Buffer
	e, drv := strictEngine(t, "off", &logBuf)
	raw := ccFixture(t, "004-sdk-cli-apikey-turn.raw")
	in := rawInput(t, drv, bytes.Replace(raw, []byte("?beta=true"), nil, 1))
	if _, ok := driver.RawHeadOf(in.Headers); ok {
		t.Fatal("raw head stored with off")
	}
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 200 || e.M.ClientVerificationFailures.Total() != 0 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
}

// fakeVerifier refuses every Messages request with err; it has no
// IDMinter, so the engine's own request id stays.
type fakeVerifier struct {
	driver.Driver
	err *core.Error
}

func (fakeVerifier) VerifiesClient(kind driver.Kind) bool { return kind == driver.KindMessages }

func (f fakeVerifier) VerifyClient(driver.Kind, driver.RawHead, []byte) *core.Error { return f.err }

// TestRefuseForgedEdges: a long detail is cut to 200 bytes in the log; an
// error without Extra still gets the pod header; without a log nothing
// breaks; a forged request is a 403 for statusOf.
func TestRefuseForgedEdges(t *testing.T) {
	var logBuf bytes.Buffer
	e, drv := strictEngine(t, "off", &logBuf)
	e.SetPodName("pod-1")
	long := strings.Repeat("é", 150) // 300 bytes
	in := rawInput(t, drv, ccFixture(t, "004-sdk-cli-apikey-turn.raw"))
	in.Driver = fakeVerifier{drv, driver.ForgedRequest("fingerprint", long, "forged")}
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 403 || w.Headers()["x-wwz-pod"] != "pod-1" {
		t.Fatalf("%d %v", w.StatusCode(), w.Headers())
	}
	if !strings.Contains(logBuf.String(), "client_verification_detail="+strings.Repeat("é", 100)+"\n") {
		t.Errorf("detail not cut at 200 bytes:\n%s", logBuf.String())
	}
	e.Log = nil
	in.Driver = fakeVerifier{drv, &core.Error{Kind: core.ErrForgedRequest}}
	w = core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 403 || w.Headers()["x-wwz-pod"] != "pod-1" || e.M.ClientVerificationFailures.Value("anthropic", "") != 1 {
		t.Errorf("bare forged error: %d %v", w.StatusCode(), w.Headers())
	}
	if s := (&Engine{}).statusOf(&core.Error{Kind: core.ErrForgedRequest}); s != 403 {
		t.Errorf("statusOf = %d", s)
	}
}
