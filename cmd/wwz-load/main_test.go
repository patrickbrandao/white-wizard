package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

const wwzCfg = `
server: {seed: 42}
drivers: [{name: openai}, {name: anthropic}]
profiles:
  latency:
    zero: {ttft: 0ms, itl: 0ms}
  generation:
    short:
      output_tokens: {dist: fixed, value: 8}
      reasoning_tokens: {min: 0, max: 0, probability: 0}
      tool_calls: {probability: 0, optional_param_probability: 1}
  errors:
    trunc: {mid_stream_rate: 100, mid_stream_types: [stream_truncated], after_chunks: 2}
models:
  - {id: wwz-fast, protocols: [openai, anthropic], latency: zero, generation: short, errors: none}
  - {id: m-trunc, protocols: [openai, anthropic], latency: zero, generation: short, errors: trunc}
`

// startWWZ serves the config in process and returns the base URL.
func startWWZ(t *testing.T) string {
	t.Helper()
	c, err := config.Parse([]byte(wwzCfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
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
	return "http://" + ln.Addr().String()
}

func runArgs(t *testing.T, ctx context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(ctx, args, &o, &e)
	return code, o.String(), e.String()
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	if code, out, _ := runArgs(t, ctx, "version"); code != 0 || !strings.HasPrefix(out, "wwz-load dev (commit ") || !strings.Contains(out, "go1") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := runArgs(t, ctx, "help"); code != 0 || !strings.Contains(out, "controller") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, errOut := runArgs(t, ctx, "bogus"); code != exitUsage || !strings.Contains(errOut, `unknown command "bogus"`) {
		t.Fatalf("bogus: %d %q", code, errOut)
	}
	if code, _, errOut := runArgs(t, ctx, "agents", "-h"); code != 0 || !strings.Contains(errOut, "-max-tool-rounds") {
		t.Fatalf("agents -h: %d %q", code, errOut)
	}
	if code, _, errOut := runArgs(t, ctx, "run", "-h"); code != 0 || !strings.Contains(errOut, "-sweep-rates") {
		t.Fatalf("-h: %d %q", code, errOut)
	}
	if v := versionTag(); v != "dev" {
		t.Fatal(v)
	}
}

// TestInvalidFlags: every invalid combination exits 2 naming the flag.
func TestInvalidFlags(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--nope"}, "-nope"},
		{[]string{"--rps", "x"}, "-rps"},
		{[]string{"extra"}, "unknown command"},
		{[]string{"run", "extra"}, `unexpected argument "extra"`},
		{[]string{"--protocol", "grpc"}, "--protocol"},
		{[]string{"--path", "v1"}, "--path"},
		{[]string{"--target", "ftp://x"}, "--target"},
		{[]string{"--target", "http://"}, "--target"},
		{[]string{"--header", "bad"}, "-header"},
		{[]string{"--max-tokens", "0"}, "--max-tokens"},
		{[]string{"--prompt", "a", "--prompt-file", "f"}, "--prompt and --prompt-file"},
		{[]string{"--scenario", "s.yaml", "--prompt", "a"}, "--scenario is exclusive"},
		{[]string{"--max-turns", "3"}, "need --scenario"},
		{[]string{"--scenario", "s.yaml", "--max-turns", "-1"}, "--max-turns"},
		{[]string{"--scenario", "s.yaml", "--tool-result-error-rate", "101"}, "--tool-result-error-rate"},
		{[]string{"--rps", "10", "--concurrency", "2"}, "--rps and --concurrency"},
		{[]string{"--ramp", "1:2:1s", "--sweep"}, "--ramp and --sweep"},
		{[]string{"--sweep-rates", "1"}, "need --sweep"},
		{[]string{"--rps", "1", "--step-duration", "1s"}, "--step-duration needs"},
		{[]string{"--ramp", "1:2:1s", "--duration", "5s"}, "--duration is not allowed with --ramp"},
		{[]string{"--concurrency", "2", "--max-inflight", "5"}, "--max-inflight needs"},
		{[]string{"--rps", "0"}, "--rps must be > 0"},
		{[]string{"--concurrency", "0"}, "--concurrency must be ≥ 1"},
		{[]string{"--ramp", "1:2"}, "--ramp"},
		{[]string{"--ramp", "a:2:1s"}, "--ramp"},
		{[]string{"--ramp", "0:0:1s"}, "--ramp"},
		{[]string{"--ramp", "1:2:1s", "--step-duration", "0s"}, "--step-duration must be > 0"},
		{[]string{"--sweep", "--step-duration", "-1s"}, "--step-duration must be > 0"},
		{[]string{"--sweep", "--sweep-rates", "1", "--sweep-from", "2"}, "exclusive"},
		{[]string{"--sweep", "--sweep-rates", "1,x"}, "--sweep-rates"},
		{[]string{"--sweep", "--sweep-from", "0"}, "--sweep-from"},
		{[]string{"--sweep", "--sweep-steps", "0"}, "--sweep-steps"},
		{[]string{"--sweep", "--sweep-stop-error-pct", "101"}, "--sweep-stop-error-pct"},
		{[]string{"--duration", "-1s"}, "--duration"},
		{[]string{"--requests", "-1"}, "--requests"},
		{[]string{"--timeout", "0s"}, "--timeout"},
		{[]string{"--warmup", "-1s"}, "--warmup"},
		{[]string{"--rps", "1", "--max-inflight", "0"}, "--max-inflight must be"},
		{[]string{"--output", "xml"}, "--output"},
		{[]string{"--fail-on-error-pct", "200"}, "--fail-on-error-pct"},
		{[]string{"--progress", "-1s"}, "--progress"},
		{[]string{"--admin", "nope"}, "--admin"},
	}
	for _, c := range cases {
		code, _, errOut := runArgs(t, context.Background(), c.args...)
		if code != exitUsage || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: code %d, stderr %q (want %q)", c.args, code, errOut, c.want)
		}
	}
}

