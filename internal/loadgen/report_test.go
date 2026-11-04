package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedRun is a deterministic ramp run with two steps.
func fixedRun() *RunResult {
	t0 := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	step := func(i int, target float64, scale time.Duration, errs map[string]int64) StepResult {
		s := NewStats()
		for k := range 10 {
			d := time.Duration(k+1) * scale
			s.Add(&Result{TTFT: d, E2E: 4 * d, ITL: []time.Duration{d / 2, d / 4}, InputTokens: 10, OutputTokens: 20})
			s.ScheduleLag.Record(time.Duration(k) * time.Microsecond)
		}
		for typ, n := range errs {
			for range n {
				s.Add(&Result{Err: typ})
			}
		}
		s.Dropped = int64(i)
		return StepResult{Index: i, TargetRPS: target, Start: time.Duration(i-1) * 10 * time.Second, Duration: 10 * time.Second, Stats: s}
	}
	s1 := step(1, 150, time.Millisecond, nil)
	s2 := step(2, 350, 2*time.Millisecond, map[string]int64{"http_429/rate_limit_error": 2, "stream_truncated": 1, `weird,"x"`: 1})
	total := StepResult{Duration: 20 * time.Second, Stats: NewStats()}
	total.Stats.Merge(s1.Stats)
	total.Stats.Merge(s2.Stats)
	return &RunResult{Started: t0, Finished: t0.Add(20 * time.Second), Steps: []StepResult{s1, s2}, Total: total}
}

func fixedConfig() ReportConfig {
	return ReportConfig{
		Target: "http://127.0.0.1:8000", Protocol: "openai", Path: "/v1/chat/completions", Model: "wwz-fast",
		Stream: true, MaxTokens: 256, Mode: ModeRamp, Ramp: "100:400:20s", StepDurationMs: 10000, DurationMs: 20000,
		TimeoutMs: 60000, Seed: 42, Instances: 1, Headers: []string{"x-test-id"},
	}
}

