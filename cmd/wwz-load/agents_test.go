package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/agents"
)

const agentsCfg = `
server: {seed: 42}
drivers: [{name: openai}, {name: anthropic}]
profiles:
  latency:
    zero: {ttft: 0ms, itl: 0ms}
  generation:
    agent:
      output_tokens: {dist: fixed, value: 8}
      reasoning_tokens: {min: 0, max: 0, probability: 0}
      tool_calls: {probability: 60, optional_param_probability: 1}
  errors:
    limited: {rate: 100, types: [rate_limit], retry_after: 1s}
models:
  - {id: wwz-fast, protocols: [openai, anthropic], latency: zero, generation: agent, errors: none}
  - {id: m-429, protocols: [openai, anthropic], latency: zero, generation: agent, errors: limited}
`

// zeroTools writes a tools.yaml without simulated latency.
func zeroTools(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.yaml")
	zero := "{latency: {min: 0ms, max: 0ms}}"
	cfg := "latency: {min: 0ms, max: 0ms}\ntools: {run_command: " + zero + ", web_search: " + zero + ", fetch_url: " + zero + "}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentsJSON(t *testing.T, out string) agents.Report {
	t.Helper()
	var rep agents.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return rep
}

// TestAgentsAcceptance: --agents 5 --turns 3 against the wwz in process
// completes every session with tool calls, in both protocols.
func TestAgentsAcceptance(t *testing.T) {
	base := startWWZWith(t, agentsCfg)
	tcfg := zeroTools(t)
	for _, p := range []string{"openai", "anthropic"} {
		code, out, errOut := runArgs(t, context.Background(), "agents", "--target", base, "--protocol", p, "--agents", "5", "--tools-config", tcfg,
			"--turns", "3", "--profile", "mixed", "--seed", "7", "--output", "json", "--progress", "0", "--tool-prob", "70", "--max-tool-rounds", "40")
		rep := agentsJSON(t, out)
		s := rep.Total.Sessions
		if code != 0 || rep.Kind != "agents" || s.Started != 5 || s.Completed != 5 || rep.Total.Turns != 15 || rep.Total.ToolCalls == 0 {
			t.Fatalf("%s: %d %s %+v", p, code, errOut, rep.Total)
		}
		c := rep.Config
		if c.Tools != agents.ToolsInProcess || c.Seed != 7 || c.Profile != "mixed" || !slices.Equal(c.Headers, []string{"x-wwz-tool-prob"}) ||
			len(rep.Agents) != 5 || rep.Agents[1].Profile != "pesquisa" || c.Path != loadgen.Protocol(p).DefaultPath() {
			t.Fatalf("%s: config %+v", p, c)
		}
	}
}

// TestAgentsRetryAfter: chaos 429 with retry-after: 1 waits ≥ 1s and counts
// the retry; --retries 0 puts the error in errors_by_type.
func TestAgentsRetryAfter(t *testing.T) {
	base := startWWZWith(t, agentsCfg)
	start := time.Now()
	code, out, _ := runArgs(t, context.Background(), "agents", "--target", base, "--model", "m-429", "--agents", "1",
		"--turns", "1", "--retries", "1", "--backoff", "1ms", "--output", "json", "--progress", "0")
	rep := agentsJSON(t, out)
	if code != 0 || time.Since(start) < time.Second || rep.Total.Retries != 1 || rep.Total.RetryWait.Max < 1000 ||
		rep.Total.RetriesByType["http_429/requests"] != 1 || rep.Total.Requests != 2 {
		t.Fatalf("retry: %d %v %+v", code, time.Since(start), rep.Total)
	}
	code, out, _ = runArgs(t, context.Background(), "agents", "--target", base, "--model", "m-429", "--agents", "1",
		"--turns", "1", "--retries", "0", "--output", "json", "--progress", "0", "--fail-on-error-pct", "50")
	rep = agentsJSON(t, out)
	if code != exitErrorRate || rep.Total.Retries != 0 || rep.Total.ErrorsByType["http_429/requests"] != 1 ||
		rep.Agents[0].ErrorsByType["http_429/requests"] != 1 || rep.Total.Sessions.FailedByReason["http_429/requests"] != 1 {
		t.Fatalf("no retry: %d %+v", code, rep.Total)
	}
}

