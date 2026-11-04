package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/dist"
)

// distReport is the aggregated report with instances and conversations
// decoded.
type distReport struct {
	loadgen.Report
	Instances     dist.Instances             `json:"instances"`
	Conversations loadgen.ConversationReport `json:"conversations"`
}

type cmdResult struct {
	code        int
	out, errOut string
}

// fastController shortens the start delay and the grace of the controller
// for the test.
func fastController(t *testing.T) {
	t.Helper()
	delay, grace, hook := controllerStartDelay, controllerGrace, controllerListening
	controllerStartDelay, controllerGrace = 50*time.Millisecond, 200*time.Millisecond
	workerRetryDelay, workerPollInterval = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		controllerStartDelay, controllerGrace, controllerListening = delay, grace, hook
		workerRetryDelay, workerPollInterval = time.Second, time.Second
	})
}

// startController runs the controller with args (plus --listen on a free
// port) in the background; it returns its URL and the channel of its
// outcome.
func startController(t *testing.T, ctx context.Context, stdout io.Writer, args ...string) (string, <-chan cmdResult) {
	t.Helper()
	addr := make(chan string, 1)
	controllerListening = func(a string) { addr <- a }
	done := make(chan cmdResult, 1)
	go func() {
		var o, e bytes.Buffer
		w := stdout
		if w == nil {
			w = &o
		}
		code := run(ctx, append([]string{"controller", "--listen", "127.0.0.1:0", "--progress", "0"}, args...), w, &e)
		done <- cmdResult{code, o.String(), e.String()}
	}()
	select {
	case a := <-addr:
		return "http://" + a, done
	case r := <-done:
		t.Fatalf("controller exited: %+v", r)
	}
	return "", nil
}

// startWorkers runs n workers against url; ids w0, w1, …
func startWorkers(ctx context.Context, url string, n int, extra ...string) <-chan cmdResult {
	out := make(chan cmdResult, n)
	for i := range n {
		go func() {
			var o, e bytes.Buffer
			code := run(ctx, append([]string{"worker", "--controller", url, "--id", fmt.Sprintf("w%d", i)}, extra...), &o, &e)
			out <- cmdResult{code, o.String(), e.String()}
		}()
	}
	return out
}

func waitAll(t *testing.T, ch <-chan cmdResult, n, want int) {
	t.Helper()
	for range n {
		if r := <-ch; r.code != want {
			t.Fatalf("worker exit %d, want %d: %s", r.code, want, r.errOut)
		}
	}
}

// TestDistributedModes is the acceptance of task 045: a controller and 3
// workers in process against wwz in process; the load is split, the
// requests add up and the aggregated report comes out once.
func TestDistributedModes(t *testing.T) {
	fastController(t)
	base := startWWZ(t)
	pf := filepath.Join(t.TempDir(), "prompts")
	_ = os.WriteFile(pf, []byte("alfa\nbeta\n"), 0o600)
	t.Setenv("WWZ_LOAD_TOKEN", "segredo")
	cases := []struct {
		name  string
		args  []string
		check func(*distReport) bool
	}{
		{"concurrency", []string{"--concurrency", "3", "--requests", "30", "--duration", "10s", "--seed", "7", "--prompt-file", pf}, func(r *distReport) bool {
			return r.Total.Requests == 30 && r.Total.Concurrency == 3 && r.Config.Seed == 7
		}},
		{"unbounded", []string{"--concurrency", "4", "--requests", "8", "--duration", "0"}, func(r *distReport) bool {
			return r.Total.Requests == 8 && r.Total.Concurrency == 4
		}},
		{"rps", []string{"--rps", "60", "--duration", "300ms", "--header", "x-a: 1"}, func(r *distReport) bool {
			return r.Total.TargetRPS == 60 && len(r.Steps) == 0 && r.Total.Requests > 0
		}},
		{"sweep", []string{"--sweep", "--sweep-rates", "30,60", "--step-duration", "150ms"}, func(r *distReport) bool {
			return len(r.Steps) == 2 && r.Steps[0].TargetRPS == 30 && r.Steps[1].TargetRPS == 60
		}},
		{"ramp", []string{"--ramp", "30:90:300ms", "--step-duration", "150ms"}, func(r *distReport) bool {
			return len(r.Steps) == 2 && r.Config.Ramp == "30:90:300ms"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			url, done := startController(t, ctx, nil, append([]string{"--instances", "3", "--target", base, "--output", "json"}, tc.args...)...)
			waitAll(t, startWorkers(ctx, url, 3), 3, exitOK)
			r := <-done
			var rep distReport
			if r.code != exitOK || json.Unmarshal([]byte(r.out), &rep) != nil {
				t.Fatalf("%d %s\n%s", r.code, r.errOut, r.out)
			}
			var sum int64
			for _, w := range rep.Instances.Workers {
				sum += w.Requests
			}
			in := rep.Instances
			if in.Expected != 3 || in.Reported != 3 || len(in.Missing) != 0 || len(in.Workers) != 3 || sum != rep.Total.Requests ||
				rep.Config.Instances != 3 || rep.Total.Errors != 0 || !tc.check(&rep) {
				t.Fatalf("report %+v\ninstances %+v", rep.Total, rep.Instances)
			}
		})
	}
}