const goldenJSON = `{
  "version": 1,
  "tool_version": "v0.1.0",
  "started": "2026-10-31T03:00:00Z",
  "finished": "2026-10-31T03:00:20Z",
  "interrupted": false,
  "config": {
    "target": "http://127.0.0.1:8000",
    "protocol": "openai",
    "path": "/v1/chat/completions",
    "model": "wwz-fast",
    "stream": true,
    "max_tokens": 256,
    "mode": "ramp",
    "rps": 0,
    "concurrency": 0,
    "ramp": "100:400:20s",
    "sweep_rates": [],
    "step_duration_ms": 10000,
    "duration_ms": 20000,
    "requests": 0,
    "warmup_ms": 0,
    "timeout_ms": 60000,
    "seed": 42,
    "scenario": "",
    "instances": 1,
    "headers": [
      "x-test-id"
    ]
  },
  "steps": [
    {
      "step": 1,
      "target_rps": 150,
      "concurrency": 0,
      "start_ms": 0,
      "duration_ms": 10000,
      "requests": 10,
      "ok": 10,
      "errors": 0,
      "dropped": 1,
      "rps": 1,
      "ok_rps": 1,
      "input_tokens": 100,
      "output_tokens": 200,
      "reasoning_tokens": 0,
      "tokens_estimated": false,
      "input_tokens_per_s": 10,
      "output_tokens_per_s": 20,
      "ttft_ms": {
        "count": 10,
        "mean": 5.5,
        "p50": 5.007,
        "p90": 8.991,
        "p99": 10,
        "max": 10
      },
      "itl_ms": {
        "count": 20,
        "mean": 2.063,
        "p50": 1.747,
        "p90": 4.007,
        "p99": 5,
        "max": 5
      },
      "e2e_ms": {
        "count": 10,
        "mean": 22,
        "p50": 20.031,
        "p90": 35.967,
        "p99": 40,
        "max": 40
      },
      "schedule_lag_ms": {
        "count": 10,
        "mean": 0.005,
        "p50": 0.004,
        "p90": 0.008,
        "p99": 0.009,
        "max": 0.009
      },
      "errors_by_type": {}
    },
    {
      "step": 2,
      "target_rps": 350,
      "concurrency": 0,
      "start_ms": 10000,
      "duration_ms": 10000,
      "requests": 14,
      "ok": 10,
      "errors": 4,
      "dropped": 2,
      "rps": 1.4,
      "ok_rps": 1,
      "input_tokens": 100,
      "output_tokens": 200,
      "reasoning_tokens": 0,
      "tokens_estimated": false,
      "input_tokens_per_s": 10,
      "output_tokens_per_s": 20,
      "ttft_ms": {
        "count": 10,
        "mean": 11,
        "p50": 10.015,
        "p90": 17.983,
        "p99": 20,
        "max": 20
      },
      "itl_ms": {
        "count": 20,
        "mean": 4.125,
        "p50": 3.495,
        "p90": 8.015,
        "p99": 10,
        "max": 10
      },
      "e2e_ms": {
        "count": 10,
        "mean": 44,
        "p50": 40.063,
        "p90": 71.935,
        "p99": 80,
        "max": 80
      },
      "schedule_lag_ms": {
        "count": 10,
        "mean": 0.005,
        "p50": 0.004,
        "p90": 0.008,
        "p99": 0.009,
        "max": 0.009
      },
      "errors_by_type": {
        "http_429/rate_limit_error": 2,
        "stream_truncated": 1,
        "weird,\"x\"": 1
      }
    }
  ],
  "total": {
    "step": "total",
    "target_rps": 0,
    "concurrency": 0,
    "start_ms": 0,
    "duration_ms": 20000,
    "requests": 24,
    "ok": 20,
    "errors": 4,
    "dropped": 3,
    "rps": 1.2,
    "ok_rps": 1,
    "input_tokens": 200,
    "output_tokens": 400,
    "reasoning_tokens": 0,
    "tokens_estimated": false,
    "input_tokens_per_s": 10,
    "output_tokens_per_s": 20,
    "ttft_ms": {
      "count": 20,
      "mean": 8.25,
      "p50": 6.991,
      "p90": 16.031,
      "p99": 20,
      "max": 20
    },
    "itl_ms": {
      "count": 40,
      "mean": 3.094,
      "p50": 2.503,
      "p90": 5.999,
      "p99": 10,
      "max": 10
    },
    "e2e_ms": {
      "count": 20,
      "mean": 33,
      "p50": 27.967,
      "p90": 64.127,
      "p99": 80,
      "max": 80
    },
    "schedule_lag_ms": {
      "count": 20,
      "mean": 0.005,
      "p50": 0.004,
      "p90": 0.008,
      "p99": 0.009,
      "max": 0.009
    },
    "errors_by_type": {
      "http_429/rate_limit_error": 2,
      "stream_truncated": 1,
      "weird,\"x\"": 1
    }
  },
  "conversations": null,
  "instances": null,
  "admin_stats": {
    "uptime_s": 1
  }
}
`

const goldenCSV = `step,target_rps,concurrency,start_ms,duration_ms,requests,ok,errors,dropped,rps,output_tokens_per_s,ttft_p50_ms,ttft_p90_ms,ttft_p99_ms,ttft_max_ms,ttft_mean_ms,itl_p50_ms,itl_p90_ms,itl_p99_ms,itl_max_ms,itl_mean_ms,e2e_p50_ms,e2e_p90_ms,e2e_p99_ms,e2e_max_ms,e2e_mean_ms,schedule_lag_p99_ms,error_types
1,150,0,0,10000,10,10,0,1,1,20,5.007,8.991,10,10,5.5,1.747,4.007,5,5,2.063,20.031,35.967,40,40,22,0.009,
2,350,0,10000,10000,14,10,4,2,1.4,20,10.015,17.983,20,20,11,3.495,8.015,10,10,4.125,40.063,71.935,80,80,44,0.009,"http_429/rate_limit_error=2;stream_truncated=1;weird,""x""=1"
total,0,0,0,20000,24,20,4,3,1.2,20,6.991,16.031,20,20,8.25,2.503,5.999,10,10,3.094,27.967,64.127,80,80,33,0.009,"http_429/rate_limit_error=2;stream_truncated=1;weird,""x""=1"
`

func TestReportGolden(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedRun(), "v0.1.0")
	rep.AdminStats = json.RawMessage(`{"uptime_s":1}`)
	var j, c bytes.Buffer
	if err := rep.Write(&j, FormatJSON); err != nil {
		t.Fatal(err)
	}
	if j.String() != goldenJSON {
		t.Errorf("json:\n%s", j.String())
	}
	if err := rep.Write(&c, FormatCSV); err != nil {
		t.Fatal(err)
	}
	if c.String() != goldenCSV {
		t.Errorf("csv:\n%s", c.String())
	}
	if err := rep.Write(&c, "xml"); err == nil {
		t.Fatal("xml accepted")
	}
}

