package engine_test

import (
	"encoding/json"
	"testing"
)

// Task 383 (auditoria do DONE, task 362): misbehavior de tool por header
// (R20, spec 009) e x-wwz-delay-ms com server.request_headers: false (R22).

const auditHeadersCfg = `
server: {seed: 70}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  errors:
    broken-tools:
      tool_misbehavior: {rate: 100, kinds: [wrong_type]}
models:
  - {id: m-plain, latency: fast, errors: none}
`

// auditArgsAreValid reports whether the first tool call carries arguments
// that are a JSON object whose every value keeps the schema's string type.
func auditArgsAreValid(t *testing.T, c impChat) bool {
	t.Helper()
	tcs := c.tools()
	if len(tcs) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(tcs))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tcs[0].Function.Arguments), &m); err != nil {
		t.Fatalf("arguments %q: %v", tcs[0].Function.Arguments, err)
	}
	for _, v := range m {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
}

// TestAuditChaosProfileHeaderToolMisbehavior: x-wwz-chaos-profile naming a
// profile with tool_misbehavior breaks the arguments of that request only.
func TestAuditChaosProfileHeaderToolMisbehavior(t *testing.T) {
	e := newEngine(t, auditHeadersCfg)
	body := impBody("m-plain", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"read_file"}}`)

	c, _ := impChatCall(t, e, map[string]string{"x-wwz-chaos-profile": "broken-tools"}, body)
	if auditArgsAreValid(t, c) {
		t.Fatalf("header profile did not break the arguments: %+v", c.tools())
	}
	if rec := impLastRecord(t, e); rec.ToolBreak != "wrong_type" {
		t.Fatalf("record tool_break %q, want wrong_type", rec.ToolBreak)
	}

	// The next request, without the header, is valid again.
	for i := 0; i < 3; i++ {
		c, _ = impChatCall(t, e, nil, body)
		if !auditArgsAreValid(t, c) {
			t.Fatalf("request %d without the header got broken arguments: %+v", i, c.tools())
		}
		if rec := impLastRecord(t, e); rec.ToolBreak != "" {
			t.Fatalf("request %d record tool_break %q", i, rec.ToolBreak)
		}
	}
}

const auditDelayCfg = `
server: {seed: 42%s}
profiles:
  latency:
    logical: {ttft: 10ms, itl: 0ms, clock: logical}
models:
  - {id: m-logical, latency: logical, errors: none}
`

// TestAuditDelayMSHeader: x-wwz-delay-ms sets the TTFT while
// server.request_headers is on, and is ignored when it is off (the logical
// clock keeps the profile's TTFT).
func TestAuditDelayMSHeader(t *testing.T) {
	// The header also sets the ITL (the real clock sleeps it), so keep it small.
	h := map[string]string{"x-wwz-delay-ms": "20"}
	on := newEngine(t, sprintfCfg(auditDelayCfg, ""))
	impChatCall(t, on, h, impBody("m-logical", ""))
	if rec := impLastRecord(t, on); rec.TTFTMS != 20 {
		t.Fatalf("headers on: ttft %d ms, want 20", rec.TTFTMS)
	}

	off := newEngine(t, sprintfCfg(auditDelayCfg, ", request_headers: false"))
	impChatCall(t, off, h, impBody("m-logical", ""))
	rec := impLastRecord(t, off)
	if rec.TTFTMS != 10 {
		t.Fatalf("request_headers false: ttft %d ms, want the profile's 10", rec.TTFTMS)
	}
}

func sprintfCfg(format, arg string) string {
	out := make([]byte, 0, len(format)+len(arg))
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) && format[i+1] == 's' {
			out = append(out, arg...)
			i++
			continue
		}
		out = append(out, format[i])
	}
	return string(out)
}
