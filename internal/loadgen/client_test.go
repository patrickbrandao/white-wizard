package loadgen

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

const wwzCfg = `
server: {seed: 42}
drivers: [{name: openai}, {name: anthropic}]
profiles:
  latency:
    fixed: {ttft: 40ms, itl: 5ms}
    zero: {ttft: 0ms, itl: 0ms}
  generation:
    short:
      output_tokens: {dist: fixed, value: 8}
      reasoning_tokens: {min: 0, max: 0, probability: 0}
      tool_calls: {probability: 0, optional_param_probability: 1}
  errors:
    trunc: {mid_stream_rate: 100, mid_stream_types: [stream_truncated], after_chunks: 2}
    serr: {mid_stream_rate: 100, mid_stream_types: [stream_error_event], after_chunks: 2}
    hang: {mid_stream_rate: 100, mid_stream_types: [stream_hang], after_chunks: 2, hang_duration: 30s}
models:
  - {id: m-fixed, protocols: [openai, anthropic], latency: fixed, generation: short, errors: none}
  - {id: m-zero, protocols: [openai, anthropic], latency: zero, generation: short, errors: none}
  - {id: m-trunc, protocols: [openai, anthropic], latency: zero, generation: short, errors: trunc}
  - {id: m-serr, protocols: [openai, anthropic], latency: zero, generation: short, errors: serr}
  - {id: m-hang, protocols: [openai, anthropic], latency: zero, generation: short, errors: hang}
`

var bg = context.Background()

// startWWZ serves cfg on 127.0.0.1 (engine + internal/server in process)
// and returns the base URL.
func startWWZ(t *testing.T, cfg string) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
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
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

func baseReq(base string, p Protocol, model string, stream bool) *Request {
	return &Request{
		Protocol: p, URL: base + p.DefaultPath(), Key: "wwz-key-01", Model: model, Stream: stream, MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: "olá"}},
	}
}

// TestMeasuresAgainstWWZ: TTFT, ITL and E2E coherent with a fixed latency
// (ttft 40ms, itl 5ms, 8 tokens), both protocols, with and without stream.
func TestMeasuresAgainstWWZ(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	cl := NewClient(Options{Timeout: 5 * time.Second})
	defer cl.CloseIdleConnections()
	const ttft, itl, tokens = 40 * time.Millisecond, 5 * time.Millisecond, 8
	slack := 60 * time.Millisecond // scheduler and timer granularity
	for _, p := range []Protocol{OpenAI, Anthropic} {
		for _, stream := range []bool{true, false} {
			ttftH, itlH, e2eH := hist.New(), hist.New(), hist.New()
			var res Result
			for range 3 {
				cl.DoInto(bg, baseReq(base, p, "m-fixed", stream), &res)
				if !res.OK() {
					t.Fatalf("%s stream=%v: %s %v", p, stream, res.Err, res.Cause)
				}
				ttftH.Record(res.TTFT)
				e2eH.Record(res.E2E)
				for _, d := range res.ITL {
					itlH.Record(d)
				}
			}
			if res.Status != 200 || res.InputTokens == 0 || res.OutputTokens != tokens || res.TokensEstimated || res.Start.IsZero() {
				t.Fatalf("%s stream=%v: %+v", p, stream, res)
			}
			wantFinish := map[Protocol]string{OpenAI: "stop", Anthropic: "end_turn"}[p]
			if res.FinishReason != wantFinish {
				t.Fatalf("finish %q", res.FinishReason)
			}
			minE2E := ttft + (tokens-1)*itl
			if e := e2eH.Quantile(0.5); e < minE2E || e > minE2E+slack {
				t.Errorf("%s stream=%v: e2e p50 %v, want ~%v", p, stream, e, minE2E)
			}
			if !stream {
				if res.TTFT != res.E2E || len(res.ITL) != 0 || res.ContentEvents != 1 {
					t.Errorf("%s non-stream: ttft %v e2e %v itl %v", p, res.TTFT, res.E2E, res.ITL)
				}
				continue
			}
			if f := ttftH.Quantile(0.5); f < ttft || f > ttft+slack {
				t.Errorf("%s: ttft p50 %v, want ~%v", p, f, ttft)
			}
			if itlH.Count() != 3*(tokens-1) || res.ContentEvents != tokens {
				t.Errorf("%s: %d itl samples, %d events", p, itlH.Count(), res.ContentEvents)
			}
			if m := itlH.Quantile(0.5); m < itl-2*time.Millisecond || m > itl+10*time.Millisecond {
				t.Errorf("%s: itl p50 %v, want ~%v", p, m, itl)
			}
		}
	}
}

