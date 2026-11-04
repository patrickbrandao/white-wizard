package server

import (
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

const slowHeadersBody = `{"model":"m-slow","messages":[{"role":"user","content":"hi"}]}`

func slowHeadersCfg(delay string) string {
	return fastCfg + `  errors:
    slow: {rate: 100, types: [slow_headers]` + delay + `}
models:
  - id: m-slow
    latency: fast
    errors: slow
`
}

// TestSlowHeadersWaitsConfiguredDelay: through the real fasthttp server,
// slow_headers_delay of 300 ms holds the response headers for at least that
// long, then the error is written normally (spec 009, ADR 0058).
func TestSlowHeadersWaitsConfiguredDelay(t *testing.T) {
	h := start(t, slowHeadersCfg(", slow_headers_delay: 300ms"), nil)
	t0 := time.Now()
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, slowHeadersBody)
	if d := time.Since(t0); d < 300*time.Millisecond {
		t.Fatalf("headers arrived after %v, want >= 300ms", d)
	}
	if resp.StatusCode() != fasthttp.StatusInternalServerError {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
}

// TestSlowHeadersDefaultIs15s: without slow_headers_delay the plan is 15 s.
// Only the plan is checked: waiting 15 s (or aborting the wait, which holds
// the server until Shutdown times out) would make the test slow.
func TestSlowHeadersDefaultIs15s(t *testing.T) {
	if e := chaos.BuildError("slow_headers", &config.ErrorProfile{}); e.Transport != core.TransportSlowHeaders || e.TransportDelay != 15*time.Second {
		t.Fatalf("plan: %+v", e)
	}
	if chaos.DefaultSlowHeadersDelay != 15*time.Second {
		t.Fatalf("default %v", chaos.DefaultSlowHeadersDelay)
	}
	cfg, err := config.Parse([]byte(slowHeadersCfg("")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles.Errors["slow"].SlowHeadersDelay; got != 0 {
		t.Fatalf("profile delay %v, want unset (0 = default 15s)", got)
	}
}
