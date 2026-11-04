package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// outagesCfg declares windows at every level: root (hours, anthropic only),
// model (seconds) and key profile (minutes).
const outagesCfg = `
server: {seed: 42}
errors:
  outages:
    - {schedule: "0 0 3 * * *", duration: 1h, type: bad_gateway, drivers: [anthropic]}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    keydown:
      rate: 0
      outages:
        - {schedule: "0 */15 * * * *", duration: 2m, type: server_error}
models:
  - id: m-out
    latency: fast
    errors:
      rate: 0
      outages:
        - {schedule: "*/30 * * * * *", duration: 5s}
keys:
  - {key: k-plain, label: plain}
  - {key: k-down, label: down, errors: keydown}
`

// TestErrorsOutages: spec 009 "Janelas de indisponibilidade" — windows in
// seconds, minutes and hours fail their target only inside the window, with
// retry-after up to its end, on an injected clock; the error is counted as
// injected (phase pre) and x-wwz-chaos-* headers do not change a window.
func TestErrorsOutages(t *testing.T) {
	e := newEngine(t, outagesCfg)
	var now time.Time
	e.Now = func() time.Time { return now }
	day := func(clock string) time.Time {
		t0, err := time.Parse(time.RFC3339Nano, "2026-10-31T"+clock+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return t0
	}
	cases := []struct {
		name, clock, drv, key string
		hdr                   map[string]string
		status                int
		kind, retry           string
	}{
		{"seconds window", "01:00:02.300", "openai", "k-plain", nil, 503, "overloaded", "3"},
		{"after seconds window", "01:00:05", "openai", "k-plain", nil, 200, "", ""},
		{"headers do not change a window", "01:00:31", "openai", "k-plain", map[string]string{"x-wwz-chaos-rate": "0", "x-wwz-chaos-types": "rate_limit"}, 503, "overloaded", "4"},
		{"minutes window of the key", "01:16:10", "openai", "k-down", nil, 500, "server_error", ""},
		{"key window is the key's only", "01:16:10", "openai", "k-plain", nil, 200, "", ""},
		{"after minutes window", "01:17:10", "openai", "k-down", nil, 200, "", ""},
		{"hours window of the driver", "03:59:59", "anthropic", "k-plain", nil, 502, "502 Bad Gateway", ""},
		{"driver window spares other drivers", "03:20:10", "openai", "k-plain", nil, 200, "", ""},
		{"after hours window", "04:00:10", "anthropic", "k-plain", nil, 200, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now = day(tc.clock)
			h := map[string]string{"Authorization": "Bearer " + tc.key}
			path, body := "/v1/chat/completions", `{"model":"m-out","messages":[{"role":"user","content":"hi"}]}`
			if tc.drv == "anthropic" {
				h = map[string]string{"x-api-key": tc.key, "anthropic-version": "2023-06-01"}
				path, body = "/v1/messages", `{"model":"m-out","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			}
			for k, v := range tc.hdr {
				h[k] = v
			}
			w := call(t, e, tc.drv, path, h, body)
			if w.StatusCode() != tc.status {
				t.Fatalf("status %d, want %d: %s", w.StatusCode(), tc.status, w.Body())
			}
			if !strings.Contains(string(w.Body()), tc.kind) {
				t.Fatalf("body %s", w.Body())
			}
			if tc.retry != "" && w.Headers()["retry-after"] != tc.retry {
				t.Fatalf("retry-after %q, want %q (headers %v)", w.Headers()["retry-after"], tc.retry, w.Headers())
			}
		})
	}
	out := exposition(e)
	for _, want := range []string{
		`wwz_errors_injected_total{driver="openai",kind="overloaded",phase="pre"} 2`,
		`wwz_errors_injected_total{driver="openai",kind="server_error",phase="pre"} 1`,
		`wwz_errors_injected_total{driver="anthropic",kind="bad_gateway",phase="pre"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// Task 384 (audit of spec 009).

const outagesRetryCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
errors:
  outages:
    - {schedule: "0 0 * * * *", duration: 10m, type: rate_limit, drivers: [openai]}
    - {schedule: "0 0 * * * *", duration: 10m, type: overloaded, drivers: [anthropic]}
models:
  - id: m-out
    latency: fast
`

// TestOutageRetryAfterOnTheWire: a 429 (rate_limit) window on OpenAI and a
// 529 (overloaded) window on Anthropic send retry-after equal to the seconds
// left in the window; a 503 window is covered by TestErrorsOutages.
func TestOutageRetryAfterOnTheWire(t *testing.T) {
	e := newEngine(t, outagesRetryCfg)
	var now time.Time
	e.Now = func() time.Time { return now }
	cases := []struct {
		name, clock, drv string
		status           int
		retry            string
	}{
		{"openai 429 start", "05:00:00", "openai", 429, "600"},
		{"openai 429 mid", "05:02:30", "openai", 429, "450"},
		{"openai 429 last second", "05:09:59", "openai", 429, "1"},
		{"anthropic 529 mid", "05:02:30", "anthropic", 529, "450"},
		{"anthropic 529 last second", "05:09:59", "anthropic", 529, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, _ = time.Parse(time.RFC3339, "2026-10-31T"+tc.clock+"Z")
			h := map[string]string{"Authorization": "Bearer wwz-key-01"}
			path, body := "/v1/chat/completions", `{"model":"m-out","messages":[{"role":"user","content":"hi"}]}`
			if tc.drv == "anthropic" {
				h = map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
				path, body = "/v1/messages", `{"model":"m-out","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			}
			w := call(t, e, tc.drv, path, h, body)
			if w.StatusCode() != tc.status || w.Headers()["retry-after"] != tc.retry {
				t.Fatalf("status %d retry-after %q, want %d %q: %s", w.StatusCode(), w.Headers()["retry-after"], tc.status, tc.retry, w.Body())
			}
		})
	}
	now, _ = time.Parse(time.RFC3339, "2026-10-31T05:10:00Z")
	if w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, `{"model":"m-out","messages":[{"role":"user","content":"hi"}]}`); w.StatusCode() != 200 {
		t.Fatalf("after the window: %d", w.StatusCode())
	}
}

const outagesHeaderCfg = `
server: {seed: 42}
errors:
  outages:
    - {schedule: "0 0 1 * * *", duration: 1h, type: rate_limit}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    viaheader:
      rate: 0
      outages:
        - {schedule: "0 0 2 * * *", duration: 1h, type: server_error}
    shared:
      rate: 0
      outages:
        - {schedule: "0 0 4 * * *", duration: 1h, type: bad_gateway}
models:
  - id: m-out
    latency: fast
    errors: shared
keys:
  - {key: k-shared, label: shared, errors: shared}
  - {key: k-plain, label: plain}
`

// TestOutageProfileHeaderIsFourthLevel: x-wwz-chaos-profile adds the named
// profile's windows to those of the root, model and key (spec 009 "Níveis
// somam"); the same profile reached by model, key and header counts once.
func TestOutageProfileHeaderIsFourthLevel(t *testing.T) {
	e := newEngine(t, outagesHeaderCfg)
	var now time.Time
	e.Now = func() time.Time { return now }
	cases := []struct {
		name, clock, key, profile string
		status                    int
		kind                      string
	}{
		{"header window", "02:30:00", "k-plain", "viaheader", 500, "server_error"},
		{"no header, no window", "02:30:00", "k-plain", "", 200, ""},
		{"root still applies with the header", "01:30:00", "k-plain", "viaheader", 429, "rate_limit"},
		{"model window still applies with the header", "04:30:00", "k-plain", "viaheader", 502, "502 Bad Gateway"},
		{"key window alone", "04:30:00", "k-shared", "", 502, "502 Bad Gateway"},
		{"same profile at model, key and header", "04:30:00", "k-shared", "shared", 502, "502 Bad Gateway"},
		{"header window beyond its hour", "03:30:00", "k-plain", "viaheader", 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, _ = time.Parse(time.RFC3339, "2026-10-31T"+tc.clock+"Z")
			h := map[string]string{"Authorization": "Bearer " + tc.key}
			if tc.profile != "" {
				h["x-wwz-chaos-profile"] = tc.profile
			}
			w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"m-out","messages":[{"role":"user","content":"hi"}]}`)
			if w.StatusCode() != tc.status || !strings.Contains(string(w.Body()), tc.kind) {
				t.Fatalf("status %d, want %d (%s): %s", w.StatusCode(), tc.status, tc.kind, w.Body())
			}
		})
	}
}

// TestOutageScenarioStepBeatsWindow: a scenario step with `error` inside an
// open window returns the step's error, not the window's.
func TestOutageScenarioStepBeatsWindow(t *testing.T) {
	e := newPrecedenceEngine(t, `
server: {seed: 42}
errors:
  outages:
    - {schedule: "0 0 * * * *", duration: 10m, type: server_error}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`)
	now, _ := time.Parse(time.RFC3339, "2026-10-31T05:02:00Z")
	e.Now = func() time.Time { return now }
	h := map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-scenario": stepErrScenario, "x-wwz-conversation-id": "win-1"}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 429 || w.Headers()["x-wwz-step"] != "boom" {
		t.Fatalf("step must win over the window: %d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
	w = call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 500 {
		t.Fatalf("without the scenario the window applies: %d %s", w.StatusCode(), w.Body())
	}
}

// TestSeededRateIsReproducible: with server.seed != 0 and rate 50, two
// executions of the same requests draw the same sequence of errors.
func TestSeededRateIsReproducible(t *testing.T) {
	const cfg = `
server: {seed: 7}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    half: {rate: 50, types: [server_error]}
models:
  - id: m-half
    latency: fast
    errors: half
`
	run := func() string {
		e := newEngine(t, cfg)
		var b strings.Builder
		for i := 0; i < 40; i++ {
			h := map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-conversation-id": fmt.Sprintf("seed-%d", i)}
			w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"m-half","messages":[{"role":"user","content":"hi"}]}`)
			if w.StatusCode() == 200 {
				b.WriteByte('.')
			} else {
				b.WriteByte('x')
			}
		}
		return b.String()
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("seeded runs differ:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "x") || !strings.Contains(a, ".") {
		t.Fatalf("rate 50 must mix successes and errors: %s", a)
	}
}
