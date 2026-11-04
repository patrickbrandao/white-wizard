//go:build !race

package server

import (
	"math"
	"net"
	"runtime"
	"strconv"
	"testing"

	"github.com/valyala/fasthttp"
)

// The hot path does not pay for the strict client verification when it is
// off (spec 020 RNF-01, criterion "Com off, os benchmarks de
// internal/server não mudam alocações por operação"; ADR 0074): the same
// POST /v1/messages, without and with streaming, allocates the same per
// request on an anthropic mount without options, on one with
// client_verification: off spelled out and on one with oauth_key reached
// with an API key. Each mount has its own server on the same root path, so
// the request bytes are identical. Without -race (the detector allocates).

// allocVerifyBase is the configuration shared by the three servers; only
// the options of the anthropic driver differ.
const allocVerifyBase = `
server: {auth: keys, seed: 42}
store: {kind: memory}
keys:
  - {key: wwz-key-01, label: api}
  - {key: wwz-oauth-secret-0001, label: assinante}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 16}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text}
`

// allocConn is the connection of the measured requests: writes vanish
// (the streamed frames in direct mode) and it has addresses; it is not a
// syscall.Conn, so no disconnect watcher runs.
type allocConn struct{ net.Conn }

var allocPeer = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}

func (allocConn) Write(p []byte) (int, error) { return len(p), nil }
func (allocConn) RemoteAddr() net.Addr        { return allocPeer }
func (allocConn) LocalAddr() net.Addr         { return allocPeer }

// allocsPerMessage is the allocations of one POST /v1/messages through
// Handle on a server with the anthropic driver and opts, the streamed body
// drained to the end.
func allocsPerMessage(t *testing.T, opts string, stream bool) float64 {
	t.Helper()
	s, err := New(newEngine(t, allocVerifyBase+"drivers:\n  - {name: anthropic"+opts+"}\n"), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-sonnet-5-5","max_tokens":64,"stream":` + strconv.FormatBool(stream) +
		`,"messages":[{"role":"user","content":"hi"}]}`
	var req fasthttp.Request
	req.Header.SetMethod("POST")
	req.SetRequestURI("/v1/messages")
	req.Header.SetHost("wwz")
	req.Header.SetContentType("application/json")
	req.Header.Set("x-api-key", "wwz-key-01")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.SetBodyString(body)
	var ctx fasthttp.RequestCtx
	once := func() {
		ctx.Init2(allocConn{}, nil, false)
		req.CopyTo(&ctx.Request)
		s.Handle(&ctx)
		if ctx.Response.StatusCode() != 200 {
			t.Fatalf("opts %q stream %v: status %d: %s", opts, stream, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if stream {
			if !ctx.Response.IsBodyStream() {
				t.Fatalf("opts %q: no streamed body", opts)
			}
			if err := ctx.Response.BodyWriteTo(allocConn{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 50 { // warm the pools and the lazily built tables
		once()
	}
	return allocsRounded(1000, once)
}

// allocsRounded is testing.AllocsPerRun rounded to the nearest integer
// instead of truncated: the hot path carries a small amortized remainder
// (slice and map growth, pool refills after a GC; about 0.02 alloc/op
// without streaming, 0.07 with) that makes the truncated mean flip between
// n and n-1 from run to run, while a cost of the verification would be at
// least one whole allocation per request.
func allocsRounded(runs int, f func()) float64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return math.Round(float64(after.Mallocs-before.Mallocs) / float64(runs))
}

func TestClientVerificationOffKeepsAllocs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			base := allocsPerMessage(t, "", stream)
			if base == 0 {
				t.Fatal("measured no allocation: the request did not run")
			}
			for _, opts := range []string{
				", options: {client_verification: off}",
				", options: {oauth_key: assinante}",
			} {
				if got := allocsPerMessage(t, opts, stream); got != base {
					t.Errorf("anthropic%s: %v allocs/op, without options %v", opts, got, base)
				}
			}
			t.Logf("%v allocs/op on the three mounts", base)
		})
	}
}
