package engine_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// I8 (tool_call_id_format), I9 (tool placeholder por classe), I12 (latest_message_role,
// tool_result_for, semantic_match), I13 (mixed, on_exhausted, step_limit) e I14 (timeline)
// exercised end to end through the engine (spec 017, spec 011).

// --- I9 ----------------------------------------------------------------

const i9Scenario = `
name: i9
steps:
  - id: s
    expect: {header: {x-class: CLASS}}
    tool_calls: [{name: CLASS, args: schema}]
`

// TestI9ToolClass: any_write picks a writing tool, any_read a reading tool,
// any any offered tool — at runtime, over many conversations.
func TestI9ToolClass(t *testing.T) {
	allowed := map[string]map[string]bool{
		"any_write": {"write_file": true},
		"any_read":  {"read_file": true, "list_dir": true},
		"any":       {"read_file": true, "write_file": true, "list_dir": true},
	}
	for class, ok := range allowed {
		t.Run(class, func(t *testing.T) {
			e := newEngine(t, fastCfg)
			impAddScenario(t, e, strings.ReplaceAll(i9Scenario, "CLASS", class))
			seen := map[string]bool{}
			for i := 0; i < 20; i++ {
				h := map[string]string{"x-wwz-scenario": "i9", "x-class": class, "x-wwz-conversation-id": "c" + strconv.Itoa(i)}
				c, _ := impChatCall(t, e, h, impBody("wwz-fast", ","+impTools))
				tcs := c.tools()
				if len(tcs) != 1 || !ok[tcs[0].Function.Name] {
					t.Fatalf("%s picked %+v", class, tcs)
				}
				if !json.Valid([]byte(tcs[0].Function.Arguments)) {
					t.Fatalf("args %q", tcs[0].Function.Arguments)
				}
				seen[tcs[0].Function.Name] = true
			}
			if class == "any" && len(seen) < 2 {
				t.Fatalf("any always picked the same tool: %v", seen)
			}
		})
	}
}

// TestI9NoToolsSkipsPlaceholder: with no tools offered the placeholder call
// is dropped silently.
func TestI9NoToolsSkipsPlaceholder(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i9n\nsteps:\n  - id: s\n    content: hi\n    tool_calls: [{name: any_write}]\n")
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "i9n"}, impBody("wwz-fast", ""))
	if len(c.tools()) != 0 || c.content() != "hi" {
		t.Fatalf("content %q tools %+v", c.content(), c.tools())
	}
}

// TestI9StepNamesUnofferedTool: a step naming a tool the request does not
// offer answers HTTP 400 in the driver's envelope.
func TestI9StepNamesUnofferedTool(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i9x\nsteps:\n  - id: s\n    tool_calls: [{name: delete_file, args: {path: /a}}]\n")
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i9x"}), impBody("wwz-fast", ","+impTools))
	if w.StatusCode() != 400 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body(), &env); err != nil || env.Error.Type != "invalid_request_error" || !strings.Contains(env.Error.Message, "delete_file") {
		t.Fatalf("envelope %s", w.Body())
	}
	// Anthropic dialect too.
	body := `{"model":"wwz-fast","max_tokens":256,"messages":[{"role":"user","content":"x"}],` + anthToolsJSON + `}`
	w = call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "x-wwz-scenario": "i9x"}, body)
	if w.StatusCode() != 400 || !strings.Contains(string(w.Body()), "invalid_request_error") {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
}

// --- I12 ---------------------------------------------------------------

const i12Scenario = `
name: i12
steps:
  - id: after-write
    expect: {latest_message_role: tool, tool_result_for: write_file}
    content: "after write"
  - id: after-tool
    expect: {latest_message_role: tool}
    content: "after tool"
  - id: user-turn
    expect: {latest_message_role: user}
    content: "user turn"
`