// TestDistributedScenarioText: conversation mode distributed, text output
// with the conversations and instances blocks; write errors exit 1.
func TestDistributedScenarioText(t *testing.T) {
	fastController(t)
	base := startScenarioWWZ(t)
	ctx := context.Background()
	args := []string{"--instances", "3", "--target", base, "--scenario", fileAgentFlow, "--concurrency", "3", "--requests", "3", "--max-turns", "5", "--token", "t"}
	url, done := startController(t, ctx, nil, args...)
	waitAll(t, startWorkers(ctx, url, 3, "--token", "t"), 3, exitOK)
	r := <-done
	if r.code != exitOK || !strings.Contains(r.out, "conversations: 3 started  3 completed") || !strings.Contains(r.out, "instances: 3 expected  3 reported") {
		t.Fatalf("%d %s\n%s", r.code, r.errOut, r.out)
	}
	for _, marker := range []string{"conversations:", "instances:"} {
		w := &writeFailOn{marker: marker}
		url, done := startController(t, ctx, w, args...)
		waitAll(t, startWorkers(ctx, url, 3, "--token", "t"), 3, exitOK)
		if r := <-done; r.code != exitError {
			t.Fatalf("%s: %d %s", marker, r.code, r.errOut)
		}
	}
}

// TestDistributedMissingWorker: a worker that registers and goes silent and
// one that fails do not hang the controller: the report comes out with who
// answered and exit 4.
func TestDistributedMissingWorker(t *testing.T) {
	fastController(t)
	base := startWWZ(t)
	ctx := context.Background()
	start := time.Now()
	url, done := startController(t, ctx, nil, "--instances", "3", "--target", base, "--rps", "20", "--duration", "100ms", "--timeout", "100ms")
	workers := startWorkers(ctx, url, 1)
	cl := &dist.Client{URL: url}
	var wg sync.WaitGroup
	for _, id := range []string{"ghost", "broken"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := cl.Register(ctx, dist.Registration{ID: id, Version: versionTag()})
			if err != nil {
				t.Error(err)
				return
			}
			if id == "broken" {
				if err := cl.SendResult(ctx, &dist.Result{Index: a.Index, Error: "boom"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	waitAll(t, workers, 1, exitOK)
	r := <-done
	if r.code != exitMissing || !strings.Contains(r.out, "instances: 3 expected  1 reported  missing: ") ||
		!strings.Contains(r.errOut, "worker broken: boom") || !strings.Contains(r.errOut, "missing workers") {
		t.Fatalf("%d %s\n%s", r.code, r.errOut, r.out)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("controller took %v", el)
	}
}

// TestDistributedFailures: register timeout, interruption before and
// during the run, error rate, admin stats and usage errors.
func TestDistributedFailures(t *testing.T) {
	fastController(t)
	base := startWWZ(t)
	ctx := context.Background()

	// Fewer workers than --instances within --register-timeout: exit 1, and
	// the worker that waited loses the controller (exit 1).
	url, done := startController(t, ctx, nil, "--instances", "2", "--concurrency", "2", "--target", base, "--register-timeout", "200ms")
	workers := startWorkers(ctx, url, 1)
	if r := <-done; r.code != exitError || !strings.Contains(r.errOut, "fewer than 2 workers") {
		t.Fatalf("%+v", r)
	}
	waitAll(t, workers, 1, exitError)

	cctx, cancel := context.WithCancel(ctx)
	_, done = startController(t, cctx, nil, "--target", base)
	cancel()
	if r := <-done; r.code != exitError || !strings.Contains(r.errOut, "interrupted before the start") {
		t.Fatalf("%+v", r)
	}

	// Interrupted during the run, a worker that never answers: after
	// --timeout + grace the report comes out with it missing.
	cctx, cancel = context.WithCancel(ctx)
	url, done = startController(t, cctx, nil, "--target", base, "--rps", "20", "--duration", "1m", "--timeout", "100ms", "--output", "json", "--scenario", fileAgentFlow)
	go func() {
		_, _ = (&dist.Client{URL: url}).Register(ctx, dist.Registration{ID: "ghost", Version: versionTag()})
	}()
	waitState(t, url, dist.StateRunning)
	cancel()
	r := <-done
	var rep distReport
	if r.code != exitMissing || json.Unmarshal([]byte(r.out), &rep) != nil || !rep.Interrupted || rep.Instances.Missing[0] != "ghost" || rep.Conversations.Started != 0 {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}

	// Error rate above --fail-on-error-pct (exit 3) with /admin/stats, to
	// a file.
	outFile := filepath.Join(t.TempDir(), "rep.json")
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer admin.Close()
	url, done = startController(t, ctx, nil, "--instances", "2", "--target", base, "--model", "m-trunc", "--concurrency", "2", "--requests", "4",
		"--fail-on-error-pct", "1", "--admin", admin.URL, "--output", "json", "--out", outFile)
	waitAll(t, startWorkers(ctx, url, 2), 2, exitOK)
	r = <-done
	b, _ := os.ReadFile(outFile)
	r.out = string(b)
	if r.code != exitErrorRate || json.Unmarshal([]byte(r.out), &rep) != nil || !strings.Contains(string(rep.AdminStats), `"ok": true`) || rep.Total.Errors != 4 {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}

	// The report cannot be written.
	url, done = startController(t, ctx, failWriter{}, "--target", base, "--requests", "1")
	waitAll(t, startWorkers(ctx, url, 1), 1, exitOK)
	if r := <-done; r.code != exitError {
		t.Fatalf("%+v", r)
	}
}

// TestDistributedStop is the acceptance of ADR 0033: SIGINT on the
// controller during an unbounded run stops the 3 workers, which send their
// partial results; the report is interrupted with all of them.
func TestDistributedStop(t *testing.T) {
	fastController(t)
	base := startWWZ(t)
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)
	url, done := startController(t, cctx, nil, "--instances", "3", "--target", base, "--concurrency", "3", "--duration", "0", "--output", "json")
	workers := startWorkers(ctx, url, 3)
	waitState(t, url, dist.StateRunning)
	time.Sleep(200 * time.Millisecond)
	cancel()
	r := <-done
	var rep distReport
	if r.code != exitOK || json.Unmarshal([]byte(r.out), &rep) != nil || !rep.Interrupted || rep.Instances.Reported != 3 ||
		len(rep.Instances.Missing) != 0 || rep.Total.Requests == 0 {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}
	waitAll(t, workers, 3, exitOK)
}

// TestDistributedSeeds is the acceptance of ADR 0033: with --seed and 3
// workers the x-wwz-seed values are distinct and the same as those of one
// machine with the same total.
func TestDistributedSeeds(t *testing.T) {
	fastController(t)
	ctx := context.Background()
	var mu sync.Mutex
	seeds := map[string]int{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seeds[r.Header.Get("x-wwz-seed")]++
		mu.Unlock()
		http.Error(w, "{}", http.StatusInternalServerError)
	}))
	defer target.Close()
	args := []string{"--target", target.URL, "--concurrency", "3", "--requests", "30", "--seed", "100", "--output", "json"}
	collect := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := seeds
		seeds = map[string]int{}
		return out
	}
	if code, _, errOut := runArgs(t, ctx, append([]string{"run"}, args...)...); code != exitOK {
		t.Fatalf("%d %s", code, errOut)
	}
	single := collect()
	url, done := startController(t, ctx, nil, append([]string{"--instances", "3"}, args...)...)
	waitAll(t, startWorkers(ctx, url, 3), 3, exitOK)
	if r := <-done; r.code != exitOK {
		t.Fatalf("%+v", r)
	}
	distributed := collect()
	for j := 100; j < 130; j++ {
		k := fmt.Sprint(j)
		if single[k] != 1 || distributed[k] != 1 {
			t.Fatalf("seed %s: single %v distributed %v", k, single, distributed)
		}
	}
	if len(single) != 30 || len(distributed) != 30 {
		t.Fatalf("single %v distributed %v", single, distributed)
	}
}

