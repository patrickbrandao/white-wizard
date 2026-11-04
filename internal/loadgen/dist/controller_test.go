package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

func newServer(t *testing.T, cfg Config) (*Coordinator, *httptest.Server) {
	t.Helper()
	c := NewCoordinator(cfg)
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return c, srv
}

func okResult(index int, requests int64) *Result {
	run := &loadgen.RunResult{Started: time.Now(), Finished: time.Now(), Total: loadgen.StepResult{Stats: loadgen.NewStats()}}
	run.Total.Stats.Requests = requests
	return &Result{Index: index, Histograms: &Histograms{Run: run}, Report: json.RawMessage(`{}`), LateStartMs: 1.5}
}

// TestCoordinatorFlow: token, long-poll registration, split plans, start
// instant, status states, results and the aggregation with a missing and a
// failed worker.
func TestCoordinatorFlow(t *testing.T) {
	plan := Plan{Load: Load{Mode: loadgen.ModeRPS, RPS: 30, Duration: 200 * time.Millisecond}, DrawSeed: 1}
	c, srv := newServer(t, Config{Instances: 4, Token: "tok", Version: "v1", Plan: plan, StartDelay: 50 * time.Millisecond})
	ctx := context.Background()
	cl := func(tok string) *Client { return &Client{URL: srv.URL + "/", Token: tok} }

	if _, err := cl("bad").Status(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("token: %v", err)
	}
	if _, err := (&Client{URL: srv.URL}).Status(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("no token: %v", err)
	}
	st, err := cl("tok").Status(ctx)
	if err != nil || st.State != StateWaiting {
		t.Fatalf("%v %+v", err, st)
	}
	if _, err := cl("tok").Register(ctx, Registration{ID: "x", Version: "v0"}); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("version: %v", err)
	}
	if _, err := cl("tok").Register(ctx, Registration{Version: "v1"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("no id: %v", err)
	}
	// A worker that leaves before the start frees its slot.
	lctx, cancel := context.WithCancel(ctx)
	left := make(chan error, 1)
	go func() {
		_, err := cl("tok").Register(lctx, Registration{ID: "leaver", Version: "v1"})
		left <- err
	}()
	waitRegistered(t, cl("tok"), 1)
	cancel()
	if err := <-left; err == nil {
		t.Fatal("leaver registered")
	}
	waitRegistered(t, cl("tok"), 0)

	var wg sync.WaitGroup
	got := make([]*Assignment, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := cl("tok").Register(ctx, Registration{ID: fmt.Sprintf("w%d", i), Hostname: "h", Version: "v1"})
			if err != nil {
				t.Error(err)
				return
			}
			got[a.Index] = a
		}()
		waitRegistered(t, cl("tok"), i+1)
		if i == 0 {
			if _, err := cl("tok").Register(ctx, Registration{ID: "w0", Version: "v1"}); err == nil || !strings.Contains(err.Error(), "409") {
				t.Fatalf("duplicate id: %v", err)
			}
		}
	}
	wg.Wait()
	<-c.Ready()
	for i, a := range got {
		if a == nil || a.Instances != 4 || a.Plan.Load.RPS != 7.5 || a.Plan.DrawSeed != 1+int64(i) || !a.StartAt.Equal(c.StartAt()) {
			t.Fatalf("%d: %+v", i, a)
		}
	}
	if _, err := cl("tok").Register(ctx, Registration{ID: "late", Version: "v1"}); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("beyond N: %v", err)
	}
	if st, _ := cl("tok").Status(ctx); st.State != StateRunning || st.Registered != 4 {
		t.Fatalf("%+v", st)
	}

	if err := cl("tok").SendResult(ctx, okResult(0, 10)); err != nil {
		t.Fatal(err)
	}
	if err := cl("tok").SendResult(ctx, okResult(0, 10)); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("duplicate result: %v", err)
	}
	if err := cl("tok").SendResult(ctx, okResult(9, 10)); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("bad index: %v", err)
	}
	if err := cl("tok").SendResult(ctx, &Result{Index: 1}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("invalid: %v", err)
	}
	conv := loadgen.NewConvStats()
	conv.Started = 3
	r1 := okResult(1, 5)
	r1.Histograms.Conversations = conv
	if err := cl("tok").SendResult(ctx, r1); err != nil {
		t.Fatal(err)
	}
	r3 := okResult(3, 1)
	r3.Histograms.Conversations = conv
	if err := cl("tok").SendResult(ctx, r3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(c.StartAt().Add(plan.Load.Expected())))
	if st, _ := cl("tok").Status(ctx); st.State != StateCollecting || st.Results != 3 {
		t.Fatalf("%+v", st)
	}
	run, cs, inst, errs := c.Collect()
	if run.Total.Stats.Requests != 16 || cs.Started != 6 || inst.Expected != 4 || inst.Reported != 3 ||
		len(inst.Missing) != 1 || inst.Missing[0] != "w2" || len(errs) != 0 || inst.Workers[1].ID != "w1" ||
		inst.Workers[1].Requests != 5 || inst.Workers[1].LateStartMs != 1.5 || inst.Workers[0].Hostname != "h" {
		t.Fatalf("%+v %+v %+v %v", run.Total.Stats, cs, inst, errs)
	}
	if err := cl("tok").SendResult(ctx, okResult(2, 1)); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("after collect: %v", err)
	}
	if st, _ := cl("tok").Status(ctx); st.State != StateDone {
		t.Fatalf("%+v", st)
	}
	if _, err := cl("tok").Register(ctx, Registration{ID: "after", Version: "v1"}); err == nil {
		t.Fatal("register after collect")
	}
}

