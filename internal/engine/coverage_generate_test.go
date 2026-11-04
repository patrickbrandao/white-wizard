package engine_test

import (
	"strings"
	"testing"
)

const covGenCfg = `
server: {seed: 5}
profiles:
  latency:
    default: {ttft: 0ms, itl: 0ms, clock: logical}
  generation:
    echo: {mode: echo}
    lorem: {mode: lorem}
    tpl: {mode: template, template: "m={{model}} u={{last_user_message}} r={{request_id}} p={{prompt_tokens}}"}
    json: {mode: json}
    sent: {sentences: ["Hello world."]}
    red: {redacted_probability: 100, reasoning_tokens: {min: 20, max: 30, probability: 100}}
    tools: {tool_calls: {probability: 0}, text_before_tool_probability: 100}
  errors:
    tm-unknown: {tool_misbehavior: {rate: 100, kinds: [unknown_tool]}}
    tm-dup: {tool_misbehavior: {rate: 100, kinds: [duplicate_id]}}
    tm-len: {tool_misbehavior: {rate: 100, kinds: [length_mid_tool]}}
    tm-empty: {tool_misbehavior: {rate: 100, kinds: [empty_response]}}
    tm-reason: {tool_misbehavior: {rate: 100, kinds: [reasoning_only]}}
    tm-enum: {tool_misbehavior: {rate: 100, kinds: [enum_mismatch]}}
    tm-tool: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], tool: nothere}}
    tm-tool-hit: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], tool: read_file}}
    tm-other-driver: {tool_misbehavior: {rate: 100, kinds: [unknown_tool], drivers: [anthropic]}}
    mod-filter: {moderation: {probability: 100, mode: finish_reason}}
    mod-refusal: {moderation: {probability: 100, mode: refusal}}
models:
  - {id: g-echo, generation: echo}
  - {id: g-lorem, generation: lorem}
  - {id: g-tpl, generation: tpl}
  - {id: g-json, generation: json}
  - {id: g-sent, generation: sent}
  - {id: g-red, generation: red}
  - {id: g-tools, generation: tools}
  - {id: g-plain}
  - {id: g-unknown, errors: tm-unknown}
  - {id: g-dup, errors: tm-dup}
  - {id: g-len, errors: tm-len}
  - {id: g-empty, errors: tm-empty}
  - {id: g-reason, errors: tm-reason}
  - {id: g-enum, errors: tm-enum}
  - {id: g-tool, errors: tm-tool}
  - {id: g-tool-hit, errors: tm-tool-hit}
  - {id: g-other, errors: tm-other-driver}
  - {id: g-filter, errors: mod-filter}
  - {id: g-refusal, errors: mod-refusal}
`

func TestCoverageGenerationModes(t *testing.T) {
	e := newEngine(t, covGenCfg)
	c, _ := impChatCall(t, e, nil, impBody("g-echo", ""))
	if c.content() != "do the task" {
		t.Fatalf("echo: %q", c.content())
	}
	c, _ = impChatCall(t, e, nil, impBody("g-lorem", ""))
	if c.content() == "" {
		t.Fatal("lorem")
	}
	c, _ = impChatCall(t, e, nil, impBody("g-tpl", ""))
	if !strings.HasPrefix(c.content(), "m=g-tpl u=do the task r=") {
		t.Fatalf("template: %q", c.content())
	}
	c, _ = impChatCall(t, e, nil, impBody("g-json", ""))
	if !strings.HasPrefix(c.content(), "{") {
		t.Fatalf("json: %q", c.content())
	}
	c, _ = impChatCall(t, e, nil, impBody("g-sent", ""))
	if !strings.Contains(c.content(), "Hello world.") {
		t.Fatalf("sentences: %q", c.content())
	}
	c, _ = impChatCall(t, e, nil, impBody("g-plain", `,"response_format":{"type":"json_object"}`))
	if !strings.HasPrefix(c.content(), "{") {
		t.Fatalf("json_object: %q", c.content())
	}
	schema := `,"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}}}`
	c, _ = impChatCall(t, e, nil, impBody("g-plain", schema))
	if !strings.Contains(c.content(), `"a"`) {
		t.Fatalf("json_schema: %q", c.content())
	}
	bad := `,"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"bogus-type"}}}`
	c, _ = impChatCall(t, e, nil, impBody("g-plain", bad))
	_ = c
}