func waitState(t *testing.T, url, state string) {
	t.Helper()
	cl := &dist.Client{URL: url}
	for range 500 {
		if st, err := cl.Status(context.Background()); err == nil && st.State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %s not reached", state)
}

func TestControllerUsage(t *testing.T) {
	controllerListening("127.0.0.1:0") // the default hook does nothing
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("name: x\n"), 0o600)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-h"}, exitOK, ""},
		{[]string{"--nope"}, exitUsage, "-nope"},
		{[]string{"extra"}, exitUsage, `unexpected argument "extra"`},
		{[]string{"--instances", "0"}, exitUsage, "--instances"},
		{[]string{"--register-timeout", "0s"}, exitUsage, "--register-timeout"},
		{[]string{"--rps", "0"}, exitUsage, "--rps"},
		{[]string{"--instances", "3", "--concurrency", "2"}, exitUsage, "concurrency 2 below 3 instances"},
		{[]string{"--prompt-file", filepath.Join(dir, "none")}, exitError, "none"},
		{[]string{"--scenario", filepath.Join(dir, "none.yaml")}, exitError, "none.yaml"},
		{[]string{"--scenario", bad}, exitError, "bad.yaml"},
		{[]string{"--out", filepath.Join(dir, "no", "such")}, exitError, "such"},
		{[]string{"--listen", "256.0.0.1:x"}, exitError, "listen"},
	}
	for _, tc := range cases {
		code, _, errOut := runArgs(t, context.Background(), append([]string{"controller"}, tc.args...)...)
		if code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%v: %d %s", tc.args, code, errOut)
		}
	}
}