func waitRegistered(t *testing.T, cl *Client, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := cl.Status(context.Background())
		if err == nil && st.Registered == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("registered %+v %v, want %d", st, err, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCoordinatorDoneAndErrors: all results close Done; a worker error is
// missing with its message; results before the start are refused; an
// unbounded plan stays running; the default start delay.
func TestCoordinatorDoneAndErrors(t *testing.T) {
	c, srv := newServer(t, Config{Instances: 2, Version: "v", Plan: Plan{Load: Load{Mode: loadgen.ModeConcurrency, Concurrency: 2}}})
	if c.cfg.StartDelay != DefaultStartDelay {
		t.Fatal(c.cfg.StartDelay)
	}
	ctx := context.Background()
	cl := &Client{URL: srv.URL}
	if err := cl.SendResult(ctx, okResult(0, 1)); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("before ready: %v", err)
	}
	resp, err := http.Post(srv.URL+PathResult, "application/json", strings.NewReader("{"))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%v %v", err, resp)
	}
	_ = resp.Body.Close()
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cl.Register(ctx, Registration{ID: fmt.Sprint(i), Version: "v"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if st, _ := cl.Status(ctx); st.State != StateRunning {
		t.Fatalf("%+v", st)
	}
	c.Stop()
	if st, _ := cl.Status(ctx); st.State != StateStopping {
		t.Fatalf("%+v", st)
	}
	if err := cl.SendResult(ctx, okResult(0, 4)); err != nil {
		t.Fatal(err)
	}
	if err := cl.SendResult(ctx, &Result{Index: 1, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	<-c.Done()
	if st, _ := cl.Status(ctx); st.State != StateDone {
		t.Fatalf("%+v", st)
	}
	run, conv, inst, errs := c.Collect()
	if run.Total.Stats.Requests != 4 || conv != nil || inst.Reported != 1 || len(inst.Missing) != 1 || len(errs) != 1 || !strings.HasSuffix(errs[0], ": boom") {
		t.Fatalf("%+v %+v %v", run.Total.Stats, inst, errs)
	}
}

// TestClientErrors: bad URL, unencodable body, unreachable controller and
// an undecodable response.
func TestClientErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Client{URL: "http://bad\x7f"}).Status(ctx); err == nil {
		t.Fatal("bad url")
	}
	if err := (&Client{URL: "http://127.0.0.1:1"}).SendResult(ctx, &Result{Report: json.RawMessage("{")}); err == nil {
		t.Fatal("marshal")
	}
	if _, err := (&Client{URL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}).Register(ctx, Registration{}); err == nil {
		t.Fatal("unreachable")
	}
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer junk.Close()
	if _, err := (&Client{URL: junk.URL}).Status(ctx); err == nil || !strings.Contains(err.Error(), PathStatus) {
		t.Fatalf("decode: %v", err)
	}
}
