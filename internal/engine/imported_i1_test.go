package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// I1: catálogo tool misbehavior (spec 017 I1, spec 009). Kinds de nível
// tool (unknown_tool, duplicate_id, length_mid_tool, empty_response,
// reasoning_only) e de argumento, por passo de cenário e por
// errors.tool_misbehavior no perfil do modelo.

const i1Cfg = `
server: {seed: 70}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`

// i1Check validates one broken response; a non-empty return is a failure
// message.
type i1Check func(t *testing.T, c impChat) string

var i1ToolLevel = map[string]i1Check{
	// I1: unknown_tool — the call names a tool the request does not offer.
	"unknown_tool": func(t *testing.T, c impChat) string {
		tcs := c.tools()
		if len(tcs) == 0 {
			t.Fatalf("no tool call")
		}
		switch tcs[0].Function.Name {
		case "read_file", "write_file", "list_dir":
			t.Fatalf("unknown_tool kept an offered name %q", tcs[0].Function.Name)
		}
		return ""
	},
	// I1: duplicate_id — two calls share one id.
	"duplicate_id": func(t *testing.T, c impChat) string {
		tcs := c.tools()
		if len(tcs) < 2 || tcs[0].ID != tcs[len(tcs)-1].ID {
			t.Fatalf("duplicate_id: want a repeated id, got %+v", tcs)
		}
		return ""
	},
	// I1: length_mid_tool — finish_reason length with truncated arguments.
	"length_mid_tool": func(t *testing.T, c impChat) string {
		tcs := c.tools()
		if len(tcs) != 1 {
			t.Fatalf("length_mid_tool: want 1 call, got %d", len(tcs))
		}
		if json.Valid([]byte(tcs[0].Function.Arguments)) {
			t.Fatalf("length_mid_tool: arguments should be cut mid-JSON: %q", tcs[0].Function.Arguments)
		}
		if c.finish() != "length" {
			return "length_mid_tool: finish_reason " + c.finish() + ", want length"
		}
		return ""
	},
	// I1: empty_response — no content, no tool calls.
	"empty_response": func(t *testing.T, c impChat) string {
		if c.content() != "" || len(c.tools()) != 0 {
			t.Fatalf("empty_response: content %q tools %d", c.content(), len(c.tools()))
		}
		if c.finish() != "stop" {
			t.Fatalf("empty_response: finish %q", c.finish())
		}
		return ""
	},
	// I1: reasoning_only — only reasoning, cut by length.
	"reasoning_only": func(t *testing.T, c impChat) string {
		if c.content() != "" || len(c.tools()) != 0 {
			t.Fatalf("reasoning_only: content %q tools %d", c.content(), len(c.tools()))
		}
		if c.finish() != "length" {
			t.Fatalf("reasoning_only: finish %q", c.finish())
		}
		if c.Usage.CompletionTokensDetails.ReasoningTokens <= 0 {
			t.Fatalf("reasoning_only: no reasoning tokens counted: %+v", c.Usage)
		}
		return ""
	},
}

// argObj decodes the first call's arguments as an object.
func argObj(t *testing.T, c impChat) (map[string]any, string) {
	t.Helper()
	tcs := c.tools()
	if len(tcs) == 0 {
		t.Fatalf("no tool call")
	}
	var m map[string]any
	raw := tcs[0].Function.Arguments
	_ = json.Unmarshal([]byte(raw), &m)
	return m, raw
}

