package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamLoadCfg is the verify-heavy streams round (deploy/heavy/rounds/
// streams.yaml): one token per chunk every 20 ms, TTFT zero.
const streamLoadCfg = `
server: {seed: 42, ping_interval: 60s}
profiles:
  latency:
    fast: {itl: {dist: fixed, value: 20ms}, chunk_tokens: 1}
`

// TestStreamLoad is a manual load harness for the streaming data plane (task
// 341), split in two processes so the server's CPU time and profile hold no
// client work. It runs only with WWZ_STREAM_LOAD set:
//
//	WWZ_STREAM_LOAD=server WWZ_STREAM_LOAD_ADDR=:8000 WWZ_STREAM_LOAD_STREAMS=4000 \
//	  WWZ_STREAM_LOAD_SECONDS=15 WWZ_STREAM_LOAD_PROFILE=cpu.out server.test -test.run TestStreamLoad
//	WWZ_STREAM_LOAD=client WWZ_STREAM_LOAD_ADDR=host:8000 WWZ_STREAM_LOAD_STREAMS=4000 \
//	  WWZ_STREAM_LOAD_SECONDS=30 server.test -test.run TestStreamLoad
//
// Both sides wait for the client's streams to open, then measure for
// WWZ_STREAM_LOAD_SECONDS: the server prints its cores, req/s and tokens/s
// per core (and writes a CPU profile), the client its frames/s and ITL.
func TestStreamLoad(t *testing.T) {
	role := os.Getenv("WWZ_STREAM_LOAD")
	if role == "" {
		t.Skip("manual load harness: set WWZ_STREAM_LOAD=server|client")
	}
	addr := os.Getenv("WWZ_STREAM_LOAD_ADDR")
	secs, _ := strconv.Atoi(os.Getenv("WWZ_STREAM_LOAD_SECONDS"))
	if secs <= 0 {
		secs = 30
	}
	n, _ := strconv.Atoi(os.Getenv("WWZ_STREAM_LOAD_STREAMS"))
	switch role {
	case "server":
		streamLoadServer(t, addr, n, time.Duration(secs)*time.Second)
	case "client":
		streamLoadClient(t, addr, n, time.Duration(secs)*time.Second)
	}
}

func streamLoadServer(t *testing.T, addr string, n int, d time.Duration) {
	eng := newEngine(t, streamLoadCfg)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	// Measure once the client says its streams are open: it connects to
	// the next port.
	host, port, _ := net.SplitHostPort(addr)
	pn, _ := strconv.Atoi(port)
	cl, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(pn+1)))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := cl.Accept(); err == nil {
		_ = c.Close()
	}
	_ = cl.Close()
	time.Sleep(3 * time.Second)
	if p := os.Getenv("WWZ_STREAM_LOAD_PROFILE"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		_ = pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}
	cpu0, tok0, req0, t0 := cpuTime(), eng.M.Tokens.Total(), eng.M.Requests.Total(), time.Now()
	time.Sleep(d)
	cpu, el := cpuTime()-cpu0, time.Since(t0).Seconds()
	tok, req := float64(eng.M.Tokens.Total()-tok0)/el, float64(eng.M.Requests.Total()-req0)/el
	cores := cpu.Seconds() / el
	fmt.Printf("server: %.2f cores, %.0f req/s, %.0f tokens/s, %.0f tokens/s/core\n", cores, req, tok, tok/cores)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func streamLoadClient(t *testing.T, addr string, n int, d time.Duration) {
	body := `{"model":"wwz-fast","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	req := []byte(fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: wwz\r\nContent-Type: application/json\r\nAuthorization: Bearer wwz-key-01\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	var frames, streams atomic.Int64
	var itl itlHist
	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := range n {
		c, err := net.Dial("tcp", addr)
		for try := 0; err != nil && try < 50; try++ {
			// A burst overflows the listen backlog (128 on macOS): retry.
			time.Sleep(20 * time.Millisecond)
			c, err = net.Dial("tcp", addr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if i%100 == 99 {
			time.Sleep(10 * time.Millisecond)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			br := bufio.NewReaderSize(c, 4<<10)
			for !stop.Load() {
				if _, err := c.Write(req); err != nil {
					return
				}
				if !readStreamITL(br, &frames, &itl) {
					return
				}
				streams.Add(1)
			}
		}()
	}
	host, port, _ := net.SplitHostPort(addr)
	pn, _ := strconv.Atoi(port)
	if c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(pn+1))); err == nil {
		_ = c.Close()
	}
	time.Sleep(3 * time.Second)
	itl.reset()
	cpu0 := cpuTime()
	f0, s0, t0 := frames.Load(), streams.Load(), time.Now()
	time.Sleep(d)
	f1, s1, el := frames.Load(), streams.Load(), time.Since(t0).Seconds()
	fmt.Printf("client: %d streams, %.2f cores, %.0f frames/s, %.0f streams/s, ITL p50 %.1f p90 %.1f p99 %.1f p999 %.1f ms\n",
		n, (cpuTime()-cpu0).Seconds()/el, float64(f1-f0)/el, float64(s1-s0)/el, itl.q(0.5), itl.q(0.9), itl.q(0.99), itl.q(0.999))
	stop.Store(true)
	wg.Wait()
}

// itlHist counts inter-frame gaps in 0.1 ms buckets up to 1 s.
type itlHist struct{ b [10001]atomic.Int64 }

func (h *itlHist) add(d time.Duration) {
	i := min(int(d/(100*time.Microsecond)), len(h.b)-1)
	h.b[i].Add(1)
}

func (h *itlHist) reset() {
	for i := range h.b {
		h.b[i].Store(0)
	}
}

// q is the q-quantile in ms.
func (h *itlHist) q(q float64) float64 {
	var n int64
	for i := range h.b {
		n += h.b[i].Load()
	}
	want, acc := int64(q*float64(n)), int64(0)
	for i := range h.b {
		if acc += h.b[i].Load(); acc > want {
			return float64(i) / 10
		}
	}
	return 0
}

// readStreamITL is readStream that also records the gap between content
// frames (from the second frame on, as the heavy round measures ITL).
func readStreamITL(br *bufio.Reader, frames *atomic.Int64, h *itlHist) bool {
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return false
		}
		if len(line) == 2 {
			break
		}
	}
	var last time.Time
	nf := 0
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return false
		}
		switch string(line) {
		case "\n":
			frames.Add(1)
			now := time.Now()
			if nf >= 2 && nf < 101 { // content frames after the first
				h.add(now.Sub(last))
			}
			last = now
			nf++
		case "0\r\n":
			_, err := br.ReadSlice('\n')
			return err == nil
		}
	}
}