// TestToolCallsAndConversation: tool calls arrive with id, name and args;
// the captured assistant message and the tool results go back in the next
// turn and the server accepts them.
func TestToolCallsAndConversation(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	cl := NewClient(Options{})
	forced := `[{"name":"read_file","arguments":{"path":"a.go"}},{"name":"write_file","arguments":{"path":"b"}}]`
	for _, p := range []Protocol{OpenAI, Anthropic} {
		for _, stream := range []bool{true, false} {
			r := baseReq(base, p, "m-zero", stream)
			r.Capture = true
			r.Tools = []Tool{{Name: "read_file"}, {Name: "write_file", Description: "writes"}}
			r.Messages = append([]Message{{Role: "system", Content: "você é um agente"}}, r.Messages...)
			r.Headers = []Header{{"x-wwz-tool-calls", forced}}
			res := cl.Do(bg, r)
			if !res.OK() || len(res.ToolCalls) != 2 || res.Message == nil {
				t.Fatalf("%s stream=%v: %+v", p, stream, res)
			}
			want := []ToolCall{{Name: "read_file", Arguments: `{"path":"a.go"}`}, {Name: "write_file", Arguments: `{"path":"b"}`}}
			for i, tc := range res.ToolCalls {
				if tc.ID == "" || tc.Name != want[i].Name || tc.Arguments != want[i].Arguments {
					t.Fatalf("%s stream=%v: tool call %d = %+v", p, stream, i, tc)
				}
			}
			if f := map[Protocol]string{OpenAI: "tool_calls", Anthropic: "tool_use"}[p]; res.FinishReason != f {
				t.Fatalf("finish %q", res.FinishReason)
			}
			// Next turn: assistant as received + one result per call.
			next := baseReq(base, p, "m-zero", stream)
			next.Tools = r.Tools
			next.Messages = append(r.Messages, *res.Message,
				Message{Role: "tool", ToolCallID: res.ToolCalls[0].ID, Content: "conteúdo"},
				Message{Role: "tool", ToolCallID: res.ToolCalls[1].ID, Content: "falhou", IsError: true})
			if res2 := cl.Do(bg, next); !res2.OK() || res2.ContentEvents == 0 {
				t.Fatalf("%s stream=%v turn 2: %s %v", p, stream, res2.Err, res2.Cause)
			}
		}
	}
}

// TestErrorClassesAgainstWWZ uses the chaos of the wwz.
func TestErrorClassesAgainstWWZ(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	cl := NewClient(Options{Timeout: 300 * time.Millisecond})
	cases := []struct {
		p      Protocol
		model  string
		stream bool
		header string
		want   string
	}{
		{Anthropic, "m-zero", true, "rate_limit", "http_429/rate_limit_error"},
		{OpenAI, "m-zero", true, "rate_limit", "http_429/requests"},
		{Anthropic, "m-zero", false, "overloaded", "http_529/overloaded_error"},
		{OpenAI, "m-zero", true, "connection_reset", ErrReset},
		{OpenAI, "m-trunc", true, "", ErrStreamTruncated},
		{Anthropic, "m-trunc", true, "", ErrStreamTruncated},
		{OpenAI, "m-serr", true, "", "stream_error/service_unavailable_error"},
		{Anthropic, "m-serr", true, "", "stream_error/overloaded_error"},
		{OpenAI, "m-hang", true, "", ErrTimeout},
		{Anthropic, "m-hang", true, "", ErrTimeout},
		{Anthropic, "m-zero", false, "delay", ErrTimeout},
	}
	for _, c := range cases {
		r := baseReq(base, c.p, c.model, c.stream)
		switch c.header {
		case "":
		case "delay": // timeout before the response headers
			r.Headers = []Header{{"x-wwz-delay-ms", "1000"}}
		default:
			r.Headers = []Header{{"x-wwz-return-error", c.header}}
		}
		start := time.Now()
		res := cl.Do(bg, r)
		if res.Err != c.want || res.OK() {
			t.Errorf("%s %s %s: got %q (%v), want %q", c.p, c.model, c.header, res.Err, res.Cause, c.want)
		}
		if c.want == ErrTimeout && time.Since(start) > 2*time.Second {
			t.Errorf("timeout took %v", time.Since(start))
		}
	}
	// The client keeps working after timeouts and resets.
	if res := cl.Do(bg, baseReq(base, OpenAI, "m-zero", true)); !res.OK() {
		t.Fatalf("after errors: %s %v", res.Err, res.Cause)
	}
	// 429 from a real limit (rpm) with the provider envelope.
	limited := startWWZ(t, wwzCfg+"keys:\n  - {key: k-rpm, label: rpm, limits: {rpm: 1}}\n")
	r := baseReq(limited, Anthropic, "m-zero", true)
	r.Key = "k-rpm"
	cl.Do(bg, r)
	if res := cl.Do(bg, r); res.Err != "http_429/rate_limit_error" || res.Status != 429 {
		t.Fatalf("rpm: %q %d", res.Err, res.Status)
	}
}

