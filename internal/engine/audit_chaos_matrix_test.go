package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 384 (audit of spec 009): every error type of the spec reaches the
// client through the three paths (profile, x-wwz-chaos-* headers, admin
// event) in the OpenAI and the Anthropic dialects.

// chaosMatrixCfg gives the model a profile carrying the small delays the
// typed faults need.
func chaosMatrixCfg(rate int, types, extra string) string {
	return fmt.Sprintf(`
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    base: {rate: %d, types: [%s], timeout_delay: 1ms, slow_headers_delay: 1ms, hang_duration: 1ms, after_chunks: 1%s}
models:
  - id: m-chaos
    latency: fast
    errors: base
`, rate, types, extra)
}

type chaosDialect struct {
	name, driver, path string
	headers            map[string]string
	body, streamBody   string
	// typ extracts error.type from a JSON error body.
	typ func(b []byte) string
}

var chaosDialects = []chaosDialect{
	{
		name: "openai", driver: "openai", path: "/v1/chat/completions",
		headers:    map[string]string{"Authorization": "Bearer wwz-key-01"},
		body:       `{"model":"m-chaos","messages":[{"role":"user","content":"hi"}]}`,
		streamBody: `{"model":"m-chaos","stream":true,"messages":[{"role":"user","content":"tell me a long story about chaos"}]}`,
		typ: func(b []byte) string {
			var v struct{ Error struct{ Type string } }
			_ = json.Unmarshal(b, &v)
			return v.Error.Type
		},
	},
	{
		name: "anthropic", driver: "anthropic", path: "/v1/messages",
		headers:    map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"},
		body:       `{"model":"m-chaos","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		streamBody: `{"model":"m-chaos","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"tell me a long story about chaos"}]}`,
		typ: func(b []byte) string {
			var v struct {
				Type  string
				Error struct{ Type string }
			}
			_ = json.Unmarshal(b, &v)
			if v.Type != "error" {
				return "?" + v.Type
			}
			return v.Error.Type
		},
	},
}

// preCase is one error type before the first byte.
type preCase struct {
	name                  string
	oaiStatus, anthStatus int
	oaiType, anthType     string // expected error.type; "" = not checked (non-JSON)
	fault                 core.TransportFault
}

var preCases = []preCase{
	{name: "invalid_request", oaiStatus: 400, anthStatus: 400, oaiType: "invalid_request_error", anthType: "invalid_request_error"},
	{name: "context_length", oaiStatus: 400, anthStatus: 400, oaiType: "invalid_request_error", anthType: "invalid_request_error"},
	{name: "invalid_api_key", oaiStatus: 401, anthStatus: 401, oaiType: "invalid_request_error", anthType: "authentication_error"},
	{name: "permission", oaiStatus: 403, anthStatus: 403, oaiType: "invalid_request_error", anthType: "permission_error"},
	{name: "model_not_found", oaiStatus: 404, anthStatus: 404, oaiType: "invalid_request_error", anthType: "not_found_error"},
	{name: "request_too_large", oaiStatus: 413, anthStatus: 413, anthType: "request_too_large"},
	{name: "rate_limit", oaiStatus: 429, anthStatus: 429, oaiType: "requests", anthType: "rate_limit_error"},
	{name: "quota", oaiStatus: 429, anthStatus: 429, oaiType: "insufficient_quota", anthType: "rate_limit_error"},
	{name: "server_error", oaiStatus: 500, anthStatus: 500, oaiType: "server_error", anthType: "api_error"},
	{name: "bad_gateway", oaiStatus: 502, anthStatus: 502},
	{name: "overloaded", oaiStatus: 503, anthStatus: 529, oaiType: "service_unavailable_error", anthType: "overloaded_error"},
	{name: "gateway_timeout", oaiStatus: 504, anthStatus: 504},
	{name: "connection_reset", fault: core.TransportReset},
	{name: "slow_headers", fault: core.TransportSlowHeaders},
}

func chaosPost(t *testing.T, e *engine.Engine, d chaosDialect, extra map[string]string, body string) *core.MemWriter {
	t.Helper()
	h := map[string]string{}
	for k, v := range d.headers {
		h[k] = v
	}
	for k, v := range extra {
		h[k] = v
	}
	return call(t, e, d.driver, d.path, h, body)
}

func checkPre(t *testing.T, w *core.MemWriter, d chaosDialect, c preCase, path string) {
	t.Helper()
	if c.fault != "" {
		if f := w.Faults(); len(f) != 1 || f[0] != c.fault {
			t.Fatalf("%s: faults %v, want [%s] (status %d)", path, f, c.fault, w.StatusCode())
		}
		return
	}
	want, wantType := c.oaiStatus, c.oaiType
	if d.name == "anthropic" {
		want, wantType = c.anthStatus, c.anthType
	}
	if w.StatusCode() != want {
		t.Fatalf("%s: status %d, want %d: %s", path, w.StatusCode(), want, w.Body())
	}
	if wantType != "" {
		if got := d.typ(w.Body()); got != wantType {
			t.Fatalf("%s: error.type %q, want %q: %s", path, got, wantType, w.Body())
		}
	}
}

func TestAuditChaosPreTypesThreePaths(t *testing.T) {
	for _, d := range chaosDialects {
		for _, c := range preCases {
			t.Run(d.name+"/"+c.name, func(t *testing.T) {
				// (a) profile with rate 100 and types [t].
				e := newEngine(t, chaosMatrixCfg(100, c.name, ""))
				checkPre(t, chaosPost(t, e, d, nil, d.body), d, c, "profile")
				// (b) headers over a profile that never fires by itself.
				e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
				if w := chaosPost(t, e, d, nil, d.body); w.StatusCode() != 200 {
					t.Fatalf("baseline status %d: %s", w.StatusCode(), w.Body())
				}
				hdr := map[string]string{"x-wwz-chaos-rate": "100", "x-wwz-chaos-types": c.name}
				checkPre(t, chaosPost(t, e, d, hdr, d.body), d, c, "header")
				// (c) admin event (POST /admin/chaos) carrying the type.
				e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
				spec := chaos.EventSpec{Profile: &config.ErrorProfile{Rate: 100, Types: []string{c.name}, SlowHeadersDelay: time.Millisecond}, Duration: time.Minute}
				if _, err := e.Injector.Start(spec, time.Now()); err != nil {
					t.Fatal(err)
				}
				checkPre(t, chaosPost(t, e, d, nil, d.body), d, c, "admin")
			})
		}
	}
}

// content_filter is a successful response the client must handle.
func TestAuditChaosContentFilterThreePaths(t *testing.T) {
	want := map[string]string{"openai": "content_filter", "anthropic": "refusal"}
	for _, d := range chaosDialects {
		t.Run(d.name, func(t *testing.T) {
			check := func(path string, w *core.MemWriter) {
				t.Helper()
				if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), want[d.name]) {
					t.Fatalf("%s: %d %s", path, w.StatusCode(), w.Body())
				}
			}
			e := newEngine(t, chaosMatrixCfg(100, "content_filter", ""))
			check("profile", chaosPost(t, e, d, nil, d.body))
			e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
			check("header", chaosPost(t, e, d, map[string]string{"x-wwz-chaos-rate": "100", "x-wwz-chaos-types": "content_filter"}, d.body))
			e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
			if _, err := e.Injector.Start(chaos.EventSpec{Type: "content_filter", Duration: time.Minute}, time.Now()); err != nil {
				t.Fatal(err)
			}
			check("admin", chaosPost(t, e, d, nil, d.body))
		})
	}
}