// TestRunJSONAgainstWWZ: the spec 015 acceptance shape (--rps, stream, both
// protocols) with the JSON report, --out and --admin.
func TestRunJSONAgainstWWZ(t *testing.T) {
	base := startWWZ(t)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer env-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"requests":1}`))
	}))
	defer admin.Close()
	t.Setenv("WWZ_ADMIN_TOKEN", "env-tok")
	dir := t.TempDir()
	for _, proto := range []string{"openai", "anthropic"} {
		out := filepath.Join(dir, proto+".json")
		code, stdout, errOut := runArgs(t, context.Background(), "--target", base+"/", "--protocol", proto, "--rps", "200", "--duration", "500ms",
			"--warmup", "50ms", "--seed", "9", "--test-id", "t1", "--output", "json", "--out", out, "--admin", admin.URL, "--progress", "100ms")
		if code != 0 || stdout != "" || !strings.Contains(errOut, "started") {
			t.Fatalf("%s: %d %q %q", proto, code, stdout, errOut)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var rep loadgen.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatal(err)
		}
		tot := rep.Total
		if rep.Version != 1 || rep.Config.Mode != loadgen.ModeRPS || rep.Config.Seed != 9 || rep.Config.Protocol != proto || len(rep.Steps) != 0 {
			t.Fatalf("%s: %+v", proto, rep.Config)
		}
		if tot.Requests != 100 || tot.OK != 100 || tot.TTFT.Count != 100 || tot.ITL.Count != 700 || tot.E2E.P99 <= 0 || tot.OutputTokensPerS <= 0 {
			t.Fatalf("%s: %+v", proto, tot)
		}
		if tot.RPS < 190 || tot.RPS > 210 {
			t.Errorf("%s: rps %v", proto, tot.RPS)
		}
		if strings.Join(rep.Config.Headers, ",") != "x-test-id" || string(rep.AdminStats) == "" || !strings.Contains(string(rep.AdminStats), `"requests": 1`) {
			t.Fatalf("%s: headers %v admin %s", proto, rep.Config.Headers, rep.AdminStats)
		}
	}
}

func TestRunModesCSVAndText(t *testing.T) {
	base := startWWZ(t)
	ctx := context.Background()
	code, out, errOut := runArgs(t, ctx, "--target", base, "--protocol", "anthropic", "--stream=false", "--concurrency", "2", "--requests", "6", "--output", "csv", "--progress", "0")
	if code != 0 {
		t.Fatalf("csv: %d %s", code, errOut)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][0] != "total" || rows[1][5] != "6" || strings.Join(rows[0], ",") != strings.Join(loadgen.CSVHeader, ",") {
		t.Fatalf("csv %q %v", out, err)
	}
	code, out, errOut = runArgs(t, ctx, "run", "--target", base, "--ramp", "0:200:400ms", "--step-duration", "200ms", "--progress", "0")
	if code != 0 || !strings.Contains(out, "mode ramp 0:200:400ms") || !strings.Contains(out, "ttft p50") {
		t.Fatalf("ramp: %d %s %s", code, out, errOut)
	}
	code, out, errOut = runArgs(t, ctx, "--target", base, "--sweep", "--sweep-from", "50", "--sweep-to", "100", "--sweep-steps", "2", "--step-duration", "200ms", "--output", "json", "--progress", "0")
	var rep loadgen.Report
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || len(rep.Steps) != 2 || rep.Config.DurationMs != 400 || rep.Total.Requests != 30 {
		t.Fatalf("sweep: %d %s %s", code, out, errOut)
	}
	code, out, _ = runArgs(t, ctx, "--target", base, "--sweep", "--sweep-rates", "100", "--step-duration", "100ms", "--output", "json", "--progress", "0")
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.Total.Requests != 10 || len(rep.Config.SweepRates) != 1 {
		t.Fatalf("sweep rates: %d %s", code, out)
	}
}

// gateProxy forwards to target and records the peak of requests in flight;
// each request is held for hold so a closed loop can saturate.
func gateProxy(t *testing.T, target string, hold time.Duration) (url string, peak *atomic.Int64) {
	t.Helper()
	u, err := neturl.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var cur atomic.Int64
	peak = new(atomic.Int64)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		defer cur.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(hold)
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, peak
}

// TestRunConcurrencyAgainstWWZ (spec 015): --concurrency N reaches N requests
// in flight (±5%) and total.concurrency == N.
func TestRunConcurrencyAgainstWWZ(t *testing.T) {
	const n = 40
	url, peak := gateProxy(t, startWWZ(t), 30*time.Millisecond)
	code, out, errOut := runArgs(t, context.Background(), "--target", url, "--stream=false", "--concurrency", "40", "--duration", "600ms",
		"--output", "json", "--progress", "0")
	var rep loadgen.Report
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	if rep.Total.Concurrency != n || rep.Config.Concurrency != n || rep.Total.Errors != 0 || rep.Total.Requests < n {
		t.Fatalf("total %+v", rep.Total)
	}
	if p := peak.Load(); p < n*95/100 || p > n*105/100 {
		t.Fatalf("peak in flight %d, want %d ±5%%", p, n)
	}
}

// TestRunRampAgainstWWZ (spec 015): one step per interval; each step has its
// target_rps (mean of the interval) and requests within ±5%.
func TestRunRampAgainstWWZ(t *testing.T) {
	base := startWWZ(t)
	code, out, errOut := runArgs(t, context.Background(), "--target", base, "--stream=false", "--ramp", "100:300:800ms", "--step-duration", "200ms",
		"--output", "json", "--progress", "0")
	var rep loadgen.Report
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	wantRPS := []float64{125, 175, 225, 275}
	if len(rep.Steps) != len(wantRPS) {
		t.Fatalf("steps %d, want %d", len(rep.Steps), len(wantRPS))
	}
	var sum int64
	for i, s := range rep.Steps {
		want := wantRPS[i] * 0.2
		if s.TargetRPS != wantRPS[i] || float64(s.Requests) < want*0.95 || float64(s.Requests) > want*1.05 || s.Errors != 0 {
			t.Errorf("step %d: target %v requests %d (want %.0f ±5%%) errors %d", i, s.TargetRPS, s.Requests, want, s.Errors)
		}
		sum += s.Requests
	}
	if rep.Total.Requests != sum {
		t.Errorf("total %d != sum %d", rep.Total.Requests, sum)
	}
}

func TestRunFailures(t *testing.T) {
	base := startWWZ(t)
	ctx := context.Background()
	dir := t.TempDir()
	// --fail-on-error-pct: all streams truncated.
	code, out, errOut := runArgs(t, ctx, "--target", base, "--model", "m-trunc", "--requests", "3", "--fail-on-error-pct", "10", "--output", "json", "--progress", "0")
	if code != exitErrorRate || !strings.Contains(out, `"stream_truncated": 3`) || !strings.Contains(errOut, "--fail-on-error-pct") {
		t.Fatalf("fail-on-error: %d %s %s", code, out, errOut)
	}
	// --out not writable, --prompt-file missing or empty.
	if code, _, errOut := runArgs(t, ctx, "--target", base, "--out", filepath.Join(dir, "no", "such", "f")); code != exitError || !strings.Contains(errOut, "no such") {
		t.Fatalf("out: %d %s", code, errOut)
	}
	if code, _, _ := runArgs(t, ctx, "--target", base, "--prompt-file", filepath.Join(dir, "missing")); code != exitError {
		t.Fatalf("prompt-file missing: %d", code)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n  \n"), 0o600)
	if code, _, errOut := runArgs(t, ctx, "--target", base, "--prompt-file", empty); code != exitError || !strings.Contains(errOut, "no non-empty line") {
		t.Fatalf("prompt-file empty: %d %s", code, errOut)
	}
	// A failing stdout.
	if code := run(ctx, []string{"--target", base, "--requests", "1", "--progress", "0"}, failWriter{}, io.Discard); code != exitError {
		t.Fatalf("stdout: %d", code)
	}
}

func TestVersionFromAndHeaders(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "x", Value: "y"}}}
	if v := versionFrom("dev", bi); !strings.HasPrefix(v, "wwz-load v1.2.3 (commit abc, go") {
		t.Fatal(v)
	}
	if v := versionFrom("v9", nil); !strings.HasPrefix(v, "wwz-load v9 (commit unknown, ") {
		t.Fatal(v)
	}
	// -ldflags "-X main.commit=<rev>" wins over the VCS revision.
	oldCommit := commit
	t.Cleanup(func() { commit = oldCommit })
	commit = "def456"
	if v := versionFrom("dev", bi); !strings.HasPrefix(v, "wwz-load v1.2.3 (commit def456, go") {
		t.Fatal(v)
	}
	commit = oldCommit
	h := headerList{}
	if err := h.Set("x-a: 1"); err != nil || h.String() != "x-a: 1" {
		t.Fatal(h.String(), err)
	}
}

// TestRunSeams: errors of the unit and of the runner exit 1; the progress
// line marks the warmup.
func TestRunSeams(t *testing.T) {
	base := startWWZ(t)
	ctx := context.Background()
	defer func() { newUnit, runLoadgen = loadgen.NewRequestUnit, loadgen.Run }()
	newUnit = func(*loadgen.Client, loadgen.RequestPlan) (loadgen.Unit, error) { return nil, errors.New("unit boom") }
	if code, _, errOut := runArgs(t, ctx, "--target", base); code != exitError || !strings.Contains(errOut, "unit boom") {
		t.Fatalf("unit: %d %s", code, errOut)
	}
	newUnit = loadgen.NewRequestUnit
	runLoadgen = func(context.Context, loadgen.RunConfig, loadgen.Unit) (*loadgen.RunResult, error) {
		return nil, errors.New("run boom")
	}
	if code, _, errOut := runArgs(t, ctx, "--target", base); code != exitError || !strings.Contains(errOut, "run boom") {
		t.Fatalf("run: %d %s", code, errOut)
	}
	runLoadgen = loadgen.Run
	code, _, errOut := runArgs(t, ctx, "--target", base, "--rps", "20", "--warmup", "150ms", "--duration", "50ms", "--progress", "40ms", "--header", "x-b: 2")
	if code != 0 || !strings.Contains(errOut, "(warmup)") {
		t.Fatalf("warmup progress: %d %s", code, errOut)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

// TestRunKeyPromptsAndInterrupt: WWZ_LOAD_KEY, --key precedence, prompts
// drawn from --prompt-file, x-wwz-seed and an interrupted run.
func TestRunKeyPromptsAndInterrupt(t *testing.T) {
	var mu sync.Mutex
	var auths, seeds, bodies []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		seeds = append(seeds, r.Header.Get("x-wwz-seed"))
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fake.Close()
	pf := filepath.Join(t.TempDir(), "prompts")
	_ = os.WriteFile(pf, []byte("alfa\n\nbeta\n"), 0o600)
	t.Setenv("WWZ_LOAD_KEY", "env-key")
	ctx := context.Background()
	if code, _, errOut := runArgs(t, ctx, "--target", fake.URL, "--stream=false", "--requests", "8", "--prompt-file", pf, "--seed", "100", "--progress", "0"); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if code, _, _ := runArgs(t, ctx, "--target", fake.URL, "--stream=false", "--requests", "1", "--key", "flag-key", "--progress", "0"); code != 0 {
		t.Fatal(code)
	}
	mu.Lock()
	if auths[0] != "Bearer env-key" || auths[8] != "Bearer flag-key" || seeds[8] != "" {
		t.Fatalf("auth %v seeds %v", auths, seeds)
	}
	got := map[string]bool{}
	for i, b := range bodies[:8] {
		got[map[bool]string{true: "alfa", false: "beta"}[strings.Contains(b, "alfa")]] = true
		if seeds[i] == "" {
			t.Fatalf("seed missing %v", seeds)
		}
	}
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("prompts drawn %v", got)
	}
	// Interrupted: partial report, exit 0.
	ictx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	code, out, _ := runArgs(t, ictx, "--target", fake.URL, "--stream=false", "--rps", "50", "--duration", "1m", "--output", "json", "--progress", "0")
	var rep loadgen.Report
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.Interrupted || rep.Total.Requests == 0 {
		t.Fatalf("interrupt: %d %s", code, out)
	}
}