func TestCancelAndDeadline(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	cl := NewClient(Options{Timeout: 10 * time.Second})
	// Cancel in the middle of a hanging stream.
	ctx, cancel := context.WithCancel(bg)
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	res := cl.Do(ctx, baseReq(base, OpenAI, "m-hang", true))
	if res.Err != ErrCanceled || time.Since(start) > 3*time.Second || res.Status != 200 {
		t.Fatalf("cancel: %q %v after %v", res.Err, res.Cause, time.Since(start))
	}
	// Context deadline shorter than the timeout.
	ctx, cancel2 := context.WithTimeout(bg, 100*time.Millisecond)
	defer cancel2()
	if res := cl.Do(ctx, baseReq(base, Anthropic, "m-hang", true)); res.Err != ErrTimeout {
		t.Fatalf("deadline: %q %v", res.Err, res.Cause)
	}
	// Context already done before sending.
	if res := cl.Do(ctx, baseReq(base, OpenAI, "m-zero", true)); res.Err != ErrTimeout || res.Status != 0 {
		t.Fatalf("expired: %q", res.Err)
	}
	done, cancel3 := context.WithCancel(bg)
	cancel3()
	if res := cl.Do(done, baseReq(base, OpenAI, "m-zero", true)); res.Err != ErrCanceled {
		t.Fatalf("canceled: %q", res.Err)
	}
	// A request that finished is not disturbed by a later cancel, and its
	// connection serves the next requests.
	ctx4, cancel4 := context.WithCancel(bg)
	if res := cl.Do(ctx4, baseReq(base, OpenAI, "m-zero", true)); !res.OK() {
		t.Fatalf("ok: %q", res.Err)
	}
	cancel4()
	for range 3 {
		if res := cl.Do(bg, baseReq(base, OpenAI, "m-fixed", true)); !res.OK() {
			t.Fatalf("after stale cancel: %q %v", res.Err, res.Cause)
		}
	}
}

func TestStaleAbortIgnored(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c, _ := track(a, nil)
	tc := c.(*trackedConn)
	seq := tc.current()
	_ = tc.SetWriteDeadline(time.Time{}) // a new request starts on the connection
	tc.abort(seq)                        // stale: must not poison the read
	go func() { _, _ = b.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := tc.Read(buf); err != nil {
		t.Fatalf("stale abort poisoned the connection: %v", err)
	}
	tc.abort(tc.current())
	if _, err := tc.Read(buf); err == nil {
		t.Fatal("abort must fail the read")
	}
	if _, err := track(nil, net.ErrClosed); err == nil {
		t.Fatal("track must pass dial errors")
	}
}

func TestConnectAndRequestErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cl := NewClient(Options{Timeout: time.Second})
	if res := cl.Do(bg, baseReq("http://"+addr, OpenAI, "m", true)); res.Err != ErrConnect {
		t.Fatalf("refused: %q %v", res.Err, res.Cause)
	}
	r := baseReq("http://"+addr, OpenAI, "m", true)
	r.Tools = []Tool{{Name: "x", Schema: json.RawMessage(`{bad`)}}
	if res := cl.Do(bg, r); res.Err != ErrRequest {
		t.Fatalf("bad body: %q", res.Err)
	}
}

// fake serves canned responses for cases the wwz does not produce.
func fake(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeSSE(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, c := range chunks {
		_, _ = io.WriteString(w, c)
		w.(http.Flusher).Flush()
	}
}

// rawChunkedThenReset writes a chunked SSE response with body but without
// the terminating chunk, then resets the connection.
func rawChunkedThenReset(w http.ResponseWriter, body string) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	_, _ = buf.WriteString(strconv.FormatInt(int64(len(body)), 16) + "\r\n" + body + "\r\n")
	_ = buf.Flush()
	time.Sleep(20 * time.Millisecond)
	_ = conn.(*net.TCPConn).SetLinger(0)
	_ = conn.Close()
}

