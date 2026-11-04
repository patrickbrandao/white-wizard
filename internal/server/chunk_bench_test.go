package server

import (
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/latency"
)

// discardConn is a net.Conn whose writes vanish.
type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error) { return len(p), nil }

// benchChunkPath measures one content chunk end to end on the server side,
// without the request around it: the plan's timer, the driver's chunk, the
// SSE frame and the chunk-encoded write of fastWriter in direct mode.
func benchChunkPath(b *testing.B, name string, endpoint core.Endpoint) {
	drv, err := driver.New(name, nil)
	if err != nil {
		b.Fatal(err)
	}
	fw := &fastWriter{
		ctx: &fasthttp.RequestCtx{}, streaming: true, direct: true, conn: discardConn{}, pend: make([]byte, chunkHead, 1024),
		wake: make(chan struct{}, 1), ready: make(chan struct{}), gone: make(chan struct{}),
	}
	req := &core.Request{Endpoint: endpoint, Model: "wwz-fast", Stream: true}
	resp := &core.Response{ID: "chatcmpl-bench", Model: "wwz-fast", Created: time.Unix(1793457687, 0)}
	sw := drv.StreamWriter(fw, req, &core.RateLimitState{})
	if err := sw.Start(resp); err != nil {
		b.Fatal(err)
	}
	words := []string{" Load", " tests", " measure", " how", " a", " router", " behaves", " <under>", " \"pressure\"", ".\n"}
	plan := &latency.Plan{}
	done := make(chan struct{})
	for _, w := range words { // warm the templates and buffers
		if err := sw.TextDelta(w); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		plan.Wait(time.Nanosecond, done)
		if err := sw.TextDelta(words[i%len(words)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChunkPathOpenAI(b *testing.B)    { benchChunkPath(b, "openai", core.EndpointChat) }
func BenchmarkChunkPathAnthropic(b *testing.B) { benchChunkPath(b, "anthropic", core.EndpointMessages) }

func BenchmarkChunkPathOpenRouter(b *testing.B) { benchChunkPath(b, "openrouter", core.EndpointChat) }