var midTypes = []string{"stream_error_event", "stream_truncated", "stream_hang", "stream_malformed", "stream_reset", "stream_slow_tail", "finish_length", "usage_missing"}

func midRecorded(t *testing.T, e *engine.Engine, path, want string) {
	t.Helper()
	recs := e.Recorder.Last(1, nil)
	if len(recs) != 1 || recs[0].MidStream != want {
		t.Fatalf("%s: recorder %+v, want mid_stream %q", path, recs, want)
	}
}

func TestAuditChaosMidStreamTypesThreePaths(t *testing.T) {
	for _, d := range chaosDialects {
		for _, mt := range midTypes {
			t.Run(d.name+"/"+mt, func(t *testing.T) {
				// (a) profile.
				e := newEngine(t, chaosMatrixCfg(0, "rate_limit", ", mid_stream_rate: 100, mid_stream_types: ["+mt+"]"))
				chaosPost(t, e, d, nil, d.streamBody)
				midRecorded(t, e, "profile", mt)
				// (b) headers.
				e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
				chaosPost(t, e, d, map[string]string{"x-wwz-chaos-mid-stream-rate": "100", "x-wwz-chaos-mid-stream-types": mt}, d.streamBody)
				midRecorded(t, e, "header", mt)
				// (c) admin event.
				e = newEngine(t, chaosMatrixCfg(0, "rate_limit", ""))
				spec := chaos.EventSpec{Profile: &config.ErrorProfile{MidStreamRate: 100, MidStreamTypes: []string{mt}, AfterChunks: 1, HangDuration: time.Millisecond}, Duration: time.Minute}
				if _, err := e.Injector.Start(spec, time.Now()); err != nil {
					t.Fatal(err)
				}
				chaosPost(t, e, d, nil, d.streamBody)
				midRecorded(t, e, "admin", mt)
			})
		}
	}
}