func i12Body(tool string) string {
	if tool == "" {
		return `{"model":"wwz-fast","messages":[{"role":"user","content":"go"}],` + impTools + `}`
	}
	return `{"model":"wwz-fast","messages":[
	  {"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + tool + `","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"done"}],` + impTools + `}`
}

// TestI12LatestRoleAndToolResultFor: the step follows the role of the last
// message and which tool the results answer.
func TestI12LatestRoleAndToolResultFor(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, i12Scenario)
	h := map[string]string{"x-wwz-scenario": "i12"}
	for _, c := range []struct{ tool, want string }{{"", "user turn"}, {"read_file", "after tool"}, {"write_file", "after write"}} {
		got, w := impChatCall(t, e, h, i12Body(c.tool))
		if got.content() != c.want {
			t.Fatalf("tool %q: content %q (step %s), want %q", c.tool, got.content(), w.Headers()["x-wwz-step"], c.want)
		}
	}
}

// TestI12ToolResultForAnthropic: tool_result_for also reads Anthropic
// tool_result blocks (which arrive in a user message).
func TestI12ToolResultForAnthropic(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, `
name: i12a
steps:
  - id: after-write
    expect: {tool_result_for: write_file}
    content: "after write"
  - id: other
    content: "other"
`)
	tools := `"tools":[{"name":"write_file","input_schema":{"type":"object","properties":{}}},{"name":"read_file","input_schema":{"type":"object","properties":{}}}]`
	mk := func(tool string) string {
		return `{"model":"wwz-fast","max_tokens":256,"messages":[
		  {"role":"user","content":"go"},
		  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"` + tool + `","input":{}}]},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}],` + tools + `}`
	}
	h := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "x-wwz-scenario": "i12a"}
	for tool, want := range map[string]string{"write_file": "after write", "read_file": "other"} {
		w := call(t, e, "anthropic", "/v1/messages", h, mk(tool))
		if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"text":"`+want+`"`) {
			t.Fatalf("%s: %d %s", tool, w.StatusCode(), w.Body())
		}
	}
}

// --- I8 (tool_call_id_format) -------------------------------------------

const i8Scenario = `
name: i8
tool_call_id_format: FORMAT
steps:
  - id: first
    expect: {tool_results: 0}
    tool_calls: [{name: read_file, args: {path: /a}}]
  - id: after
    content: done
`

// i8IDs runs the i8 scenario on every surface and returns the tool call ids.
func i8IDs(t *testing.T, format string) map[surface][]string {
	t.Helper()
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, strings.ReplaceAll(i8Scenario, "FORMAT", format))
	out := map[surface][]string{}
	for _, s := range surfaces {
		h := s.headers()
		h["x-wwz-scenario"] = "i8"
		w := call(t, e, s.drv, s.path, h, s.body(s.tools()))
		if w.StatusCode() != 200 {
			t.Fatalf("%s %s: %d %s", s.drv, s.path, w.StatusCode(), w.Body())
		}
		ids := decode(t, w.Body()).toolIDs()
		if len(ids) != 1 {
			t.Fatalf("%s %s: tool ids %v in %s", s.drv, s.path, ids, w.Body())
		}
		out[s] = ids
	}
	return out
}

// TestI8EncodedIDs: encoded ids carry scenario and step, with the toolu
// prefix on Messages routes and call elsewhere.
func TestI8EncodedIDs(t *testing.T) {
	for s, ids := range i8IDs(t, "encoded") {
		want := "call_i8_first_0_"
		if s.messages {
			want = "toolu_i8_first_0_"
		}
		if !strings.HasPrefix(ids[0], want) {
			t.Errorf("%s %s: id %q, want prefix %q", s.drv, s.path, ids[0], want)
		}
	}
}

// TestI8NativeIDs: native ids are the driver's own format, identical to the
// provider (call_ + 24 base62, toolu_01 + 22 base62).
func TestI8NativeIDs(t *testing.T) {
	for s, ids := range i8IDs(t, "native") {
		if !s.toolID.MatchString(ids[0]) {
			t.Errorf("%s %s: native id %q does not match %s", s.drv, s.path, ids[0], s.toolID)
		}
	}
}

// TestI8LiteralIDWins: tool_calls[].id is kept in both formats.
func TestI8LiteralIDWins(t *testing.T) {
	for _, format := range []string{"encoded", "native"} {
		e := newEngine(t, fastCfg)
		impAddScenario(t, e, "name: lit\ntool_call_id_format: "+format+"\nsteps:\n  - id: s\n    tool_calls: [{name: read_file, id: my_fixed_id, args: {path: /a}}]\n")
		c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "lit"}, impBody("wwz-fast", ","+impTools))
		if tcs := c.tools(); len(tcs) != 1 || tcs[0].ID != "my_fixed_id" {
			t.Fatalf("%s: %+v", format, tcs)
		}
	}
}

