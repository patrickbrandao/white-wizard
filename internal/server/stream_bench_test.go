package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// benchStreamCfg paces one token per chunk every 200 µs, so the
// benchmark exercises the timers as well as serialization and the transport.
const benchStreamCfg = `
server: {seed: 42, ping_interval: 60s}
profiles:
  latency:
    fast: {ttft: 0ms, itl: {dist: fixed, value: 200us}, chunk_tokens: 1}
`

// benchStreams runs b.N SSE frames through the real data plane over
// loopback TCP: conns clients loop on streaming requests (keep-alive) and
// count the frames they read. It reports allocations per frame (server and
// client together; the client is a raw socket reader that allocates
// nothing per frame).
func benchStreams(b *testing.B, path string, headers map[string]string, body string, conns int) {
	eng := newEngine(b, benchStreamCfg)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		b.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()
	var hb strings.Builder
	fmt.Fprintf(&hb, "POST %s HTTP/1.1\r\nHost: wwz\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, len(body))
	for k, v := range headers {
		fmt.Fprintf(&hb, "%s: %s\r\n", k, v)
	}
	hb.WriteString("\r\n")
	hb.WriteString(body)
	req := []byte(hb.String())

	var frames atomic.Int64
	target := int64(b.N)
	var wg sync.WaitGroup
	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	// Dial one by one: a burst overflows the listen backlog (128 on macOS).
	cs := make([]net.Conn, conns)
	for i := range cs {
		if cs[i], err = net.Dial("tcp", ln.Addr().String()); err != nil {
			b.Fatal(err)
		}
		defer cs[i].Close()
	}
	cpu0 := cpuTime()
	b.ResetTimer()
	for _, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			br := bufio.NewReaderSize(c, 64<<10)
			for frames.Load() < target {
				if _, err := c.Write(req); err != nil {
					b.Error(err)
					return
				}
				if !readStream(br, &frames) {
					b.Error("stream ended early")
					return
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	runtime.ReadMemStats(&ms1)
	n := float64(frames.Load())
	b.ReportMetric(float64(cpuTime()-cpu0)/n, "cpu-ns/frame")
	b.ReportMetric(float64(ms1.Mallocs-ms0.Mallocs)/n, "allocs/frame")
	b.ReportMetric(float64(ms1.TotalAlloc-ms0.TotalAlloc)/n, "B/frame")
}

// readStream reads one chunked SSE response, counting blank lines (frame
// ends; fasthttp writes each flushed frame as one chunk), until the
// terminating zero-length chunk.
func readStream(br *bufio.Reader, frames *atomic.Int64) bool {
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return false
		}
		if len(line) == 2 { // blank line after the headers
			break
		}
	}
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return false
		}
		switch string(line) {
		case "\n":
			frames.Add(1)
		case "0\r\n":
			_, err := br.ReadSlice('\n')
			return err == nil
		}
	}
}

func BenchmarkStreamOpenAI(b *testing.B) {
	benchStreams(b, "/v1/chat/completions", oaiAuth,
		`{"model":"wwz-fast","stream":true,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`, 256)
}

func BenchmarkStreamAnthropic(b *testing.B) {
	benchStreams(b, "/v1/messages", anthAuth,
		`{"model":"wwz-fast","stream":true,"max_tokens":2000,"messages":[{"role":"user","content":"hello"}]}`, 256)
}

// cpuTime is the user + system CPU time of the process.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
