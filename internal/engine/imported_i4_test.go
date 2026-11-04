package engine_test

import (
	"encoding/base64"
	"testing"
)

// I4: controle da resposta por header (spec 017 I4, spec 004/009):
// x-wwz-response, -response-base64, -tool-calls (ignora tool_choice),
// -content-filter, -tool-prob e server.request_headers: false.

// TestI4Response: x-wwz-response replaces the content.
func TestI4Response(t *testing.T) {
	e := newEngine(t, fastCfg)
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-response": "fixed answer"}, impBody("wwz-fast", ""))
	if c.content() != "fixed answer" || c.finish() != "stop" {
		t.Fatalf("content %q finish %q", c.content(), c.finish())
	}
}

// TestI4ResponseStream: the header content also drives the stream.
func TestI4ResponseStream(t *testing.T) {
	e := newEngine(t, fastCfg)
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-response": "one two three"}, impBody("wwz-fast", `,"stream":true`))
	got := ""
	for _, x := range s.texts {
		got += x
	}
	if got != "one two three" || !s.done {
		t.Fatalf("stream text %q done %v", got, s.done)
	}
}

// TestI4ResponseBase64: std and URL-safe base64 are both decoded and win
// over x-wwz-response.
func TestI4ResponseBase64(t *testing.T) {
	e := newEngine(t, fastCfg)
	msg := "linha 1\nlinha 2 ✓"
	for _, enc := range []string{base64.StdEncoding.EncodeToString([]byte(msg)), base64.RawURLEncoding.EncodeToString([]byte(msg))} {
		c, _ := impChatCall(t, e, map[string]string{"x-wwz-response-base64": enc, "x-wwz-response": "ignored"}, impBody("wwz-fast", ""))
		if c.content() != msg {
			t.Fatalf("base64 %q → %q", enc, c.content())
		}
	}
}

// TestI4ToolCallsIgnoresToolChoice: x-wwz-tool-calls emits the calls even
// with tool_choice none; ids are minted in the provider format.
func TestI4ToolCallsIgnoresToolChoice(t *testing.T) {
	e := newEngine(t, fastCfg)
	hdr := `[{"name":"read_file","arguments":{"path":"/a"}},{"function":{"name":"write_file","arguments":"{\"path\":\"/b\",\"content\":\"x\"}"}}]`
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-tool-calls": hdr}, impBody("wwz-fast", ","+impTools+`,"tool_choice":"none"`))
	tcs := c.tools()
	if len(tcs) != 2 || tcs[0].Function.Name != "read_file" || tcs[1].Function.Name != "write_file" {
		t.Fatalf("tool calls %+v", tcs)
	}
	if tcs[0].Function.Arguments != `{"path":"/a"}` || tcs[1].Function.Arguments != `{"path":"/b","content":"x"}` {
		t.Fatalf("arguments %q / %q", tcs[0].Function.Arguments, tcs[1].Function.Arguments)
	}
	for _, tc := range tcs {
		if !reCall.MatchString(tc.ID) {
			t.Fatalf("id %q not in OpenAI format", tc.ID)
		}
	}
	if c.finish() != "tool_calls" {
		t.Fatalf("finish %q", c.finish())
	}
}

// TestI4ToolCallsWithoutTools: the header works even when the request offers
// no tools.
func TestI4ToolCallsWithoutTools(t *testing.T) {
	e := newEngine(t, fastCfg)
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-tool-calls": `[{"id":"call_fixed","name":"x","arguments":{}}]`}, impBody("wwz-fast", ""))
	if len(c.tools()) != 1 || c.tools()[0].ID != "call_fixed" || c.tools()[0].Function.Arguments != "{}" {
		t.Fatalf("tool calls %+v", c.tools())
	}
}

// TestI4ContentFilter: x-wwz-content-filter turns the reply into a
// content_filter finish.
func TestI4ContentFilter(t *testing.T) {
	e := newEngine(t, fastCfg)
	for _, v := range []string{"1", "true"} {
		c, _ := impChatCall(t, e, map[string]string{"x-wwz-content-filter": v}, impBody("wwz-fast", ""))
		if c.finish() != "content_filter" {
			t.Fatalf("%s: finish %q", v, c.finish())
		}
	}
}

// TestI4ToolProb: x-wwz-tool-prob 1 (or 100) always calls a tool with
// tool_choice auto; 0 never does.
func TestI4ToolProb(t *testing.T) {
	e := newEngine(t, fastCfg)
	for i := 0; i < 8; i++ {
		body := `{"model":"wwz-fast","messages":[{"role":"user","content":"q` + string(rune('a'+i)) + `"}],` + impTools + `}`
		for _, p := range []string{"1", "100"} {
			c, _ := impChatCall(t, e, map[string]string{"x-wwz-tool-prob": p}, body)
			if len(c.tools()) == 0 {
				t.Fatalf("tool-prob %s: no tool call", p)
			}
		}
		c, _ := impChatCall(t, e, map[string]string{"x-wwz-tool-prob": "0"}, body)
		if len(c.tools()) != 0 {
			t.Fatalf("tool-prob 0: %d tool calls", len(c.tools()))
		}
	}
}

// TestI4RequestHeadersOff: server.request_headers: false ignores every
// x-wwz-* override (pure throughput runs).
func TestI4RequestHeadersOff(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42, request_headers: false}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`)
	h := map[string]string{
		"x-wwz-response":       "should be ignored",
		"x-wwz-return-error":   "server_error",
		"x-wwz-content-filter": "1",
		"x-wwz-tool-calls":     `[{"name":"x","arguments":{}}]`,
		"x-wwz-seed":           "99",
	}
	c, w := impChatCall(t, e, h, impBody("wwz-fast", ""))
	if c.content() == "should be ignored" || c.finish() != "stop" || len(c.tools()) != 0 {
		t.Fatalf("overrides applied with request_headers false: content %q finish %q tools %d", c.content(), c.finish(), len(c.tools()))
	}
	if got := w.Headers()["x-wwz-seed"]; got != "42" {
		t.Fatalf("x-wwz-seed %q, want the global 42", got)
	}
}
