package engine_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I2: camada de headers x-wwz-chaos-* (spec 009 "Camadas", spec 017 I2,
// ADR 0027): global → modelo → chave → passo → headers → /admin/chaos →
// chaos/next. Cada header vale só para a requisição que o traz.

const i2Cfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    boom: {rate: 100, types: [server_error]}
    always: {rate: 100, types: [bad_gateway]}
models:
  - {id: m-quiet, latency: fast}
  - {id: m-loud, latency: fast, errors: always}
`

// i2Call sends one OpenAI request and returns the status and its record.
func i2Call(t *testing.T, e *engine.Engine, model string, stream bool, headers map[string]string) (int, engine.Record) {
	t.Helper()
	extra := ""
	if stream {
		extra = `,"stream":true`
	}
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(headers), impBody(model, extra))
	recs := e.Recorder.Last(1, nil)
	if len(recs) != 1 {
		t.Fatalf("no record (status %d %s)", w.StatusCode(), w.Body())
	}
	return w.StatusCode(), recs[0]
}

// i2StreamBroken sends a streaming request and reports whether the stream
// carries an in-band error event.
func i2StreamBroken(t *testing.T, e *engine.Engine, headers map[string]string) bool {
	t.Helper()
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(headers), impBody("m-quiet", `,"stream":true`))
	if w.StatusCode() != 200 {
		t.Fatalf("status %d", w.StatusCode())
	}
	return strings.Contains(string(w.Body()), `data: {"error"`)
}

// TestI2ProfileHeader: x-wwz-chaos-profile swaps the resolved profile for
// profiles.errors.<name>, only for that request; an unknown name is ignored.
func TestI2ProfileHeader(t *testing.T) {
	e := newEngine(t, i2Cfg)
	if st, rec := i2Call(t, e, "m-quiet", false, map[string]string{"x-wwz-chaos-profile": "boom"}); st != 500 || rec.Error != "server_error" {
		t.Fatalf("profile header: %d %+v", st, rec)
	}
	if st, rec := i2Call(t, e, "m-quiet", false, nil); st != 200 || rec.Error != "" {
		t.Fatalf("next request kept the header profile: %d %+v", st, rec)
	}
	if st, _ := i2Call(t, e, "m-quiet", false, map[string]string{"x-wwz-chaos-profile": "nope"}); st != 200 {
		t.Fatalf("unknown profile not ignored: %d", st)
	}
	// The header beats the model's own profile.
	if st, rec := i2Call(t, e, "m-loud", false, map[string]string{"x-wwz-chaos-profile": "boom"}); st != 500 || rec.Error != "server_error" {
		t.Fatalf("header should beat model profile: %d %+v", st, rec)
	}
}

// TestI2RateAndTypes: rate and types override the profile fields; unknown
// types are dropped; the next request is untouched.
func TestI2RateAndTypes(t *testing.T) {
	e := newEngine(t, i2Cfg)
	h := map[string]string{"x-wwz-chaos-rate": "100", "x-wwz-chaos-types": " bogus , overloaded"}
	if st, rec := i2Call(t, e, "m-quiet", false, h); st == 200 || rec.Error != "overloaded" {
		t.Fatalf("rate+types: %d %+v", st, rec)
	}
	if st, _ := i2Call(t, e, "m-quiet", false, nil); st != 200 {
		t.Fatalf("next request failed: %d", st)
	}
	// Types alone change what m-loud injects.
	if _, rec := i2Call(t, e, "m-loud", false, map[string]string{"x-wwz-chaos-types": "server_error"}); rec.Error != "server_error" {
		t.Fatalf("types header: %+v", rec)
	}
	// Rate 0 silences m-loud.
	if st, _ := i2Call(t, e, "m-loud", false, map[string]string{"x-wwz-chaos-rate": "0"}); st != 200 {
		t.Fatalf("rate 0: %d", st)
	}
	// The I19 frame-level fault is accepted in types like errors.types.
	if _, rec := i2Call(t, e, "m-quiet", true, map[string]string{"x-wwz-chaos-rate": "100", "x-wwz-chaos-types": "duplicate_frames"}); rec.MidStream != "duplicate_frames" {
		t.Fatalf("duplicate_frames: %+v", rec)
	}
}

// TestI2InvalidIgnored: invalid rates and all-unknown lists are ignored.
func TestI2InvalidIgnored(t *testing.T) {
	e := newEngine(t, i2Cfg)
	for _, v := range []string{"abc", "-1", "100.5", "NaN"} {
		if st, _ := i2Call(t, e, "m-quiet", false, map[string]string{"x-wwz-chaos-rate": v, "x-wwz-chaos-types": "server_error"}); st != 200 {
			t.Fatalf("rate %q not ignored: %d", v, st)
		}
		if i2StreamBroken(t, e, map[string]string{"x-wwz-chaos-mid-stream-rate": v, "x-wwz-chaos-mid-stream-types": "stream_error_event"}) {
			t.Fatalf("mid rate %q not ignored", v)
		}
	}
	if _, rec := i2Call(t, e, "m-loud", false, map[string]string{"x-wwz-chaos-types": "bogus, ,"}); rec.Error != "bad_gateway" {
		t.Fatalf("unknown types not ignored: %+v", rec)
	}
}

// TestI2MidStream: mid-stream rate and types override the profile; unknown
// mid-stream types are dropped.
func TestI2MidStream(t *testing.T) {
	e := newEngine(t, i2Cfg)
	h := map[string]string{"x-wwz-chaos-mid-stream-rate": "100", "x-wwz-chaos-mid-stream-types": "server_error,stream_error_event"}
	if !i2StreamBroken(t, e, h) {
		t.Fatal("mid-stream headers did not break the stream")
	}
	if i2StreamBroken(t, e, nil) {
		t.Fatal("next stream kept the fault")
	}
	// Unknown mid-stream types are dropped; stream_truncated stays.
	h["x-wwz-chaos-mid-stream-types"] = "bogus,stream_truncated"
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(h), impBody("m-quiet", `,"stream":true`))
	if b := string(w.Body()); strings.Contains(b, `data: {"error"`) || strings.Contains(b, "data: [DONE]") {
		t.Fatalf("want a truncated stream without error event:\n%s", b)
	}
}

// TestI2RequestHeadersOff: server.request_headers: false ignores them all.
func TestI2RequestHeadersOff(t *testing.T) {
	e := newEngine(t, "server: {seed: 42, request_headers: false}\n"+i2Cfg[len("\nserver: {seed: 42}\n"):])
	h := map[string]string{
		"x-wwz-chaos-profile": "boom", "x-wwz-chaos-rate": "100", "x-wwz-chaos-types": "server_error",
		"x-wwz-chaos-mid-stream-rate": "100", "x-wwz-chaos-mid-stream-types": "stream_error_event",
	}
	if i2StreamBroken(t, e, h) {
		t.Fatal("mid-stream headers applied with request_headers false")
	}
	if st, rec := i2Call(t, e, "m-quiet", false, h); st != 200 || rec.Error != "" {
		t.Fatalf("headers applied with request_headers false: %d %+v", st, rec)
	}
}

// TestI2NextBeatsHeader: POST /admin/chaos/next wins over the headers.
func TestI2NextBeatsHeader(t *testing.T) {
	e := newEngine(t, i2Cfg)
	if err := e.Injector.ScheduleNext(chaos.Next{TestID: "t-i2", Type: "overloaded"}); err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"X-Test-Id": "t-i2", "x-wwz-chaos-profile": "boom"}
	if _, rec := i2Call(t, e, "m-quiet", false, h); rec.Error != "overloaded" {
		t.Fatalf("chaos/next should win: %+v", rec)
	}
	if _, rec := i2Call(t, e, "m-quiet", false, h); rec.Error != "server_error" {
		t.Fatalf("header after the one-shot: %+v", rec)
	}
}

// TestI2HeaderBeatsStep: an error drawn from the headers wins over the
// scenario step; when the headers draw nothing the step still fails.
func TestI2HeaderBeatsStep(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	h := map[string]string{"x-wwz-scenario": stepErrScenario, "x-wwz-chaos-rate": "100", "x-wwz-chaos-types": "server_error"}
	if st, rec := i2Call(t, e, "wwz-fast", false, h); st != 500 || rec.Error != "server_error" {
		t.Fatalf("header should beat step: %d %+v", st, rec)
	}
	h["x-wwz-chaos-rate"] = "0"
	if st, _ := i2Call(t, e, "wwz-fast", false, h); st != 429 {
		t.Fatalf("step should apply when the header draws nothing: %d", st)
	}
}
