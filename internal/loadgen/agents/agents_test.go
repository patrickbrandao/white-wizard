package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

func TestProfiles(t *testing.T) {
	for _, s := range []string{"coder", "pesquisa", "chat", "mixed"} {
		if p, err := ParseProfile(s); err != nil || string(p) != s {
			t.Fatalf("%s: %v %v", s, p, err)
		}
	}
	if _, err := ParseProfile("all"); err == nil {
		t.Fatal("all accepted")
	}
	want := []Profile{Coder, Pesquisa, Chat, Coder}
	for a, w := range want {
		if got := Mixed.For(a); got != w {
			t.Fatalf("mixed agent %d: %s", a, got)
		}
		if Pesquisa.For(a) != Pesquisa {
			t.Fatal("pesquisa does not rotate")
		}
	}
	if len(declared[Coder]) != 6 || len(declared[Pesquisa]) != 3 || len(declared[Chat]) != 0 {
		t.Fatalf("declared tools: %d %d %d", len(declared[Coder]), len(declared[Pesquisa]), len(declared[Chat]))
	}
	for _, p := range rotation {
		if sc := scripts[p]; sc.system == "" || sc.first == "" || len(sc.next) == 0 {
			t.Fatalf("script %s", p)
		}
	}
}

// validConfig is a configuration that passes Validate.
func validConfig() Config {
	return Config{
		Agents: 1, Profile: Coder, Turns: 1, MaxToolRounds: 1, Sessions: 1, Tools: stubRunner{},
	}
}

func TestValidate(t *testing.T) {
	ok := validConfig()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	ok.RunID = "0a1b2c3d"
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"profile":         func(c *Config) { c.Profile = "x" },
		"agents":          func(c *Config) { c.Agents = 0 },
		"turns":           func(c *Config) { c.Turns = 0 },
		"max tool rounds": func(c *Config) { c.MaxToolRounds = 0 },
		"sessions":        func(c *Config) { c.Sessions = 0 },
		"duration":        func(c *Config) { c.ThinkTime = -1 },
		"retries":         func(c *Config) { c.Retries = -1 },
		"backoff":         func(c *Config) { c.RetryAfterMax = -1 },
		"tool runner":     func(c *Config) { c.Tools = nil },
		"run id":          func(c *Config) { c.RunID = "XYZ" },
	}
	for want, mut := range cases {
		c := validConfig()
		mut(&c)
		err := c.Validate()
		if !errors.Is(err, errConfig) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
		if _, err := Run(context.Background(), nil, c); err == nil {
			t.Errorf("%s: Run accepted", want)
		}
	}
}

