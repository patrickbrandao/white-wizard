package engine_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// TestFinishEventUsageFields: the request event carries the key's account
// and, in fields, reasoning/cached tokens, cost_usd and the tool call names
// that the panel's usage screen reads (asyncapi 0.3.0, task 075). A key
// without account gets the default one (acct-<label>); without a key there is
// no account.
func TestFinishEventUsageFields(t *testing.T) {
	cfg := `
server: {auth: keys}
keys:
  - {key: k-acct, label: with-acct, account: org-carga}
  - {key: k-solo, label: solo}
`
	e := newEngine(t, cfg)
	body := `{"model":"wwz-fast","tool_choice":"required","messages":[{"role":"user","content":"read the file"}],
	  "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]}`
	if w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-acct"}, body); w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	evs, err := e.Store.Events(context.Background(), 1)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	ev := evs[0]
	if ev.Endpoint != "chat" {
		t.Fatalf("event endpoint = %q, want chat", ev.Endpoint)
	}
	if ev.Type != "request" || ev.Account != "org-carga" || ev.Key != "with-acct" {
		t.Fatalf("event: %+v", ev)
	}
	tools, _ := ev.Fields["tool_calls"].([]string)
	if len(tools) == 0 {
		t.Fatalf("tool_calls = %v", ev.Fields["tool_calls"])
	}
	for _, n := range tools {
		if n != "read_file" {
			t.Fatalf("tool_calls = %v, want only read_file", tools)
		}
	}
	if c, _ := ev.Fields["cost_usd"].(float64); c <= 0 {
		t.Fatalf("cost_usd = %v", ev.Fields["cost_usd"])
	}
	for _, k := range []string{"reasoning_tokens", "cached_tokens"} {
		if _, ok := ev.Fields[k].(int); !ok {
			t.Fatalf("fields.%s = %v", k, ev.Fields[k])
		}
	}

	plain := `{"model":"wwz-fast","messages":[{"role":"user","content":"x"}]}`
	if w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-solo"}, plain); w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	evs, _ = e.Store.Events(context.Background(), 1)
	if ev := evs[0]; ev.Account != "acct-solo" || ev.Fields["tool_calls"] != nil {
		t.Fatalf("event with default account, no tools: %+v", ev)
	}

	open := newEngine(t, "server: {auth: none}\n")
	if w := call(t, open, "openai", "/v1/chat/completions", nil, plain); w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	evs, _ = open.Store.Events(context.Background(), 1)
	if ev := evs[0]; ev.Account != "" || ev.Key != "" {
		t.Fatalf("event without key: %+v", ev)
	}
}

// eventFields returns the latest ev:stream event and its fields after a JSON
// round trip (the form the panel reads).
func eventFields(t *testing.T, e *engine.Engine) (limits.Event, map[string]any) {
	t.Helper()
	evs, err := e.Store.Events(context.Background(), 1)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	b, err := json.Marshal(evs[0].Fields)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return evs[0], f
}

