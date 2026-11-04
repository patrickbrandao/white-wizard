package engine_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// benchReqCfg has no latency and no limit that a tight loop would hit, so
// the benchmark measures the engine's own cost per request (task 268).
const benchReqCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`

// benchRequest runs b.N whole requests through Engine.Handle with a fresh
// MemWriter each time and reports the cost per request.
func benchRequest(b *testing.B, drvName, path, body string, headers driver.MapHeaders) {
	cfg, err := config.Parse([]byte(benchReqCfg))
	if err != nil {
		b.Fatal(err)
	}
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	drv, err := driver.New(drvName, nil)
	if err != nil {
		b.Fatal(err)
	}
	var kind driver.Kind
	for _, rt := range drv.Routes() {
		if rt.Path == path {
			kind = rt.Kind
		}
	}
	in := engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: headers, Body: []byte(body), ClientIP: "127.0.0.1"}
	ctx := context.Background()
	w := core.NewMemWriter()
	e.Handle(ctx, in, w)
	if w.StatusCode() != 200 {
		b.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.Handle(ctx, in, core.NewMemWriter())
	}
}

func BenchmarkRequestOpenAI(b *testing.B) {
	benchRequest(b, "openai", "/v1/chat/completions",
		`{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
		driver.MapHeaders{"authorization": "Bearer wwz-key-01"})
}

func BenchmarkRequestOpenAIStream(b *testing.B) {
	benchRequest(b, "openai", "/v1/chat/completions",
		`{"model":"wwz-fast","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		driver.MapHeaders{"authorization": "Bearer wwz-key-01"})
}

func BenchmarkRequestAnthropicStream(b *testing.B) {
	benchRequest(b, "anthropic", "/v1/messages",
		`{"model":"wwz-fast","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		driver.MapHeaders{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"})
}