func TestRetryable(t *testing.T) {
	yes := []string{
		"http_408", "http_429", "http_429/rate_limit_error", "http_500", "http_503/x", "http_529/overloaded_error",
		"http_599", "connect", "reset", "timeout", "stream_truncated", "stream_error/server_error",
	}
	no := []string{"http_400/invalid_request_error", "http_401", "http_404", "http_600", "http_x", "parse", "canceled", "request", ""}
	for _, c := range yes {
		if !Retryable(c) {
			t.Errorf("%s not retryable", c)
		}
	}
	for _, c := range no {
		if Retryable(c) {
			t.Errorf("%s retryable", c)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ms, ra string
		want   time.Duration
		ok     bool
	}{
		{"1500", "9", 1500 * time.Millisecond, true},
		{" 0.5 ", "", 500 * time.Microsecond, true},
		{"", "2", 2 * time.Second, true},
		{"bad", "1.5", 1500 * time.Millisecond, true},
		{"-1", "", 0, false},
		{"NaN", "x", 0, false},
		{"", now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second, true},
		{"", now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"", "1e30", math.MaxInt64, true},
		{"", "", 0, false},
	}
	for _, c := range cases {
		got, ok := RetryAfter(c.ms, c.ra, now)
		if got != c.want || ok != c.ok {
			t.Errorf("%q %q: %v %v, want %v %v", c.ms, c.ra, got, ok, c.want, c.ok)
		}
	}
}

func TestBackoff(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	if d := Backoff(r, 0, time.Second, 3); d != 0 {
		t.Fatal(d)
	}
	for attempt := range 70 {
		limit := min(time.Second, (100*time.Millisecond)<<min(attempt, 10))
		for range 50 {
			d := Backoff(r, 100*time.Millisecond, time.Second, attempt)
			if d < 0 || d > limit {
				t.Fatalf("attempt %d: %v > %v", attempt, d, limit)
			}
		}
	}
	// Full jitter: the draws spread over the whole interval.
	var lo, hi bool
	for range 200 {
		d := Backoff(r, time.Second, time.Hour, 0)
		lo = lo || d < 300*time.Millisecond
		hi = hi || d > 700*time.Millisecond
	}
	if !lo || !hi {
		t.Fatal("no jitter")
	}
}

func TestRunID(t *testing.T) {
	if a, b := RunID(42), RunID(42); a != b || a == RunID(43) || len(a) != 8 {
		t.Fatal(RunID(42), RunID(43))
	}
	a, b := RunID(0), RunID(0)
	if len(a) != 8 || a == b {
		t.Fatal(a, b)
	}
}

func TestInProcess(t *testing.T) {
	ex, err := tools.New(tools.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := InProcess{Ex: ex}
	ctx := context.Background()
	res, err := r.Call(ctx, ToolRequest{Session: "s1", Seed: 5, Call: loadgen.ToolCall{ID: "c", Name: "list_dir", Arguments: `{}`}})
	if err != nil || res.IsError || res.Outcome != tools.OutcomeSuccess || res.Content == "" {
		t.Fatalf("list_dir: %+v %v", res, err)
	}
	res, err = r.Call(ctx, ToolRequest{Session: "s1", Call: loadgen.ToolCall{Name: "read_file", Arguments: `not json`}})
	if err != nil || !res.IsError || res.Outcome != tools.OutcomeNatural {
		t.Fatalf("bad args: %+v %v", res, err)
	}
	res, err = r.Call(ctx, ToolRequest{Session: "s1", Call: loadgen.ToolCall{Name: "nope"}})
	if err != nil || !res.IsError || res.Outcome != tools.OutcomeNatural || res.Content != `unknown tool: "nope"` {
		t.Fatalf("unknown: %+v %v", res, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Call(cctx, ToolRequest{Session: "s1", Call: loadgen.ToolCall{Name: "list_dir"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

// toolsHandler serves POST /v1/call over ex like cmd/wwz-tools does
// (openapi.yaml): 200 with the result, 504 for timeout, 404 for an unknown
// tool. It records the headers of the last call.
func toolsHandler(ex *tools.Executor, last *http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/call" {
			http.NotFound(w, r)
			return
		}
		if last != nil {
			*last = r.Header.Clone()
		}
		var body struct {
			Session    string          `json:"session"`
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
			ToolCallID string          `json:"tool_call_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var seed int64
		if v := r.Header.Get("x-wwz-seed"); v != "" {
			_ = json.Unmarshal([]byte(v), &seed)
		}
		res, err := ex.Call(r.Context(), tools.CallRequest{
			Session: body.Session, Name: body.Name, Arguments: body.Arguments, ToolCallID: body.ToolCallID, Seed: seed,
			ForceOutcome: tools.Outcome(r.Header.Get("x-wwz-tools-outcome")),
		})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case err != nil:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "not_found", "message": err.Error()}})
		case res.Outcome == tools.OutcomeTimeout:
			w.WriteHeader(http.StatusGatewayTimeout)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "timeout", "message": res.Content}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tool_call_id": res.ToolCallID, "content": res.Content, "is_error": res.IsError,
				"outcome": res.Outcome, "latency_ms": res.Latency.Milliseconds(),
			})
		}
	}
}

func TestRemote(t *testing.T) {
	ex, err := tools.New(tools.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var last http.Header
	srv := httptest.NewServer(toolsHandler(ex, &last))
	defer srv.Close()
	r := Remote{URL: srv.URL + "/", Token: "tok", Client: srv.Client()}
	ctx := context.Background()
	res, err := r.Call(ctx, ToolRequest{Session: "s1", Seed: 9, Call: loadgen.ToolCall{ID: "c1", Name: "list_dir", Arguments: `{}`}})
	if err != nil || res.IsError || res.Outcome != tools.OutcomeSuccess || res.Content == "" {
		t.Fatalf("list_dir: %+v %v", res, err)
	}
	if last.Get("Authorization") != "Bearer tok" || last.Get("x-wwz-seed") != "9" {
		t.Fatalf("headers %v", last)
	}
	res, err = r.Call(ctx, ToolRequest{Session: "s1", Call: loadgen.ToolCall{Name: "nope"}})
	if err != nil || !res.IsError || res.Outcome != tools.OutcomeNatural || res.Content != `unknown tool: "nope"` {
		t.Fatalf("unknown: %+v %v", res, err)
	}
	if last.Get("Authorization") != "Bearer tok" || last.Get("x-wwz-seed") != "" {
		t.Fatalf("headers without seed %v", last)
	}
	noAuth := Remote{URL: srv.URL, Client: srv.Client()}
	if _, err := noAuth.Call(ctx, ToolRequest{Session: "s1", Call: loadgen.ToolCall{Name: "list_dir"}}); err != nil || last.Get("Authorization") != "" {
		t.Fatalf("no token: %v %v", err, last)
	}
}

func TestRemoteStatuses(t *testing.T) {
	cases := []struct {
		name    string
		h       http.HandlerFunc
		want    ToolResult
		wantErr string
	}{
		{"timeout", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, `{"error":{"type":"timeout","message":"tool timed out after 30s"}}`)
		}, ToolResult{Content: "tool timed out after 30s", IsError: true, Outcome: tools.OutcomeTimeout}, ""},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"unauthorized","message":"missing or invalid bearer token"}}`)
		}, ToolResult{}, "HTTP 401: missing or invalid bearer token"},
		{"no error body", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{}`)
		}, ToolResult{}, "HTTP 500: "},
		{"invalid json", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `<html>`) }, ToolResult{}, "HTTP 200: invalid JSON"},
		{"short body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, `{"content":`)
		}, ToolResult{}, "unexpected EOF"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.h)
		res, err := Remote{URL: srv.URL, Client: srv.Client()}.Call(context.Background(), ToolRequest{Session: "s", Call: loadgen.ToolCall{Name: "x"}})
		srv.Close()
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err != nil || res != c.want {
			t.Errorf("%s: %+v %v", c.name, res, err)
		}
	}
	if _, err := (Remote{URL: "http://\x7f", Client: http.DefaultClient}).Call(context.Background(), ToolRequest{}); err == nil {
		t.Error("bad url accepted")
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := (Remote{URL: srv.URL, Client: http.DefaultClient}).Call(context.Background(), ToolRequest{}); err == nil {
		t.Error("closed server accepted")
	}
}

// stubRunner answers every call with res, or fails with err.
type stubRunner struct {
	res ToolResult
	err error
}

func (s stubRunner) Call(context.Context, ToolRequest) (ToolResult, error) { return s.res, s.err }