// TestAgentsToolsURL: --tools-url sends the calls to the wwz-tools server
// with the bearer of $WWZ_TOOLS_TOKEN; config.tools is the URL.
func TestAgentsToolsURL(t *testing.T) {
	base := startWWZWith(t, agentsCfg)
	var calls atomic.Int64
	var auth atomic.Value
	tools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"content":"ok","is_error":false,"outcome":"success","latency_ms":0}`)
	}))
	defer tools.Close()
	t.Setenv("WWZ_TOOLS_TOKEN", "segredo")
	code, out, errOut := runArgs(t, context.Background(), "agents", "--target", base, "--agents", "2", "--turns", "2",
		"--tools-url", tools.URL, "--output", "csv", "--progress", "0", "--tool-prob", "100", "--max-tool-rounds", "3")
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if code != 0 || err != nil || len(rows) != 4 || rows[0][0] != "agent" || rows[3][0] != "total" || calls.Load() == 0 || auth.Load() != "Bearer segredo" {
		t.Fatalf("%d %v %s %s", code, err, out, errOut)
	}
	// tool-prob 100 never ends a turn: every session fails with max_tool_rounds.
	if rows[3][4] != "2" {
		t.Fatalf("failed sessions %v", rows[3])
	}
}

// TestAgentsOutputs: text, --out, --admin, --progress, --tools-config and
// the failure paths (exit 1).
func TestAgentsOutputs(t *testing.T) {
	base := startWWZWith(t, agentsCfg)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"requests":3}`)
	}))
	defer admin.Close()
	dir := t.TempDir()
	toolsCfg := zeroTools(t)
	t.Setenv("WWZ_ADMIN_TOKEN", "adm")
	t.Setenv("WWZ_LOAD_KEY", "")
	out := filepath.Join(dir, "rep.json")
	code, _, errOut := runArgs(t, context.Background(), "agents", "--target", base+"/", "--agents", "2", "--turns", "2",
		"--duration", "300ms", "--think-time", "1ms", "--spawn-interval", "10ms", "--progress", "50ms", "--admin", admin.URL,
		"--tools-config", toolsCfg, "--output", "json", "--out", out, "--test-id", "t-9")
	data, err := os.ReadFile(out) //nolint:gosec // G304: test file
	if code != 0 || err != nil || !strings.Contains(errOut, "wwz-load agents: ") {
		t.Fatalf("%d %v %s", code, err, errOut)
	}
	rep := agentsJSON(t, string(data))
	var admBuf bytes.Buffer
	_ = json.Compact(&admBuf, rep.AdminStats)
	if admBuf.String() != `{"requests":3}` || rep.Config.DurationMs != 300 || rep.Total.Sessions.Started < 2 ||
		!slices.Equal(rep.Config.Headers, []string{"x-test-id"}) || rep.Config.Target != base {
		t.Fatalf("%+v %s", rep.Config, rep.AdminStats)
	}

	code, txt, _ := runArgs(t, context.Background(), "agents", "--target", base, "--agents", "1", "--turns", "1", "--progress", "0", "--profile", "chat")
	if code != 0 || !strings.Contains(txt, "wwz-load agents ") || !strings.Contains(txt, "sessions: 1 started") {
		t.Fatalf("text %d %s", code, txt)
	}

	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("nope: 1\n"), 0o600)
	fails := [][]string{
		{"--tools-config", bad},
		{"--tools-config", filepath.Join(dir, "missing.yaml")},
		{"--out", filepath.Join(dir, "no", "such", "dir.json")},
	}
	for _, args := range fails {
		args = append([]string{"agents", "--target", base, "--agents", "1", "--turns", "1", "--progress", "0", "--profile", "chat"}, args...)
		if code, _, errOut := runArgs(t, context.Background(), args...); code != exitError {
			t.Errorf("%v: %d %s", args, code, errOut)
		}
	}
	if code := run(context.Background(), []string{"agents", "--target", base, "--agents", "1", "--turns", "1", "--progress", "0", "--profile", "chat"}, failWriter{}, io.Discard); code != exitError {
		t.Errorf("write failure: %d", code)
	}
	old := runAgentsFn
	runAgentsFn = func(context.Context, *loadgen.Client, agents.Config) (*agents.Result, error) {
		return nil, errors.New("boom")
	}
	defer func() { runAgentsFn = old }()
	if code, _, errOut := runArgs(t, context.Background(), "agents", "--target", base); code != exitError || !strings.Contains(errOut, "boom") {
		t.Errorf("run error: %d %s", code, errOut)
	}
}

// TestAgentsInvalidFlags: the run-only flags and every invalid value exit 2.
func TestAgentsInvalidFlags(t *testing.T) {
	cases := [][]string{
		{"--rps", "10"},
		{"--concurrency", "2"},
		{"--ramp", "1:2:3s"},
		{"--sweep"},
		{"--prompt", "x"},
		{"--prompt-file", "f"},
		{"--scenario", "s.yaml"},
		{"--warmup", "1s"},
		{"--requests", "1"},
		{"--instances", "2"},
		{"--nope"},
		{"extra"},
		{"--protocol", "grpc"},
		{"--profile", "all"},
		{"--path", "v1"},
		{"--target", "ftp://x"},
		{"--admin", "x"},
		{"--tools-url", "nope"},
		{"--tools-url", "http://h", "--tools-config", "c.yaml"},
		{"--max-tokens", "0"},
		{"--timeout", "0s"},
		{"--output", "xml"},
		{"--progress", "-1s"},
		{"--fail-on-error-pct", "101"},
		{"--tool-prob", "120"},
		{"--sessions", "2", "--duration", "1s"},
		{"--agents", "0"},
		{"--turns", "0"},
		{"--max-tool-rounds", "0"},
		{"--sessions", "0"},
		{"--retries", "-1"},
		{"--duration", "-1s"},
		{"--think-time", "-1s"},
		{"--spawn-interval", "-1s"},
		{"--backoff", "-1s"},
		{"--backoff-max", "-1s"},
		{"--retry-after-max", "-1s"},
	}
	for _, args := range cases {
		code, _, errOut := runArgs(t, context.Background(), append([]string{"agents"}, args...)...)
		if code != exitUsage || !strings.Contains(errOut, "wwz-load agents: ") {
			t.Errorf("%v: %d %q", args, code, errOut)
		}
	}
	if code, _, errOut := runArgs(t, context.Background(), "agents", "--rps", "10"); !strings.Contains(errOut, "--rps is not allowed with agents") || code != exitUsage {
		t.Fatal(errOut)
	}
}