// TestReportNoSteps: --rps/--concurrency have steps: [] and the CSV only
// the total row; nil slices become [].
func TestReportNoSteps(t *testing.T) {
	run := fixedRun()
	run.Steps = nil
	run.Total.Duration = 0
	rep := NewReport(ReportConfig{Mode: ModeRPS, RPS: 10}, run, "dev")
	var j bytes.Buffer
	_ = rep.WriteJSON(&j)
	for _, want := range []string{`"steps": []`, `"sweep_rates": []`, `"headers": []`, `"rps": 0,`, `"admin_stats": null`} {
		if !strings.Contains(j.String(), want) {
			t.Errorf("missing %s in\n%s", want, j.String())
		}
	}
	var c bytes.Buffer
	_ = rep.WriteCSV(&c)
	if lines := strings.Split(strings.TrimSpace(c.String()), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "total,") {
		t.Fatalf("csv %q", c.String())
	}
}

func TestReportText(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedRun(), "v0.1.0")
	rep.Interrupted = true
	var b bytes.Buffer
	if err := rep.Write(&b, FormatText); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"mode ramp 100:400:20s", "seed 42", "(interrupted)", "ttft p50", "total: 24 requests", "errors 4 (16.667%)", "tokens/s: output 20", "schedule lag", "errors by type:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Errors in decreasing order of count.
	if strings.Index(out, "http_429/rate_limit_error") > strings.Index(out, "stream_truncated") {
		t.Errorf("order:\n%s", out)
	}
	for _, c := range []ReportConfig{
		{Mode: ModeRPS, RPS: 2.5},
		{Mode: ModeConcurrency, Concurrency: 3},
		{Mode: ModeSweep, SweepRates: []float64{1, 2}},
	} {
		run := fixedRun()
		run.Steps = nil
		run.Total.Stats = NewStats()
		run.Total.Stats.Add(&Result{TokensEstimated: true})
		b.Reset()
		_ = NewReport(c, run, "dev").WriteText(&b)
		want := map[Mode]string{ModeRPS: "mode rps 2.5", ModeConcurrency: "mode concurrency 3", ModeSweep: "mode sweep 1,2"}[c.Mode]
		if !strings.Contains(b.String(), want) || !strings.Contains(b.String(), "(estimated)") || strings.Contains(b.String(), "errors by type") {
			t.Errorf("%s:\n%s", c.Mode, b.String())
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("disk full")
	}
	f.n--
	return len(p), nil
}

func TestReportWriteErrors(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedRun(), "v")
	for _, f := range []string{FormatText, FormatJSON, FormatCSV} {
		if err := rep.Write(&failWriter{}, f); err == nil {
			t.Errorf("%s: no error", f)
		}
	}
	if err := rep.WriteText(&failWriter{n: 1}); err == nil {
		t.Error("text: second write error lost")
	}
}

func TestReportExceeds(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedRun(), "v") // 4/24 = 16.7 %
	if rep.Exceeds(0) || !rep.Exceeds(10) || rep.Exceeds(20) {
		t.Fatal("exceeds")
	}
	empty := &StepReport{}
	if empty.ErrorPct() != 0 {
		t.Fatal("empty pct")
	}
}

func TestFetchAdminStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		case r.URL.Path != "/admin/stats":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Query().Get("short") != "":
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"a"`))
		case r.URL.Query().Get("x") != "":
			_, _ = w.Write([]byte(`not json`))
		default:
			_, _ = w.Write([]byte(`{"requests":3}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if got := string(FetchAdminStats(ctx, srv.Client(), srv.URL+"/", "tok")); got != `{"requests":3}` {
		t.Fatal(got)
	}
	for _, c := range []struct{ base, token, want string }{
		{srv.URL, "", "HTTP 401"},
		{srv.URL + "/nope", "tok", "HTTP 404"},
		{srv.URL + "/admin/stats?x=1#", "tok", "invalid JSON"},
		{srv.URL + "/admin/stats?short=1#", "tok", "EOF"},
		{"http://127.0.0.1:1", "tok", "connect"},
		{"http://\x7f", "tok", "invalid"},
	} {
		got := FetchAdminStats(ctx, srv.Client(), c.base, c.token)
		var e struct{ Error string }
		if json.Unmarshal(got, &e) != nil || !strings.Contains(e.Error, c.want) {
			t.Errorf("%q: %s", c.base, got)
		}
	}
}