// TestFinishEventCostAndBreakdown (ADR 0032, asyncapi 0.4.0, task 105):
// cost_usd is always in a served request, 0 for a free model; with
// usage.extended the event carries input_breakdown with the values of the
// x-wwz-input-breakdown header (also on an error reply and in a stream);
// without usage.extended it is absent; a denial carries no usage field.
func TestFinishEventCostAndBreakdown(t *testing.T) {
	cfg := `
server: {seed: 42}
usage: {extended: %s}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    boom: {rate: 100, types: [bad_gateway]}
models:
  - {id: m-free, latency: fast, errors: none}
  - {id: m-boom, latency: fast, errors: boom}
keys:
  - {key: k-any, label: any}
  - {key: k-one, label: one, limits: {rpm: 1}}
`
	anyKey := map[string]string{"Authorization": "Bearer k-any"}
	on := newEngine(t, strings.Replace(cfg, "%s", "true", 1))
	for _, tc := range []struct {
		name, model, extra string
		status             int
	}{
		{"plain", "m-free", "", 200},
		{"stream", "m-free", `,"stream":true`, 200},
		{"error", "m-boom", "", 502},
	} {
		w := call(t, on, "openai", "/v1/chat/completions", anyKey, impBody(tc.model, tc.extra))
		if w.StatusCode() != tc.status {
			t.Fatalf("%s: status %d: %s", tc.name, w.StatusCode(), w.Body())
		}
		h := w.Headers()["x-wwz-input-breakdown"]
		if h == "" {
			t.Fatalf("%s: no x-wwz-input-breakdown", tc.name)
		}
		var want any
		if err := json.Unmarshal([]byte(h), &want); err != nil {
			t.Fatal(err)
		}
		_, f := eventFields(t, on)
		if !reflect.DeepEqual(f["input_breakdown"], want) {
			t.Fatalf("%s: fields.input_breakdown = %v, header %s", tc.name, f["input_breakdown"], h)
		}
		if c, ok := f["cost_usd"].(float64); !ok || c != 0 {
			t.Fatalf("%s: free model cost_usd = %v (present %v), want 0", tc.name, f["cost_usd"], ok)
		}
	}

	off := newEngine(t, strings.Replace(cfg, "%s", "false", 1))
	if w := call(t, off, "openai", "/v1/chat/completions", anyKey, impBody("m-free", "")); w.StatusCode() != 200 {
		t.Fatalf("status %d", w.StatusCode())
	}
	if _, f := eventFields(t, off); f["input_breakdown"] != nil {
		t.Fatalf("without usage.extended: input_breakdown = %v", f["input_breakdown"])
	}

	// A denial (rpm 1 of key one) carries no usage field, even with the
	// header on the reply.
	auth := map[string]string{"Authorization": "Bearer k-one"}
	_ = call(t, on, "openai", "/v1/chat/completions", auth, impBody("m-free", ""))
	if w := call(t, on, "openai", "/v1/chat/completions", auth, impBody("m-free", "")); w.StatusCode() != 429 {
		t.Fatalf("denial status %d: %s", w.StatusCode(), w.Body())
	}
	ev, f := eventFields(t, on)
	if f["denied"] != true {
		t.Fatalf("not a denial: %+v", ev)
	}
	for _, k := range []string{"reasoning_tokens", "cached_tokens", "cost_usd", "tool_calls", "input_breakdown"} {
		if _, ok := f[k]; ok {
			t.Fatalf("denial carries %s: %v", k, f)
		}
	}
}

// TestCutStreamRecorded (task 105): a stream cut by a mid-stream fault still
// reaches the Recorder (/admin/requests) with mid_stream and the error, and
// the error event goes to ev:stream.
func TestCutStreamRecorded(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: m-quiet, latency: fast, errors: none}
`)
	for _, kind := range []string{"stream_error_event", "stream_truncated"} {
		h := map[string]string{"x-wwz-chaos-mid-stream-rate": "100", "x-wwz-chaos-mid-stream-types": kind}
		before := len(e.Recorder.Last(100, nil))
		w := call(t, e, "openai", "/v1/chat/completions", impHeaders(h), impBody("m-quiet", `,"stream":true`))
		if w.StatusCode() != 200 {
			t.Fatalf("%s: status %d", kind, w.StatusCode())
		}
		recs := e.Recorder.Last(100, nil)
		if len(recs) != before+1 {
			t.Fatalf("%s: records %d -> %d, want one more", kind, before, len(recs))
		}
		if rec := recs[0]; rec.MidStream != kind || rec.Error == "" || !rec.Stream {
			t.Fatalf("%s: record %+v", kind, rec)
		}
		if ev, _ := eventFields(t, e); ev.Type != "error" || ev.Error == "" {
			t.Fatalf("%s: event %+v", kind, ev)
		}
	}
}
