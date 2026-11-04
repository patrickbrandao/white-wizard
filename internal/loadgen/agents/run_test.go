package agents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

const wwzCfg = `
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
  - {id: m, protocols: [openai, anthropic], latency: zero, generation: agent, errors: none}
  - {id: m-429, protocols: [openai, anthropic], latency: zero, generation: agent, errors: limited}
`

// startWWZ serves wwzCfg in process and returns the base URL.
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

func newExecutor(t *testing.T) *tools.Executor {
	t.Helper()
	cfg := tools.DefaultConfig()
	cfg.Latency = tools.Range{}
	cfg.Tools = nil
	ex, err := tools.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func wwzConfig(base string, p loadgen.Protocol, stream bool, runner ToolRunner) Config {
	return Config{
		Template: loadgen.Request{
			Protocol: p, URL: base + p.DefaultPath(), Key: "wwz-key-01", Model: "m", Stream: stream, MaxTokens: 128,
		},
		Agents: 5, Profile: Mixed, Turns: 3, MaxToolRounds: 20, Sessions: 1, Seed: 42, DrawSeed: 42,
		Retries: 2, Backoff: time.Millisecond, BackoffMax: 10 * time.Millisecond, RetryAfterMax: time.Minute, Tools: runner,
	}
}

// TestAgainstWWZ: --agents 5 --turns 3 completes every session with tool
// calls executed, in both protocols, with and without stream.
func TestAgainstWWZ(t *testing.T) {
	base := startWWZ(t)
	cl := loadgen.NewClient(loadgen.Options{Timeout: 10 * time.Second})
	for _, p := range []loadgen.Protocol{loadgen.OpenAI, loadgen.Anthropic} {
		for _, stream := range []bool{true, false} {
			res, err := Run(context.Background(), cl, wwzConfig(base, p, stream, InProcess{Ex: newExecutor(t)}))
			if err != nil {
				t.Fatal(err)
			}
			tot := res.Total
			if res.Interrupted || tot.SessionsStarted != 5 || tot.SessionsCompleted != 5 || tot.Turns != 15 {
				t.Fatalf("%s stream=%v: sessions %+v reasons %v errors %v", p, stream, tot, tot.FailedByReason, tot.Requests.ErrorsByType)
			}
			if tot.ToolCalls == 0 || tot.Requests.Requests <= 15 || tot.Requests.Errors != 0 {
				t.Fatalf("%s: tool calls %d requests %d", p, tot.ToolCalls, tot.Requests.Requests)
			}
			if res.Agents[2].Profile != Chat || res.Agents[2].ToolCalls != 0 || res.Agents[2].Requests.Requests != 3 {
				t.Fatalf("chat agent %+v", res.Agents[2])
			}
			var calls int64
			for _, n := range tot.CallsByName {
				calls += n
			}
			if calls != tot.ToolCalls || tot.TurnE2E.Count() != 15 || tot.SessionE2E.Count() != 5 {
				t.Fatalf("%s: calls %d/%d", p, calls, tot.ToolCalls)
			}
		}
	}
}

// TestToolsInProcessEqualsRemote: the same seed gives the same tools block
// with the executor in process and on a wwz-tools server.
func TestToolsInProcessEqualsRemote(t *testing.T) {
	base := startWWZ(t)
	cl := loadgen.NewClient(loadgen.Options{Timeout: 10 * time.Second})
	srv := httptest.NewServer(toolsHandler(newExecutor(t), nil))
	defer srv.Close()
	blocks := make([]string, 2)
	for i, runner := range []ToolRunner{InProcess{Ex: newExecutor(t)}, Remote{URL: srv.URL, Client: srv.Client()}} {
		res, err := Run(context.Background(), cl, wwzConfig(base, loadgen.OpenAI, true, runner))
		if err != nil {
			t.Fatal(err)
		}
		rep := NewReport(ReportConfig{}, res, "test")
		b, _ := json.Marshal(rep.Total.Tools)
		blocks[i] = string(b)
		if rep.Total.ToolCalls == 0 {
			t.Fatal("no tool calls")
		}
	}
	if blocks[0] != blocks[1] {
		t.Fatalf("in process %s\nremote     %s", blocks[0], blocks[1])
	}
}

// TestRetryAfterAgainstWWZ: chaos rate_limit with retry-after 1s; with
// --retries 1 the agent waits ≥ 1s and counts the retry, with --retries 0
// the error goes to errors_by_type at once.
func TestRetryAfterAgainstWWZ(t *testing.T) {
	base := startWWZ(t)
	cl := loadgen.NewClient(loadgen.Options{Timeout: 10 * time.Second})
	cfg := wwzConfig(base, loadgen.OpenAI, true, stubRunner{})
	cfg.Template.Model, cfg.Agents, cfg.Turns, cfg.Retries = "m-429", 1, 1, 1
	start := time.Now()
	res, err := Run(context.Background(), cl, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tot := res.Total
	if time.Since(start) < time.Second || tot.Retries != 1 || tot.RetryWait.Min() < time.Second || tot.Requests.Requests != 2 {
		t.Fatalf("retry: %v retries %d wait %v requests %d", time.Since(start), tot.Retries, tot.RetryWait.Min(), tot.Requests.Requests)
	}
	if tot.SessionsFailed != 1 || tot.RetriesByType["http_429/requests"] != 1 || tot.FailedByReason["http_429/requests"] != 1 {
		t.Fatalf("classes: %v %v", tot.RetriesByType, tot.FailedByReason)
	}
	cfg.Retries = 0
	start = time.Now()
	if res, err = Run(context.Background(), cl, cfg); err != nil {
		t.Fatal(err)
	}
	tot = res.Total
	if time.Since(start) > 500*time.Millisecond || tot.Retries != 0 || tot.Requests.ErrorsByType["http_429/requests"] != 1 {
		t.Fatalf("no retry: %v %+v", time.Since(start), tot.Requests.ErrorsByType)
	}
}

// fakeTarget answers with resp(n, body) for the n-th request (from 0).
type fakeResp struct {
	status  int
	headers map[string]string
	body    string
	delay   time.Duration
}

func fakeTarget(t *testing.T, resp func(n int, r *http.Request) fakeResp) string {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fr := resp(int(n.Add(1)-1), r)
		time.Sleep(fr.delay)
		for k, v := range fr.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		if fr.status == 0 {
			fr.status = http.StatusOK
		}
		w.WriteHeader(fr.status)
		_, _ = io.WriteString(w, fr.body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	finalBody = `{"choices":[{"message":{"role":"assistant","content":"pronto"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	toolBody  = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"list_dir","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	errBody   = `{"error":{"type":"rate_limit_error","message":"slow down"}}`
)

func fakeConfig(base string) Config {
	return Config{
		Template: loadgen.Request{Protocol: loadgen.OpenAI, URL: base + "/v1/chat/completions", Model: "m", MaxTokens: 16},
		Agents:   1, Profile: Coder, Turns: 1, MaxToolRounds: 4, Sessions: 1, DrawSeed: 1, RetryAfterMax: time.Minute,
		Tools: stubRunner{res: ToolResult{Content: "ok", Outcome: tools.OutcomeSuccess}},
	}
}

var cl = loadgen.NewClient(loadgen.Options{Timeout: 5 * time.Second})

func run(t *testing.T, ctx context.Context, cfg Config) *Result {
	t.Helper()
	res, err := Run(ctx, cl, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestSessionReasons: every way a session ends.
func TestSessionReasons(t *testing.T) {
	cases := []struct {
		name   string
		resp   func(n int) fakeResp
		mut    func(*Config)
		reason string
	}{
		{"no tool calls", func(int) fakeResp {
			return fakeResp{body: `{"choices":[{"message":{"content":"x"},"finish_reason":"tool_calls"}]}`}
		}, nil, loadgen.ReasonNoToolCalls},
		{"content filter", func(int) fakeResp {
			return fakeResp{body: `{"choices":[{"message":{"content":"x"},"finish_reason":"content_filter"}]}`}
		}, nil, "finish_reason:content_filter"},
		{"max tool rounds", func(int) fakeResp { return fakeResp{body: toolBody} }, nil, ReasonMaxToolRounds},
		{"tools unavailable", func(int) fakeResp { return fakeResp{body: toolBody} }, func(c *Config) {
			c.Tools = stubRunner{err: io.ErrUnexpectedEOF}
		}, ReasonTools},
		{"not retryable", func(int) fakeResp {
			return fakeResp{status: 400, body: `{"error":{"type":"invalid_request_error"}}`}
		}, func(c *Config) { c.Retries = 3 }, "http_400/invalid_request_error"},
		{"retry-after above max", func(int) fakeResp {
			return fakeResp{status: 429, headers: map[string]string{"retry-after": "120"}, body: errBody}
		}, func(c *Config) { c.Retries = 3 }, "http_429/rate_limit_error"},
	}
	for _, c := range cases {
		base := fakeTarget(t, func(n int, _ *http.Request) fakeResp { return c.resp(n) })
		cfg := fakeConfig(base)
		if c.mut != nil {
			c.mut(&cfg)
		}
		tot := run(t, context.Background(), cfg).Total
		if tot.SessionsFailed != 1 || tot.FailedByReason[c.reason] != 1 || tot.Retries != 0 {
			t.Errorf("%s: %+v", c.name, tot.FailedByReason)
		}
	}
}

// TestRetries: 429 with retry-after: 1 then success waits ≥ 1s and counts
// one retry; a 503 without retry-after uses the backoff only.
func TestRetries(t *testing.T) {
	base := fakeTarget(t, func(n int, _ *http.Request) fakeResp {
		switch n {
		case 0:
			return fakeResp{status: 429, headers: map[string]string{"retry-after": "1"}, body: errBody}
		case 1:
			return fakeResp{status: 503}
		}
		return fakeResp{body: finalBody}
	})
	cfg := fakeConfig(base)
	cfg.Retries, cfg.Backoff, cfg.BackoffMax = 2, time.Millisecond, 2*time.Millisecond
	start := time.Now()
	res := run(t, context.Background(), cfg)
	tot := res.Total
	if time.Since(start) < time.Second || tot.SessionsCompleted != 1 || tot.Retries != 2 || tot.Requests.Requests != 3 || tot.Requests.OK != 1 {
		t.Fatalf("%v %+v", time.Since(start), tot)
	}
	if tot.RetriesByType["http_429/rate_limit_error"] != 1 || tot.RetriesByType["http_503"] != 1 || tot.Requests.Errors != 0 ||
		tot.RetryWait.Max() < time.Second || tot.Requests.E2E.Count() != 1 {
		t.Fatalf("by type %v errors %v", tot.RetriesByType, tot.Requests.ErrorsByType)
	}
}

// TestHeadersAndSeeds: every request of a session carries the conversation
// id and x-wwz-seed N+g; the user turns follow the profile script.
func TestHeadersAndSeeds(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	var bodies []string
	base := fakeTarget(t, func(_ int, r *http.Request) fakeResp {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		id := r.Header.Get(ConversationHeader)
		seen[id] = append(seen[id], r.Header.Get("x-wwz-seed"))
		bodies = append(bodies, string(b))
		mu.Unlock()
		return fakeResp{body: finalBody}
	})
	cfg := fakeConfig(base)
	cfg.Agents, cfg.Sessions, cfg.Turns, cfg.Seed, cfg.RunID, cfg.ThinkTime = 2, 2, 2, 100, "abcdef01", time.Millisecond
	cfg.Template.Headers = []loadgen.Header{{Key: "x-test-id", Value: "t1"}}
	res := run(t, context.Background(), cfg)
	if res.RunID != "abcdef01" || res.Total.SessionsCompleted != 4 || res.Total.Turns != 8 {
		t.Fatalf("%+v", res.Total)
	}
	want := map[string]string{"abcdef01-a0-s0": "100", "abcdef01-a1-s0": "101", "abcdef01-a0-s1": "102", "abcdef01-a1-s1": "103"}
	for id, seed := range want {
		if got := seen[id]; len(got) != 2 || got[0] != seed || got[1] != seed {
			t.Fatalf("%s: %v (all %v)", id, got, seen)
		}
	}
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, scripts[Coder].first) || !strings.Contains(last, `"name":"run_command"`) || !strings.Contains(last, "pronto") {
		t.Fatalf("body %s", last)
	}
}

// TestDurationProgressSpawn: --duration opens sessions until it elapses,
// the progress callback fires and the agents start spawn-interval apart.
func TestDurationProgressSpawn(t *testing.T) {
	base := fakeTarget(t, func(int, *http.Request) fakeResp { return fakeResp{body: finalBody, delay: 5 * time.Millisecond} })
	cfg := fakeConfig(base)
	var ticks atomic.Int64
	cfg.Agents, cfg.Duration, cfg.SpawnInterval, cfg.Sessions = 2, 150*time.Millisecond, 50*time.Millisecond, 1
	cfg.ProgressEvery, cfg.Progress = 10*time.Millisecond, func(p Progress) {
		if p.Elapsed > 0 {
			ticks.Add(1)
		}
	}
	res := run(t, context.Background(), cfg)
	if res.Interrupted || res.Total.SessionsCompleted < 4 || ticks.Load() == 0 || res.Agents[1].SessionsStarted >= res.Agents[0].SessionsStarted {
		t.Fatalf("sessions %d/%d ticks %d", res.Agents[0].SessionsStarted, res.Agents[1].SessionsStarted, ticks.Load())
	}
}

// TestInterrupt: no session starts after the interruption; a session cut
// between turns, between rounds or in a backoff counts as interrupted.
func TestInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := run(t, ctx, fakeConfig("http://127.0.0.1:1"))
	if !res.Interrupted || res.Total.SessionsStarted != 0 {
		t.Fatalf("before start %+v", res.Total)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cfg := fakeConfig("http://127.0.0.1:1")
	cfg.Agents, cfg.SpawnInterval = 2, time.Hour
	time.AfterFunc(50*time.Millisecond, cancel)
	if res := run(t, ctx, cfg); !res.Interrupted || res.Total.SessionsStarted != 1 {
		t.Fatalf("spawn %+v", res.Total)
	}

	for _, c := range []struct {
		name string
		body string
		mut  func(*Config)
	}{
		{"between turns", finalBody, func(c *Config) { c.Turns, c.Sessions = 2, 2 }},
		{"between rounds", toolBody, nil},
		{"think time", finalBody, func(c *Config) { c.Turns, c.ThinkTime = 2, time.Hour }},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		base := fakeTarget(t, func(int, *http.Request) fakeResp {
			cancel()
			return fakeResp{body: c.body}
		})
		cfg := fakeConfig(base)
		if c.mut != nil {
			c.mut(&cfg)
		}
		res := run(t, ctx, cfg)
		if !res.Interrupted || res.Total.SessionsInterrupted != 1 || res.Total.SessionsStarted != 1 || res.Total.Requests.Requests != 1 {
			t.Errorf("%s: %+v", c.name, res.Total)
		}
	}

	ctx, cancel = context.WithCancel(context.Background())
	base := fakeTarget(t, func(int, *http.Request) fakeResp {
		return fakeResp{status: 429, headers: map[string]string{"retry-after-ms": "60000"}, body: errBody}
	})
	cfg = fakeConfig(base)
	cfg.Retries = 3
	time.AfterFunc(100*time.Millisecond, cancel)
	res = run(t, ctx, cfg)
	tot := res.Total
	if !res.Interrupted || tot.SessionsInterrupted != 1 || tot.Retries != 0 || tot.Requests.Errors != 1 {
		t.Fatalf("backoff %+v", tot)
	}
}