var i1ArgLevel = map[string]i1Check{
	"invalid_json_truncated": func(t *testing.T, c impChat) string {
		if _, raw := argObj(t, c); json.Valid([]byte(raw)) {
			t.Fatalf("truncated args still valid: %q", raw)
		}
		return ""
	},
	"invalid_json_trailing_comma": func(t *testing.T, c impChat) string {
		if _, raw := argObj(t, c); json.Valid([]byte(raw)) || !strings.Contains(raw, ",}") {
			t.Fatalf("trailing comma missing: %q", raw)
		}
		return ""
	},
	"invalid_json_single_quotes": func(t *testing.T, c impChat) string {
		if _, raw := argObj(t, c); json.Valid([]byte(raw)) || !strings.Contains(raw, "'") {
			t.Fatalf("single quotes missing: %q", raw)
		}
		return ""
	},
	"missing_required": func(t *testing.T, c impChat) string {
		m, raw := argObj(t, c)
		if m == nil {
			t.Fatalf("not an object: %q", raw)
		}
		name := c.tools()[0].Function.Name
		req := map[string][]string{"read_file": {"path"}, "write_file": {"path", "content"}, "list_dir": {"dir"}}[name]
		for _, k := range req {
			if _, ok := m[k]; !ok {
				return ""
			}
		}
		t.Fatalf("all required present for %s: %q", name, raw)
		return ""
	},
	"wrong_type": func(t *testing.T, c impChat) string {
		m, raw := argObj(t, c)
		for _, v := range m {
			if _, ok := v.(string); !ok {
				return ""
			}
		}
		t.Fatalf("every value still a string: %q", raw)
		return ""
	},
	"extra_property": func(t *testing.T, c impChat) string {
		m, raw := argObj(t, c)
		for k := range m {
			if strings.HasPrefix(k, "unexpected_") {
				return ""
			}
		}
		t.Fatalf("no extra property: %q", raw)
		return ""
	},
	"enum_mismatch": func(t *testing.T, c impChat) string {
		if _, raw := argObj(t, c); !strings.Contains(raw, "not-in-enum") {
			t.Fatalf("no enum violation: %q", raw)
		}
		return ""
	},
	"not_object": func(t *testing.T, c impChat) string {
		if m, raw := argObj(t, c); m != nil || !json.Valid([]byte(raw)) {
			t.Fatalf("not_object should be valid JSON that is not an object: %q", raw)
		}
		return ""
	},
}

// TestI1StepToolMisbehavior: tool_misbehavior on a scenario step.
func TestI1StepToolMisbehavior(t *testing.T) {
	all := map[string]i1Check{}
	for k, v := range i1ToolLevel {
		all[k] = v
	}
	for k, v := range i1ArgLevel {
		all[k] = v
	}
	for kind, check := range all {
		t.Run(kind, func(t *testing.T) {
			e := newEngine(t, i1Cfg)
			impAddScenario(t, e, `
name: i1-step
steps:
  - id: s
    tool_calls: [{name: read_file, args: {path: /tmp/a, mode: text}}]
    tool_misbehavior: `+kind+`
`)
			c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "i1-step"}, impBody("wwz-fast", ","+impTools))
			if reason := check(t, c); reason != "" {
				t.Fatal(reason)
			}
		})
	}
}

// TestI1ProfileToolMisbehavior: errors.tool_misbehavior on the model profile
// breaks randomly generated tool calls.
func TestI1ProfileToolMisbehavior(t *testing.T) {
	all := map[string]i1Check{}
	for k, v := range i1ToolLevel {
		all[k] = v
	}
	for k, v := range i1ArgLevel {
		all[k] = v
	}
	for kind, check := range all {
		t.Run(kind, func(t *testing.T) {
			e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 100, kinds: [`+kind+`]}}
`)
			c, _ := impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"read_file"}}`))
			if reason := check(t, c); reason != "" {
				t.Fatal(reason)
			}
			if rec := impLastRecord(t, e); rec.ToolBreak != kind {
				t.Fatalf("record tool_break %q, want %q", rec.ToolBreak, kind)
			}
		})
	}
}

// TestI1ProfileRateZero: rate 0 never breaks a call.
func TestI1ProfileRateZero(t *testing.T) {
	e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 0, kinds: [unknown_tool]}}