func TestAgentsKeyDefaults(t *testing.T) {
	t.Setenv("WWZ_LOAD_KEY", "k-env")
	o, err := parseAgents([]string{"--target", "http://h"}, io.Discard)
	if err != nil || o.key != "k-env" || o.path != "/v1/chat/completions" {
		t.Fatalf("%v %+v", err, o)
	}
	t.Setenv("WWZ_LOAD_KEY", "")
	if o, _ = parseAgents(nil, io.Discard); o.key != defaultKey {
		t.Fatal(o.key)
	}
	if o, _ = parseAgents([]string{"--key", "", "--admin-token", ""}, io.Discard); o.key != "" || o.adminToken != "" {
		t.Fatal(o.key, o.adminToken)
	}
}

// TestAgentsInterrupt: the interruption (SIGINT/SIGTERM end ctx in main)
// starts no new turn and still prints the partial report, exit 0.
func TestAgentsInterrupt(t *testing.T) {
	base := startWWZWith(t, agentsCfg)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	start := time.Now()
	code, out, _ := runArgs(t, ctx, "agents", "--target", base, "--agents", "3", "--turns", "5", "--think-time", "1h",
		"--tool-prob", "0", "--max-tool-rounds", "1000", "--output", "json", "--progress", "0")
	rep := agentsJSON(t, out)
	s := rep.Total.Sessions
	if code != 0 || !rep.Interrupted || s.Started != 3 || s.Interrupted != 3 || rep.Total.Turns > 3 || rep.Total.Turns == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("%d %+v", code, rep.Total)
	}
}

// toolsRun runs the agents mode and returns its report.
func toolsRun(t *testing.T, extra ...string) agents.Report {
	t.Helper()
	base := startWWZWith(t, agentsCfg)
	args := append([]string{
		"agents", "--target", base, "--agents", "3", "--turns", "3", "--profile", "coder", "--seed", "11",
		"--output", "json", "--progress", "0", "--tool-prob", "60", "--max-tool-rounds", "40",
	}, extra...)
	code, out, errOut := runArgs(t, context.Background(), args...)
	rep := agentsJSON(t, out)
	if code != 0 || rep.Total.ToolCalls == 0 {
		t.Fatalf("%d %s %+v", code, errOut, rep.Total)
	}
	return rep
}

// TestAgentsToolsURLRealServer (spec 015): --tools-url against the real
// wwz-tools binary yields the same tools block as the in-process run with
// the same seed.
func TestAgentsToolsURLRealServer(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found")
	}
	bin := filepath.Join(t.TempDir(), "wwz-tools")
	build := exec.Command(goBin, "build", "-o", bin, "github.com/patrickbrandao/white-wizard/cmd/wwz-tools") //nolint:gosec // G204: fixed arguments
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wwz-tools: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	tcfg := zeroTools(t)
	srv := exec.Command(bin, "serve", "--listen", addr, "--config", tcfg, "--seed", "11") //nolint:gosec // G204: binary built above
	srv.Env = append(os.Environ(), "WWZ_TOOLS_TOKEN=")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
	})
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("wwz-tools not listening: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	local := toolsRun(t, "--tools-config", tcfg)
	remote := toolsRun(t, "--tools-url", "http://"+addr)
	if local.Config.Tools != agents.ToolsInProcess || remote.Config.Tools != "http://"+addr {
		t.Fatalf("config.tools %q / %q", local.Config.Tools, remote.Config.Tools)
	}
	if remote.Total.ToolCalls != local.Total.ToolCalls || !maps.Equal(remote.Total.Tools.CallsByName, local.Total.Tools.CallsByName) {
		t.Fatalf("calls differ:\nlocal  %+v\nremote %+v", local.Total.Tools, remote.Total.Tools)
	}
	if !maps.Equal(remote.Total.Tools.Outcomes, local.Total.Tools.Outcomes) {
		t.Fatalf("outcomes differ:\nlocal  %+v\nremote %+v", local.Total.Tools, remote.Total.Tools)
	}
}