func TestCoverageReasoningAndLimits(t *testing.T) {
	e := newEngine(t, covGenCfg)
	c, w := impChatCall(t, e, nil, impBody("g-red", `,"reasoning_effort":"high"`))
	if c.Usage.CompletionTokensDetails.ReasoningTokens == 0 {
		t.Fatalf("reasoning: %s", w.Body())
	}
	// A tiny max_tokens trims the reasoning (budget below the reasoning) and
	// ends with length.
	c, _ = impChatCall(t, e, nil, impBody("g-red", `,"reasoning_effort":"high","max_tokens":3`))
	if c.finish() != "length" {
		t.Fatalf("finish %q", c.finish())
	}
	// A budget above the reasoning trims the content.
	c, _ = impChatCall(t, e, nil, impBody("g-lorem", `,"max_tokens":2`))
	if c.finish() != "length" {
		t.Fatalf("finish %q", c.finish())
	}
	// Past the model's max output the request's limit is capped.
	impChatCall(t, e, nil, impBody("g-plain", `,"max_tokens":99999999`))
}

func TestCoverageToolPaths(t *testing.T) {
	e := newEngine(t, covGenCfg)
	// text before the tool: tool_choice required forces the call.
	c, _ := impChatCall(t, e, nil, impBody("g-tools", ","+impTools+`,"tool_choice":"required"`))
	if len(c.tools()) == 0 {
		t.Fatal("forced tool call")
	}
	// tool_choice none: no call.
	c, _ = impChatCall(t, e, nil, impBody("g-tools", ","+impTools+`,"tool_choice":"none"`))
	if len(c.tools()) != 0 {
		t.Fatal("tool_choice none")
	}
	// x-wwz-tool-prob header picks the probability.
	c, _ = impChatCall(t, e, map[string]string{"x-wwz-tool-prob": "1"}, impBody("g-plain", ","+impTools))
	if len(c.tools()) == 0 {
		t.Fatal("tool prob 1")
	}
	// Header tool calls with call_hdr_ ids are re-minted.
	hdr := `[{"name":"read_file","arguments":"{\"path\":\"a\"}"},{"id":"x1","function":{"name":"list_dir","arguments":{"dir":"d"}}},{"name":"write_file","arguments":""}]`
	c, _ = impChatCall(t, e, map[string]string{"x-wwz-tool-calls": hdr}, impBody("g-plain", ","+impTools))
	if len(c.tools()) != 3 || strings.HasPrefix(c.tools()[0].ID, "call_hdr_") || c.tools()[1].ID != "x1" {
		t.Fatalf("header tools: %+v", c.tools())
	}

	for _, id := range []string{"g-unknown", "g-dup", "g-len", "g-empty", "g-reason", "g-enum", "g-tool", "g-tool-hit", "g-other"} {
		c, w := impChatCall(t, e, nil, impBody(id, ","+impTools+`,"tool_choice":"required"`))
		if len(c.Choices) == 0 {
			t.Fatalf("%s: %s", id, w.Body())
		}
	}
	// Misbehaviour without tools in the request is a no-op.
	impChatCall(t, e, nil, impBody("g-unknown", ""))
	// A broken-tool request with a tool lacking parameters.
	noParams := `,"tools":[{"type":"function","function":{"name":"read_file"}}],"tool_choice":"required"`
	impChatCall(t, e, nil, impBody("g-enum", noParams))
	badParams := `,"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"bogus-type"}}}],"tool_choice":"required"`
	impChatCall(t, e, nil, impBody("g-enum", badParams))
}

func TestCoverageModeration(t *testing.T) {
	e := newEngine(t, covGenCfg)
	c, _ := impChatCall(t, e, nil, impBody("g-filter", ""))
	if c.finish() != "content_filter" {
		t.Fatalf("finish %q", c.finish())
	}
	c, _ = impChatCall(t, e, nil, impBody("g-refusal", ""))
	if c.content() == "" && c.finish() == "" {
		t.Fatal("refusal")
	}
	c, _ = impChatCall(t, e, map[string]string{"x-wwz-content-filter": "1"}, impBody("g-plain", ""))
	if c.finish() != "content_filter" {
		t.Fatalf("header filter: %q", c.finish())
	}
}