// TestI8EncodedToolUDecodes: an encoded toolu_ id sent back on the
// Anthropic route pins the scenario and the step (expect.last_step).
func TestI8EncodedToolUDecodes(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i8d\nsteps:\n  - id: first\n    expect: {tool_results: 0}\n    tool_calls: [{name: read_file, args: {path: /a}}]\n  - id: after\n    expect: {last_step: first}\n    content: decoded\n")
	h := map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01", "x-wwz-scenario": "i8d"}
	w := call(t, e, "anthropic", "/v1/messages", h, `{"model":"wwz-fast","max_tokens":1024,"messages":[{"role":"user","content":"go"}],`+anthToolsJSON+`}`)
	ids := decode(t, w.Body()).toolIDs()
	if len(ids) != 1 || !strings.HasPrefix(ids[0], "toolu_i8d_first_0_") {
		t.Fatalf("first turn ids %v: %s", ids, w.Body())
	}
	body := `{"model":"wwz-fast","max_tokens":1024,"messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + ids[0] + `","name":"read_file","input":{"path":"/a"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + ids[0] + `","content":"x"}]}],` + anthToolsJSON + `}`
	w = call(t, e, "anthropic", "/v1/messages", h, body)
	if w.StatusCode() != 200 || w.Headers()["x-wwz-step"] != "after" || !strings.Contains(string(w.Body()), "decoded") {
		t.Fatalf("second turn: %d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
}

// --- I12 (semantic_match) ----------------------------------------------

// TestI12SemanticMatch: the same words in another order match; an unrelated
// message falls below the threshold.
func TestI12SemanticMatch(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i12s\non_no_match: error\nsteps:\n  - id: deploy\n    expect: {semantic_match: {text: \"Deploy the service to production\", threshold: 0.8}}\n    content: deploying\n")
	msg := func(text string) string {
		return `{"model":"wwz-fast","messages":[{"role":"user","content":"` + text + `"}]}`
	}
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i12s"}), msg("to production, deploy THE service"))
	if w.StatusCode() != 200 || w.Headers()["x-wwz-step"] != "deploy" {
		t.Fatalf("reordered words: %d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i12s"}), msg("write a poem about the sea"))
	if w.StatusCode() != 400 {
		t.Fatalf("below the threshold: %d %s", w.StatusCode(), w.Body())
	}
}

// --- I13 ---------------------------------------------------------------

// TestI13Mixed: a step with content and tool calls answers both in the same
// response (turn type mixed).
func TestI13Mixed(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13m\nsteps:\n  - id: s\n    content: Let me check.\n    tool_calls: [{name: read_file, args: {path: /a}}]\n")
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "i13m"}, impBody("wwz-fast", ","+impTools))
	if c.content() != "Let me check." || len(c.tools()) != 1 || c.finish() != "tool_calls" {
		t.Fatalf("content %q tools %+v finish %q", c.content(), c.tools(), c.finish())
	}
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-scenario": "i13m"}, impBody("wwz-fast", ","+impTools+`,"stream":true`))
	if strings.Join(s.texts, "") != "Let me check." || s.argFrames == 0 || s.finish != "tool_calls" {
		t.Fatalf("stream mixed: %s", s.raw)
	}
}

// i13Body builds a conversation with n previous assistant turns.
func i13Body(n int) string {
	msgs := `{"role":"user","content":"go"}`
	for i := 0; i < n; i++ {
		msgs += `,{"role":"assistant","content":"a` + strconv.Itoa(i) + `"},{"role":"user","content":"again"}`
	}
	return `{"model":"wwz-fast","messages":[` + msgs + `]}`
}

// engineWrap sends i13 conversations to the i13 scenario.
type engineWrap struct {
	t *testing.T
	e *engine.Engine
}

func (w *engineWrap) chat(n int) *core.MemWriter {
	w.t.Helper()
	return call(w.t, w.e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i13"}), i13Body(n))
}

func i13Engine(t *testing.T, onExhausted string) *engineWrap {
	t.Helper()
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13\nstep_limit: 2\non_exhausted: "+onExhausted+"\nsteps:\n  - id: s\n    content: scripted\n")
	return &engineWrap{t: t, e: e}
}

// TestI13StepLimitError: once the conversation reaches step_limit assistant
// turns, on_exhausted: error answers 400.
func TestI13StepLimitError(t *testing.T) {
	w := i13Engine(t, "error")
	if c := w.chat(1); c.StatusCode() != 200 {
		t.Fatalf("below the limit: %d %s", c.StatusCode(), c.Body())
	}
	c := w.chat(2)
	if c.StatusCode() != 400 || !strings.Contains(string(c.Body()), "step limit") {
		t.Fatalf("at the limit: %d %s", c.StatusCode(), c.Body())
	}
}

// TestI13StepLimitFallback: on_exhausted: fallback hands the request to the
// generation profile (no scenario headers, random content).
func TestI13StepLimitFallback(t *testing.T) {
	w := i13Engine(t, "fallback")
	c := w.chat(3)
	if c.StatusCode() != 200 || c.Headers()["x-wwz-scenario"] != "" || strings.Contains(string(c.Body()), `"scripted"`) {
		t.Fatalf("fallback: %d %v %s", c.StatusCode(), c.Headers(), c.Body())
	}
}

// TestI13StepLimitRepeatLast: on_exhausted: repeat_last keeps answering
// with the scenario step.
func TestI13StepLimitRepeatLast(t *testing.T) {
	w := i13Engine(t, "repeat_last")
	c := w.chat(5)
	if c.StatusCode() != 200 || c.Headers()["x-wwz-step"] != "s" || !strings.Contains(string(c.Body()), `"scripted"`) {
		t.Fatalf("repeat_last: %d %v %s", c.StatusCode(), c.Headers(), c.Body())
	}
}

// TestI13StepLimitRepeatLastIsLastStep: on_exhausted is checked before
// matching, and repeat_last answers with the last step of the file even
// when an earlier step matches the conversation (ADR 0027).
func TestI13StepLimitRepeatLastIsLastStep(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13\nstep_limit: 2\nsteps:\n  - id: a\n    content: first\n  - id: z\n    expect: {turn: 99}\n    content: last\n")
	w := &engineWrap{t: t, e: e}
	if c := w.chat(1); c.Headers()["x-wwz-step"] != "a" || !strings.Contains(string(c.Body()), `"first"`) {
		t.Fatalf("below the limit: %v %s", c.Headers(), c.Body())
	}
	c := w.chat(2)
	if c.StatusCode() != 200 || c.Headers()["x-wwz-step"] != "z" || !strings.Contains(string(c.Body()), `"last"`) {
		t.Fatalf("at the limit: %d %v %s", c.StatusCode(), c.Headers(), c.Body())
	}
}

// TestI13StepLimitErrorWithMatchingStep: on_exhausted: error wins over a
// step that matches.
func TestI13StepLimitErrorWithMatchingStep(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13\nstep_limit: 3\non_exhausted: error\nsteps:\n  - id: a\n    expect: {always: true}\n    content: first\n")
	w := &engineWrap{t: t, e: e}
	if c := w.chat(3); c.StatusCode() != 400 || !strings.Contains(string(c.Body()), "step limit 3") {
		t.Fatalf("matching step at the limit: %d %s", c.StatusCode(), c.Body())
	}
}

// TestI13OnNoMatchRepeatLast: below the limit, on_no_match: repeat_last
// answers with the last step of the file.
func TestI13OnNoMatchRepeatLast(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13\non_no_match: repeat_last\nsteps:\n  - id: a\n    expect: {turn: 1}\n    content: one\n  - id: b\n    expect: {turn: 2}\n    content: two\n")
	w := &engineWrap{t: t, e: e}
	c := w.chat(4) // turn 5: no step matches
	if c.StatusCode() != 200 || c.Headers()["x-wwz-step"] != "b" || !strings.Contains(string(c.Body()), `"two"`) {
		t.Fatalf("on_no_match repeat_last: %d %v %s", c.StatusCode(), c.Headers(), c.Body())
	}
}

// TestI13DefaultStepLimit: without step_limit the guard is 50 turns.
func TestI13DefaultStepLimit(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, "name: i13\non_exhausted: error\nsteps:\n  - id: s\n    content: scripted\n")
	w := &engineWrap{t: t, e: e}
	if c := w.chat(49); c.StatusCode() != 200 {
		t.Fatalf("49 turns: %d", c.StatusCode())
	}
	if c := w.chat(50); c.StatusCode() != 400 {
		t.Fatalf("50 turns: %d %s", c.StatusCode(), c.Body())
	}
}

// --- I14 ---------------------------------------------------------------

// TestI14Timeline: a step timeline is replayed 1:1 through the stream
// writer (text, reasoning, tool_call, finish).
func TestI14Timeline(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, `
name: i14
steps:
  - id: s
    timeline:
      - {type: text, text: "Hello "}
      - {type: text, text: "world"}
      - {type: tool_call, name: read_file, args: {path: /a}}
      - {type: finish, reason: tool_calls}
