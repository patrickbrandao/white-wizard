package engine_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// waitFor polls cond for up to 3s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func exposition(e *engine.Engine) string {
	var buf bytes.Buffer
	e.M.Registry.Write(&buf)
	return buf.String()
}

// A request admitted while the model's latency concurrency is full waits in
// max_queue: wwz_queue_depth and the vllm:* mirror count it (spec 013).
func TestQueueDepthModelConcurrencyAndVLLMMirror(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
models:
  - id: m-slot
    errors: none
    latency: {ttft: 300ms, itl: 0ms, concurrency: 1, max_queue: 5}
keys:
  - {key: k1, label: one}
`)
	e.M.MirrorVLLM()
	body := `{"model":"m-slot","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
	auth := map[string]string{"Authorization": "Bearer k1"}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		codes[0] = call(t, e, "openai", "/v1/chat/completions", auth, body).StatusCode()
	}()
	waitFor(t, "first request running", func() bool { return e.M.VLLMRunning.Value("m-slot") == 1 })
	wg.Add(1)
	go func() {
		defer wg.Done()
		codes[1] = call(t, e, "openai", "/v1/chat/completions", auth, body).StatusCode()
	}()
	waitFor(t, "second request waiting", func() bool {
		return e.M.QueueDepth.Value("openai") == 1 && e.M.VLLMWaiting.Value("m-slot") == 1
	})
	out := exposition(e)
	for _, want := range []string{
		`wwz_queue_depth{driver="openai"} 1`,
		`vllm:num_requests_running{model_name="m-slot"} 1`,
		`vllm:num_requests_waiting{model_name="m-slot"} 1`,
		`# TYPE vllm:time_to_first_token_seconds histogram`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes %v", codes)
	}
	if q, r, w := e.M.QueueDepth.Value("openai"), e.M.VLLMRunning.Value("m-slot"), e.M.VLLMWaiting.Value("m-slot"); q != 0 || r != 0 || w != 0 {
		t.Fatalf("after: queue %d running %d waiting %d", q, r, w)
	}
	if out := exposition(e); !strings.Contains(out, `vllm:time_to_first_token_seconds_count{model_name="m-slot"} 2`) {
		t.Fatalf("ttft mirror:\n%s", out)
	}
}

// on_limit: queue waits for a concurrency slot: the wait shows in
// wwz_queue_depth; without the mirror nothing vllm:* is published.
func TestQueueDepthOnLimitQueueWithoutMirror(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
models:
  - id: m-q
    errors: none
    latency: {ttft: 150ms, itl: 0ms}
keys:
  - key: k1
    label: one
    limits: {concurrency: 1, on_limit: queue, queue_timeout: 800ms}
`)
	body := `{"model":"m-q","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
	auth := map[string]string{"Authorization": "Bearer k1"}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		codes[0] = call(t, e, "openai", "/v1/chat/completions", auth, body).StatusCode()
	}()
	waitFor(t, "first request in flight", func() bool { return e.M.InFlight.Value("openai") == 1 })
	time.Sleep(20 * time.Millisecond) // let it take the concurrency slot
	wg.Add(1)
	go func() {
		defer wg.Done()
		codes[1] = call(t, e, "openai", "/v1/chat/completions", auth, body).StatusCode()
	}()
	waitFor(t, "second request queued", func() bool { return e.M.QueueDepth.Value("openai") == 1 })
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes %v", codes)
	}
	if q := e.M.QueueDepth.Value("openai"); q != 0 {
		t.Fatalf("queue after: %d", q)
	}
	out := exposition(e)
	if !strings.Contains(out, "# TYPE wwz_queue_depth gauge") || strings.Contains(out, "vllm:") {
		t.Fatalf("exposition:\n%s", out)
	}
}