`)
	for i := 0; i < 5; i++ {
		c, _ := impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":"required"`))
		if n := c.tools()[0].Function.Name; strings.HasSuffix(n, "_v2") {
			t.Fatalf("rate 0 broke the call: %q", n)
		}
	}
}

// TestI1ProfileToolFilter: errors.tool_misbehavior.tool limits the breakage
// to calls of that tool.
func TestI1ProfileToolFilter(t *testing.T) {
	e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], tool: write_file}}
`)
	c, _ := impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"read_file"}}`))
	if n := c.tools()[0].Function.Name; n != "read_file" {
		t.Fatalf("errors.tool_misbehavior.tool ignored: broke %q (filter write_file)", n)
	}
	if rec := impLastRecord(t, e); rec.ToolBreak != "" {
		t.Fatalf("record tool_break %q for a filtered-out call", rec.ToolBreak)
	}
	// The matching tool is broken.
	c, _ = impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"write_file"}}`))
	if n := c.tools()[0].Function.Name; n != "write_file_v2" {
		t.Fatalf("write_file call not broken: %q", n)
	}
}

// TestI1ProfileDriversField: errors.tool_misbehavior.drivers (spec 017 I1)
// limits the breakage to the listed drivers; an OpenAI request is untouched
// when only anthropic is listed.
func TestI1ProfileDriversField(t *testing.T) {
	e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], drivers: [anthropic]}}
`)
	c, _ := impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"read_file"}}`))
	if n := c.tools()[0].Function.Name; n != "read_file" {
		t.Fatalf("errors.tool_misbehavior.drivers ignored: broke %q on the openai driver", n)
	}
	if rec := impLastRecord(t, e); rec.ToolBreak != "" {
		t.Fatalf("record tool_break %q on a driver outside the list", rec.ToolBreak)
	}
}

// TestI1ProfileDriversListed: the listed driver is broken.
func TestI1ProfileDriversListed(t *testing.T) {
	e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], drivers: [openai]}}
`)
	c, _ := impChatCall(t, e, nil, impBody("m-tm", ","+impTools+`,"tool_choice":{"type":"function","function":{"name":"read_file"}}`))
	if n := c.tools()[0].Function.Name; n != "read_file_v2" {
		t.Fatalf("openai listed but call not broken: %q", n)
	}
}

// TestI1SpecKindNames: the spec names the JSON styles truncated,
// trailing_comma and single_quotes; they must break the arguments (or be
// rejected), never pass silently as valid JSON.
func TestI1SpecKindNames(t *testing.T) {
	for _, kind := range []string{"truncated", "trailing_comma", "single_quotes"} {
		t.Run(kind, func(t *testing.T) {
			e := newEngine(t, i1Cfg)
			impAddScenario(t, e, `
name: i1-alias
steps:
  - id: s
    tool_calls: [{name: read_file, args: {path: /tmp/a}}]
    tool_misbehavior: `+kind+`
`)
			c, _ := impChatCall(t, e, map[string]string{"x-wwz-scenario": "i1-alias"}, impBody("wwz-fast", ","+impTools))
			if _, raw := argObj(t, c); json.Valid([]byte(raw)) {
				t.Fatalf("kind %s (spec 017 name) left valid JSON: %q", kind, raw)
			}
		})
	}
}

// TestI1AnthropicDuplicateID: the misbehaviour reaches the Anthropic dialect.
func TestI1AnthropicDuplicateID(t *testing.T) {
	e := newEngine(t, i1Cfg+`
models:
  - id: m-tm
    latency: fast
    errors: {tool_misbehavior: {rate: 100, kinds: [duplicate_id]}}
`)
	body := `{"model":"m-tm","max_tokens":1024,"messages":[{"role":"user","content":"x"}],` + anthToolsJSON + `}`
	w := call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}, body)
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	ids := decode(t, w.Body()).toolIDs()
	if len(ids) < 2 || ids[0] != ids[len(ids)-1] {
		t.Fatalf("duplicate_id on anthropic: %v", ids)
	}
}