`)
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-scenario": "i14"}, impBody("wwz-fast", ","+impTools+`,"stream":true`))
	if strings.Join(s.texts, "") != "Hello world" || s.argFrames == 0 || s.finish != "tool_calls" {
		t.Fatalf("timeline not replayed (stream: texts %q, args %d, finish %q)", s.texts, s.argFrames, s.finish)
	}
	if len(s.texts) != 2 {
		t.Fatalf("timeline text events should map 1:1 to deltas: %q", s.texts)
	}
	if !strings.Contains(s.raw, "read_file") {
		t.Fatalf("tool call missing: %s", s.raw)
	}
	// Non-streaming: the same timeline folds into one message.
	c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "i14"}, impBody("wwz-fast", ","+impTools))
	tcs := c.tools()
	if len(tcs) != 1 || tcs[0].Function.Name != "read_file" || tcs[0].Function.Arguments != `{"path":"/a"}` {
		t.Fatalf("non-stream tool calls %+v", tcs)
	}
	if !strings.Contains(string(mustJSON(t, c)), "Hello world") {
		t.Fatalf("non-stream content: %+v", c)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestI14TimelineEvents: reasoning, ping, delay and an early finish; events
// after finish are ignored.
func TestI14TimelineEvents(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, `
name: i14e
steps:
  - id: s
    timeline:
      - {type: reasoning, text: "thinking"}
      - {type: ping}
      - {type: delay, ms: 1}
      - {type: usage}
      - {type: text, text: "done"}
      - {type: finish, reason: length}
      - {type: text, text: "ignored"}