const oaContent = `data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"

func TestErrorClassesFake(t *testing.T) {
	cl := NewClient(Options{Timeout: 500 * time.Millisecond, MaxBody: 64})
	cases := []struct {
		name string
		p    Protocol
		h    http.HandlerFunc
		want string
	}{
		{"plain 502", OpenAI, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad gateway", http.StatusBadGateway) }, "http_502"},
		{"openrouter code", OpenAI, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(402)
			_, _ = io.WriteString(w, `{"error":{"code":402,"message":"no credits"}}`)
		}, "http_402/402"},
		{"invalid json", OpenAI, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"choices":`) }, ErrParse},
		{"not a message", Anthropic, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"type":"x"}`) }, ErrParse},
		{"too large", OpenAI, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[],"pad":"`+strings.Repeat("x", 200)+`"}`)
		}, ErrParse},
		{"bad sse", OpenAI, func(w http.ResponseWriter, _ *http.Request) { writeSSE(w, "data: {nope\n\n") }, ErrParse},
		{"bad sse at eof", Anthropic, func(w http.ResponseWriter, _ *http.Request) { writeSSE(w, "data: {nope") }, ErrParse},
		{"openai error chunk", OpenAI, func(w http.ResponseWriter, _ *http.Request) {
			writeSSE(w, oaContent, `data: {"error":{"type":"server_error"}}`+"\n\n")
		}, "stream_error/server_error"},
		{"anthropic error untyped", Anthropic, func(w http.ResponseWriter, _ *http.Request) { writeSSE(w, "event: error\ndata: {}\n\n") }, "stream_error/unknown"},
		{"truncated", Anthropic, func(w http.ResponseWriter, _ *http.Request) {
			writeSSE(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`+"\n\n")
		}, ErrStreamTruncated},
		{"hang mid stream", OpenAI, func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, oaContent)
			<-r.Context().Done()
		}, ErrTimeout},
		{"reset mid stream", OpenAI, func(w http.ResponseWriter, _ *http.Request) { rawChunkedThenReset(w, oaContent) }, ErrReset},
	}
	for _, c := range cases {
		base := fake(t, c.h)
		res := cl.Do(bg, baseReq(base, c.p, "m", true))
		if res.Err != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, res.Err, res.Cause, c.want)
		}
	}
}

// TestRetryAfterHeaders: an error response keeps retry-after-ms and
// retry-after raw; a success never carries them.
func TestRetryAfterHeaders(t *testing.T) {
	cl := NewClient(Options{Timeout: time.Second})
	base := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after-ms", "1500")
		w.Header().Set("retry-after", "2")
		if r.URL.Query().Get("ok") != "" {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"oi"},"finish_reason":"stop"}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	})
	res := cl.Do(bg, baseReq(base, OpenAI, "m", false))
	if res.Err != "http_429/rate_limit_error" || res.RetryAfterMS != "1500" || res.RetryAfter != "2" {
		t.Fatalf("429: %+v", res)
	}
	r := baseReq(base, OpenAI, "m", false)
	r.URL += "?ok=1"
	if res := cl.Do(bg, r); !res.OK() || res.RetryAfterMS != "" || res.RetryAfter != "" {
		t.Fatalf("200: %+v", res)
	}
}

// TestFakeSuccesses: an empty stream gets TTFT = E2E, a 200 JSON answer to
// a stream request is parsed, missing usage is estimated and a connection
// reset after [DONE] still counts as success.
func TestFakeSuccesses(t *testing.T) {
	cl := NewClient(Options{Timeout: time.Second})
	base := fake(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, ": keep-alive\n\n", `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n", "data: [DONE]\n\n")
	})
	res := cl.Do(bg, baseReq(base, OpenAI, "m", true))
	if !res.OK() || res.ContentEvents != 0 || res.TTFT != res.E2E || !res.TokensEstimated || res.OutputTokens != 0 {
		t.Fatalf("empty stream: %+v", res)
	}
	base = fake(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"oi"},"finish_reason":"stop"}]}`)
	})
	res = cl.Do(bg, baseReq(base, OpenAI, "m", true))
	if !res.OK() || res.ContentEvents != 1 || res.OutputTokens != 1 || !res.TokensEstimated || res.FinishReason != "stop" {
		t.Fatalf("json for stream: %+v", res)
	}
	base = fake(t, func(w http.ResponseWriter, _ *http.Request) { rawChunkedThenReset(w, oaContent+"data: [DONE]\n\n") })
	if res := cl.Do(bg, baseReq(base, OpenAI, "m", true)); !res.OK() || res.ContentEvents != 1 {
		t.Fatalf("done then reset: %q %v", res.Err, res.Cause)
	}
}

func TestCustomDial(t *testing.T) {
	base := startWWZ(t, wwzCfg)
	addr := strings.TrimPrefix(base, "http://")
	var dials atomic.Int32
	cl := NewClient(Options{Dial: func(string) (net.Conn, error) {
		dials.Add(1)
		return net.Dial("tcp", addr)
	}})
	var res Result
	for range 3 {
		cl.DoInto(bg, baseReq("http://wwz.test", Anthropic, "m-zero", true), &res)
		if !res.OK() {
			t.Fatalf("%q %v", res.Err, res.Cause)
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("keep-alive: %d dials", dials.Load())
	}
}
