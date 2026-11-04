package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

const covHandleCfg = `
server: {seed: 5, max_request_body_mb: 1}
profiles:
  latency:
    default: {ttft: 0ms, itl: 0ms, clock: logical}
    held: {ttft: 250ms, itl: 0ms, concurrency: 1, max_queue: 1}
models:
  - {id: h-plain}
  - {id: h-notools, capabilities: {tools: false}}
  - {id: h-held, latency: held}
`

// plainWriter is a ResponseWriter that cannot stream.
type plainWriter struct{ core.ResponseWriter }

// downStore fails Check.
type downStore struct{ limits.Store }

func (downStore) Check(context.Context, limits.CheckRequest) (limits.Decision, error) {
	return limits.Decision{}, errors.New("store down")
}

func covInput(t *testing.T, name string, kind driver.Kind, method, path, param, body string, headers map[string]string) (engine.Input, driver.Driver) {
	t.Helper()
	drv, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := driver.MapHeaders{}
	for k, v := range impHeaders(headers) {
		h[strings.ToLower(k)] = v
	}
	h["x-wwz-path"] = path
	return engine.Input{Driver: drv, Kind: kind, Method: method, Path: path, PathParam: param, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, drv
}

func TestCoverageBodyTooLarge(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	big := impBody("h-plain", `,"pad":"`+strings.Repeat("x", 1100*1024)+`"`)
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), big)
	if w.StatusCode() != 413 {
		t.Fatalf("status %d", w.StatusCode())
	}
}

func TestCoverageUnknownModelRoute(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	in, _ := covInput(t, "openai", driver.KindModel, "GET", "/v1/models/nope", "nope", "", nil)
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 404 {
		t.Fatalf("status %d", w.StatusCode())
	}
}

func TestCoverageProfileErrorsAtRequestTime(t *testing.T) {
	for _, field := range []string{"latency", "generation", "errors"} {
		e := newEngine(t, covHandleCfg)
		cfg := e.Config()
		for i := range cfg.Models {
			if cfg.Models[i].ID == "h-plain" {
				switch field {
				case "latency":
					cfg.Models[i].Latency = "ghost"
				case "generation":
					cfg.Models[i].Generation = "ghost"
				default:
					cfg.Models[i].Errors = "ghost"
				}
			}
		}
		e.SetConfig(cfg)
		w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody("h-plain", ""))
		if w.StatusCode() != 500 {
			t.Errorf("%s: status %d: %s", field, w.StatusCode(), w.Body())
		}
	}
}

func TestCoverageNoToolsCapability(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody("h-notools", ","+impTools))
	if w.StatusCode() != 400 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
}

func TestCoverageStoreDown(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	e.Store = downStore{e.Store}
	if w := call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody("h-plain", "")); w.StatusCode() != 200 {
		t.Fatalf("on_down default allows: %d", w.StatusCode())
	}
	e2 := newEngine(t, covHandleCfg+"store: {on_down: deny}\n")
	e2.Store = downStore{e2.Store}
	if w := call(t, e2, "openai", "/v1/chat/completions", impHeaders(nil), impBody("h-plain", "")); w.StatusCode() != 503 {
		t.Fatalf("on_down deny: %d", w.StatusCode())
	}
}

func TestCoverageNonStreamingTransport(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	in, _ := covInput(t, "openai", driver.KindChat, "POST", "/v1/chat/completions", "", impBody("h-plain", `,"stream":true`), nil)
	w := &plainWriter{core.NewMemWriter()}
	e.Handle(context.Background(), in, w)
}

func TestCoverageQueueFull(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(t, e, "openai", "/v1/chat/completions", impHeaders(nil), impBody("h-held", `,"max_tokens":4`)).StatusCode()
		}()
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	close(codes)
	full := 0
	for c := range codes {
		if c == 503 {
			full++
		}
	}
	if full == 0 {
		t.Fatal("no request met the full queue")
	}
}

func TestCoverageQueueCancelled(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
models:
  - id: m-q
    errors: none
    latency: {ttft: 200ms, itl: 0ms}
keys:
  - key: k1
    label: one
    limits: {concurrency: 1, on_limit: queue, queue_timeout: 2s}
`)
	body := `{"model":"m-q","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
	auth := map[string]string{"Authorization": "Bearer k1"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		call(t, e, "openai", "/v1/chat/completions", auth, body)
	}()
	waitFor(t, "first request in flight", func() bool { return e.M.InFlight.Value("openai") == 1 })
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	in, _ := covInput(t, "openai", driver.KindChat, "POST", "/v1/chat/completions", "", body, auth)
	e.Handle(ctx, in, core.NewMemWriter())
	wg.Wait()
}

func TestCoverageOpenRouterVariantMiss(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	or, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	slug := or.(driver.ModelNamer).ModelName(&e.Config().Models[0])
	in, _ := covInput(t, "openrouter", driver.KindChat, "POST", "/api/v1/chat/completions", "", impBody(slug+":free", ""), nil)
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 404 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
}

func TestCoverageScenarioTemplateError(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	s, err := scenario.ParseYAML([]byte("name: cov-badtpl\nsteps:\n  - id: s\n    content: \"{{ nosuchfunc }}\"\n"))
	if err != nil {
		t.Skipf("template validated at load: %v", err)
	}
	e.Scenarios.Add(s)
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "cov-badtpl"}), impBody("h-plain", ""))
	if w.StatusCode() == 200 {
		t.Fatalf("bad template served: %s", w.Body())
	}
}

func TestCoverageGeminiAfterModel(t *testing.T) {
	e := newEngine(t, covHandleCfg)
	in, _ := covInput(t, "gemini", driver.KindGenerate, "POST", "/v1beta/models/nope:generateContent", "", `{"contents":[{"role":"bogus","parts":[{"text":"x"}]}]}`,
		map[string]string{"x-goog-api-key": "wwz-key-01"})
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	if w.StatusCode() != 404 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
}