`)
	s, _ := impStreamCall(t, e, map[string]string{"x-wwz-scenario": "i14e"}, impBody("wwz-fast", `,"stream":true,"stream_options":{"include_usage":true}`))
	if strings.Join(s.texts, "") != "done" || s.finish != "length" || !s.done {
		t.Fatalf("stream: texts %q finish %q done %v\n%s", s.texts, s.finish, s.done, s.raw)
	}
	// OpenAI hides reasoning; it still counts in usage.
	if !strings.Contains(s.raw, `"reasoning_tokens"`) || strings.Contains(s.raw, `"reasoning_tokens":0`) || !strings.Contains(s.raw, ": ping") {
		t.Fatalf("reasoning/ping events missing:\n%s", s.raw)
	}
}

// TestI14TimelineError: an error event ends the stream with the dialect's
// error frame, and is the reply of a non-streaming request.
func TestI14TimelineError(t *testing.T) {
	e := newEngine(t, fastCfg)
	impAddScenario(t, e, `
name: i14x
steps:
  - id: s
    timeline:
      - {type: text, text: "partial"}
      - {type: error, error: overloaded, text: "busy now"}
      - {type: text, text: "never"}
`)
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i14x"}), impBody("wwz-fast", `,"stream":true`))
	body := string(w.Body())
	if !strings.Contains(body, "partial") || !strings.Contains(body, "busy now") || strings.Contains(body, "never") || strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("stream with error event:\n%s", body)
	}
	w = call(t, e, "openai", "/v1/chat/completions", impHeaders(map[string]string{"x-wwz-scenario": "i14x"}), impBody("wwz-fast", ""))
	if w.StatusCode() < 500 || !strings.Contains(string(w.Body()), "busy now") {
		t.Fatalf("non-stream with error event: %d %s", w.StatusCode(), w.Body())
	}
}
