package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/dist"
)

// fakeController hands out a fixed assignment and records the results,
// answering POST /v1/result with status.
type fakeController struct {
	mu      sync.Mutex
	results []dist.Result
	status  int
}

func (f *fakeController) start(t *testing.T, a dist.Assignment) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+dist.PathRegister, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(a)
	})
	mux.HandleFunc("POST "+dist.PathResult, func(w http.ResponseWriter, r *http.Request) {
		var res dist.Result
		_ = json.NewDecoder(r.Body).Decode(&res)
		f.mu.Lock()
		f.results = append(f.results, res)
		f.mu.Unlock()
		w.WriteHeader(f.status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func assignment(t *testing.T, spec requestSpec, load dist.Load, startAt time.Time) dist.Assignment {
	t.Helper()
	b, _ := json.Marshal(spec)
	return dist.Assignment{Index: 1, Instances: 2, StartAt: startAt, Plan: dist.Plan{Load: load, Seed: 3, DrawSeed: 3, Timeout: 5 * time.Second, Request: b}}
}

func runWorkerAgainst(t *testing.T, ctx context.Context, f *fakeController, a dist.Assignment) (int, string, dist.Result) {
	t.Helper()
	if f.status == 0 {
		f.status = http.StatusNoContent
	}
	f.results = nil
	code, _, errOut := runArgs(t, ctx, "worker", "--controller", f.start(t, a), "--id", "wx")
	f.mu.Lock()
	defer f.mu.Unlock()
	var last dist.Result
	if len(f.results) > 0 {
		last = f.results[len(f.results)-1]
	}
	return code, errOut, last
}

// TestWorkerRuns: a late start is reported, the report and histograms go
// to the controller, the plan carries the request side (scenario inline
// with --max-turns).
func TestWorkerRuns(t *testing.T) {
	ctx := context.Background()
	base := startWWZ(t)
	f := &fakeController{}
	spec := requestSpec{Target: base, Protocol: loadgen.OpenAI, Path: "/v1/chat/completions", Model: defaultModel, Key: defaultKey, Stream: true, MaxTokens: 16, Prompts: []string{"oi"}}
	load := dist.Load{Mode: loadgen.ModeConcurrency, Concurrency: 2, Requests: 6}
	code, errOut, res := runWorkerAgainst(t, ctx, f, assignment(t, spec, load, time.Now().Add(-time.Second)))
	var rep loadgen.Report
	if code != exitOK || res.Index != 1 || res.LateStartMs < 900 || res.Histograms.Run.Total.Stats.Requests != 6 ||
		json.Unmarshal(res.Report, &rep) != nil || rep.Config.Instances != 2 || rep.Config.Seed != 3 || rep.Total.Requests != 6 {
		t.Fatalf("%d %s %+v", code, errOut, res)
	}

	sbase := startScenarioWWZ(t)
	data, err := os.ReadFile(fileAgentFlow)
	if err != nil {
		t.Fatal(err)
	}
	spec.Target, spec.Scenario, spec.ScenarioYAML, spec.MaxTurns = sbase, "file-agent-flow", string(data), 2
	code, errOut, res = runWorkerAgainst(t, ctx, f, assignment(t, spec, dist.Load{Mode: loadgen.ModeConcurrency, Concurrency: 1, Requests: 1}, time.Now()))
	if code != exitOK || res.Histograms.Conversations == nil || res.Histograms.Conversations.MaxTurns != 1 {
		t.Fatalf("%d %s %+v", code, errOut, res.Histograms.Conversations)
	}
}

// TestWorkerFailures: every failure is sent to the controller in "error"
// and the worker exits 1; a controller that refuses the result is retried
// three times.
func TestWorkerFailures(t *testing.T) {
	fastController(t)
	ctx := context.Background()
	spec := requestSpec{Target: "http://127.0.0.1:1", Protocol: loadgen.OpenAI, Path: "/v1/chat/completions", Prompts: []string{"oi"}}
	load := dist.Load{Mode: loadgen.ModeConcurrency, Concurrency: 1, Requests: 1}
	now := time.Now()
	f := &fakeController{}

	bad := assignment(t, spec, load, now)
	bad.Plan.Request = json.RawMessage(`{}`)
	if code, errOut, res := runWorkerAgainst(t, ctx, f, bad); code != exitError || !strings.Contains(res.Error, "invalid plan request") || !strings.Contains(errOut, "invalid plan") {
		t.Fatalf("%d %s %+v", code, errOut, res)
	}
	scn := spec
	scn.Scenario, scn.ScenarioYAML = "bad.yaml", "name: x\n"
	if code, _, res := runWorkerAgainst(t, ctx, f, assignment(t, scn, load, now)); code != exitError || !strings.HasPrefix(res.Error, "bad.yaml: ") {
		t.Fatalf("%d %+v", code, res)
	}
	defer func() { newUnit, runLoadgen = loadgen.NewRequestUnit, loadgen.Run }()
	newUnit = func(*loadgen.Client, loadgen.RequestPlan) (loadgen.Unit, error) { return nil, errors.New("unit boom") }
	if code, _, res := runWorkerAgainst(t, ctx, f, assignment(t, spec, load, now)); code != exitError || res.Error != "unit boom" {
		t.Fatalf("%d %+v", code, res)
	}
	newUnit = loadgen.NewRequestUnit
	runLoadgen = func(context.Context, loadgen.RunConfig, loadgen.Unit) (*loadgen.RunResult, error) {
		return nil, errors.New("run boom")
	}
	if code, _, res := runWorkerAgainst(t, ctx, f, assignment(t, spec, load, now)); code != exitError || res.Error != "run boom" {
		t.Fatalf("%d %+v", code, res)
	}
	runLoadgen = loadgen.Run

	// Interrupted while waiting for start_at: the error still goes out.
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if code, _, res := runWorkerAgainst(t, cctx, f, assignment(t, spec, load, now.Add(time.Hour))); code != exitError || res.Error != "interrupted before start_at" {
		t.Fatalf("%d %+v", code, res)
	}

	// The controller refuses the result: three attempts, exit 1.
	f.status = http.StatusInternalServerError
	code, errOut, _ := runWorkerAgainst(t, ctx, f, assignment(t, spec, load, now))
	if code != exitError || len(f.results) != 3 || !strings.Contains(errOut, "send result") {
		t.Fatalf("%d %s %d", code, errOut, len(f.results))
	}
}

func TestWorkerUsage(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-h"}, exitOK, ""},
		{[]string{"--nope"}, exitUsage, "-nope"},
		{[]string{}, exitUsage, "--controller"},
		{[]string{"--controller", "ftp://x"}, exitUsage, "--controller"},
		{[]string{"--controller", "http://x:1", "extra"}, exitUsage, "no arguments"},
		{[]string{"--controller", "http://127.0.0.1:1"}, exitError, "register"},
	}
	for _, tc := range cases {
		code, _, errOut := runArgs(t, ctx, append([]string{"worker"}, tc.args...)...)
		if code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%v: %d %s", tc.args, code, errOut)
		}
	}
}
