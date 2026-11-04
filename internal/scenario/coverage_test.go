package scenario

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

func userReq(msg string, tools ...core.ToolDef) *core.Request {
	return &core.Request{Model: "m", Tools: tools, Messages: []core.Message{{Role: "user", Content: msg}}}
}

func resolveOne(t *testing.T, s *Scenario, req *core.Request) *Resolved {
	t.Helper()
	rv := &Resolver{ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 3}
	res, err := rv.Resolve(Selection{Scenario: s}, req, "conv")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

func TestConversationIDHeaders(t *testing.T) {
	req := userReq("hi")
	req.Headers = map[string]string{"x-custom-conv": "abc", "x-session-id": "sess"}
	if got := ConversationID(req, "x-custom-conv"); got != "abc" {
		t.Fatalf("configured header = %q", got)
	}
	// Configured header absent: well-known headers apply in order.
	if got := ConversationID(req, "x-missing"); got != "sess" {
		t.Fatalf("well-known header = %q", got)
	}
	req.Headers = nil
	if got := ConversationID(req, ""); !strings.HasPrefix(got, "conv-") {
		t.Fatalf("hash id = %q", got)
	}
}

func TestLoadDirErrors(t *testing.T) {
	r := NewRegistry()
	if _, errs := r.LoadDir(filepath.Join(t.TempDir(), "absent")); len(errs) != 1 {
		t.Fatalf("missing dir: %v", errs)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yml"), []byte("name: x\nsteps: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.yaml"), []byte("name: ok\nsteps:\n  - content: hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, errs := r.LoadDir(dir)
	if len(loaded) != 1 || loaded[0] != "ok" {
		t.Fatalf("loaded = %v", loaded)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v", errs)
	}
}

func TestSelectToolCallIDPinsScenario(t *testing.T) {
	r := NewRegistry()
	r.Add(mustParse(t, "name: pin\nmatch:\n  model: other\nsteps:\n  - content: x\n"))
	req := userReq("go")
	req.Messages = append(req.Messages, core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_pin_step1_0_abc", Name: "t"}}})
	sel, err := r.Select(req, []string{"match"}, []string{"pin"})
	if err != nil || sel.Scenario == nil || sel.Source != "tool_call_id" {
		t.Fatalf("sel = %+v err=%v", sel, err)
	}
	// An id naming a scenario that is not loaded selects nothing.
	req.Messages[1].ToolCalls[0].ID = "call_gone_step1_0_abc"
	sel, err = r.Select(req, []string{"match"}, nil)
	if err != nil || sel.Scenario != nil {
		t.Fatalf("sel = %+v err=%v", sel, err)
	}
}

func TestMatchesModelMismatch(t *testing.T) {
	s := mustParse(t, "name: m\nmatch:\n  model: gpt-*\nsteps:\n  - content: x\n")
	if s.matches(userReq("x")) {
		t.Fatal("model m must not match gpt-*")
	}
	if !glob("*-mini", "gpt-mini") || glob("a*b", "ab") {
		t.Fatal("glob suffix/other forms")
	}
}

func TestStepExpectConditions(t *testing.T) {
	s := mustParse(t, `
name: e
steps:
  - id: only
    expect:
      always: false
    content: x
`)
	one, three := 1, 3
	toolReq := userReq("hello world", core.ToolDef{Name: "t"})
	toolReq.Headers = map[string]string{"x-k": "v"}
	state := DeriveState(toolReq)
	state.ToolResults = 2
	state.LastTool = "t"
	state.LastStep = "s1"
	state.LastToolResult = "ok"
	state.LatestRole = "user"
	state.ToolResultsFor = map[string]int{"t": 1}
	cases := []struct {
		name string
		e    Expect
		want bool
	}{
		{"turn ok", Expect{Turn: &one}, true},
		{"turn_gte fail", Expect{TurnGte: &three}, false},
		{"tool_results fail", Expect{ToolResults: &three}, false},
		{"tool_results_gte fail", Expect{ToolResultsGte: &three}, false},
		{"last_tool fail", Expect{LastTool: "other"}, false},
		{"last_step fail", Expect{LastStep: "s2"}, false},
		{"tool_result_for fail", Expect{ToolResultFor: "none"}, false},
		{"latest role fail", Expect{LatestMessageRole: "assistant"}, false},
		{"model fail", Expect{Model: "gpt-*"}, false},
		{"last result regex fail", Expect{LastToolResultMatches: "^bad$"}, false},
		{"user regex fail", Expect{UserMessageMatches: "^bye"}, false},
		{"semantic fail", Expect{SemanticMatch: &SemanticMatch{Text: "completely unrelated words", Threshold: 0.9}}, false},
		{"header fail", Expect{Header: map[string]string{"X-K": "other"}}, false},
		{"header ok", Expect{Header: map[string]string{"X-K": "v"}}, true},
	}
	for _, c := range cases {
		e := c.e
		if e.SemanticMatch != nil {
			if err := e.SemanticMatch.prepare(); err != nil {
				t.Fatal(err)
			}
		}
		if got := s.stepMatches(&Step{Expect: &e}, &state, toolReq); got != c.want {
			t.Fatalf("%s: got %v", c.name, got)
		}
	}
}

func TestResolveSeedAndSpeedOverrides(t *testing.T) {
	s := mustParse(t, "name: o\nsteps:\n  - content: '{{ rand_int 1 1000000 }}'\n    speed: 2\n")
	rv := &Resolver{}
	// No global seed, no selection seed: the seed derives from the conversation.
	a, err := rv.Resolve(Selection{Scenario: s}, userReq("x"), "c1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := rv.Resolve(Selection{Scenario: s}, userReq("x"), "c1")
	if a.Content != b.Content || a.Speed != 2 {
		t.Fatalf("not deterministic: %q %q speed=%v", a.Content, b.Content, a.Speed)
	}
	// Selection seed and speed win.
	c, _ := rv.Resolve(Selection{Scenario: s, Seed: 99, Speed: 5}, userReq("x"), "c1")
	d, _ := rv.Resolve(Selection{Scenario: s, Seed: 99}, userReq("x"), "c1")
	if c.Content != d.Content || c.Speed != 5 {
		t.Fatalf("selection seed/speed ignored: %q %q speed=%v", c.Content, d.Content, c.Speed)
	}
	// A var that does not parse fails the resolution.
	bad := mustParse(t, "name: v\nvars:\n  x: '{{ nope'\nsteps:\n  - content: hi\n")
	if _, err := rv.Resolve(Selection{Scenario: bad}, userReq("x"), "c"); err == nil {
		t.Fatal("expected template error from vars")
	}
}

func TestChooseStepExhaustionAndNoMatch(t *testing.T) {
	doc := func(onEx, onNo string) *Scenario {
		return mustParse(t, "name: x\nstep_limit: 1\non_exhausted: "+onEx+"\non_no_match: "+onNo+
			"\nsteps:\n  - id: a\n    expect:\n      turn: 9\n    content: A\n  - id: b\n    expect:\n      turn: 9\n    content: B\n")
	}
	exhausted := userReq("x")
	exhausted.Messages = append(exhausted.Messages, core.Message{Role: "assistant", Content: "r"}, core.Message{Role: "user", Content: "y"})
	if r := resolveOne(t, doc("error", "fallback"), exhausted); r.Error == nil || !r.ExhaustedError {
		t.Fatalf("exhausted error = %+v", r)
	}
	if r := resolveOne(t, doc("fallback", "fallback"), exhausted); !r.Fallback {
		t.Fatalf("exhausted fallback = %+v", r)
	}
	if r := resolveOne(t, doc("repeat_last", "fallback"), exhausted); r.Step != "b" {
		t.Fatalf("exhausted repeat_last = %+v", r)
	}
	fresh := userReq("x")
	if r := resolveOne(t, doc("repeat_last", "repeat_last"), fresh); r.Step != "b" {
		t.Fatalf("no match repeat_last = %+v", r)
	}
	if r := resolveOne(t, doc("repeat_last", "error"), fresh); r.Error == nil {
		t.Fatalf("no match error = %+v", r)
	}
	if r := resolveOne(t, doc("repeat_last", "fallback"), fresh); !r.Fallback {
		t.Fatalf("no match fallback = %+v", r)
	}
}

func TestChooseStepWeighted(t *testing.T) {
	s := mustParse(t, "name: w\nsteps:\n  - id: a\n    weight: 1\n    content: A\n  - id: b\n    content: B\n  - id: c\n    weight: 2\n    content: C\n")
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		rv := &Resolver{GlobalSeed: int64(i + 1)}
		res, err := rv.Resolve(Selection{Scenario: s}, userReq("x"), "c")
		if err != nil {
			t.Fatal(err)
		}
		seen[res.Step] = true
	}
	if !seen["a"] || !seen["b"] || !seen["c"] {
		t.Fatalf("weighted draw missed steps: %v", seen)
	}
	// Float rounding can leave x > 0 after the loop; the last candidate is the fallback.
	st := &State{ToolResultsFor: map[string]int{}}
	rng := generate.NewRNG(1, "w")
	step, res := s.chooseStep(st, userReq("x"), rng)
	if step == nil || res != nil {
		t.Fatalf("choose = %v %v", step, res)
	}
}

func TestRenderFailFirstAndErrors(t *testing.T) {
	s := mustParse(t, `
name: f
steps:
  - id: ff
    expect:
      turn: 1
    fail_first:
      times: 2
      retry_after_ms: 1500
    content: ok
  - id: err
    expect:
      turn: 5
    error: overloaded
    content: no
  - id: custom
    expect:
      turn: 6
    error: rate_limit
    error_message: slow down
    content: no
`)
	rv := &Resolver{Attempts: func(conv, step string) int { return 1 }}
	res, err := rv.Resolve(Selection{Scenario: s}, userReq("x"), "c")
	if err != nil || res.Error == nil || res.Error.RetryAfter.Milliseconds() != 1500 || res.Error.Message == "" {
		t.Fatalf("fail_first = %+v %v", res, err)
	}
	if res.Attempt != 1 {
		t.Fatalf("attempt = %d", res.Attempt)
	}
	req := userReq("x")
	for i := 0; i < 4; i++ {
		req.Messages = append(req.Messages, core.Message{Role: "assistant", Content: "a"}, core.Message{Role: "user", Content: "u"})
	}
	res = resolveOne(t, s, req)
	if res.Error == nil || res.Error.Message != "scenario-injected overloaded" {
		t.Fatalf("step error = %+v", res.Error)
	}
	req.Messages = append(req.Messages, core.Message{Role: "assistant", Content: "a"}, core.Message{Role: "user", Content: "u"})
	res = resolveOne(t, s, req)
	if res.Error == nil || res.Error.Message != "slow down" {
		t.Fatalf("custom error = %+v", res.Error)
	}
}

func TestRenderTemplateErrorsAndTokens(t *testing.T) {
	rv := &Resolver{GlobalSeed: 2}
	for _, field := range []string{"reasoning", "content"} {
		s := &Scenario{Name: "t", Steps: []Step{{ID: "s"}}}
		switch field {
		case "reasoning":
			s.Steps[0].Reasoning = "{{ nope"
		default:
			s.Steps[0].Content = "{{ nope"
		}
		s.OnNoMatch, s.OnExhausted, s.StepLimit = "fallback", "repeat_last", 50
		if _, err := rv.Resolve(Selection{Scenario: s}, userReq("x"), "c"); err == nil {
			t.Fatalf("%s: expected template error", field)
		}
	}
	s := mustParse(t, "name: tk\nsteps:\n  - reasoning:\n      tokens: 5..9\n    content:\n      tokens: 20\n")
	res := resolveOne(t, s, userReq("x"))
	if res.ReasoningTokens < 5 || res.ReasoningTokens > 9 || res.ContentTokens != 20 {
		t.Fatalf("tokens = %d %d", res.ReasoningTokens, res.ContentTokens)
	}
	// Tool call errors propagate.
	s = mustParse(t, "name: te\nsteps:\n  - tool_calls:\n      - name: nothere\n")
	rvs := &Resolver{}
	if _, err := rvs.Resolve(Selection{Scenario: s}, userReq("x"), "c"); err == nil {
		t.Fatal("expected unknown-tool error")
	}
}

func TestToolCallResolution(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"n":{"type":"integer"}},"required":["path"]}`)
	tools := []core.ToolDef{
		{Name: "read_x", Parameters: schema},
		{Name: "write_x", Parameters: schema},
		{Name: "plain"},
		{Name: "badschema", Parameters: json.RawMessage(`{`)},
	}
	rv := &Resolver{ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 4}
	run := func(doc string, req *core.Request) (*Resolved, error) {
		return rv.Resolve(Selection{Scenario: mustParse(t, doc)}, req, "c")
	}
	req := userReq("x", tools...)

	// any_read / any_write / any pick from their pools.
	for name, want := range map[string]string{"any_read": "read_x", "any_write": "write_x"} {
		res, err := run("name: p\nsteps:\n  - tool_calls:\n      - name: "+name+"\n        args: {}\n", req)
		if err != nil || res.ToolCalls[0].Name != want {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	res, err := run("name: p\nsteps:\n  - tool_calls:\n      - name: any\n        args: {}\n", req)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("any: %+v %v", res, err)
	}
	// Empty pool falls back to every offered tool; no tools skips the call.
	onlyPlain := userReq("x", core.ToolDef{Name: "plain"})
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: any_read\n", onlyPlain)
	if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("any_read fallback: %+v %v", res, err)
	}
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: any\n", userReq("x"))
	if err != nil || len(res.ToolCalls) != 0 {
		t.Fatalf("no tools: %+v %v", res, err)
	}
	// Absent args generate the required fields from the schema.
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: read_x\n", req)
	if err != nil || !strings.Contains(res.ToolCalls[0].Arguments, "path") {
		t.Fatalf("schema args: %+v %v", res, err)
	}
	// schema and schema+ (with a templated and a literal overlay).
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: read_x\n        args: schema+\n        overlay:\n          path: '{{ upper \"abc\" }}'\n          n: 7\n", req)
	if err != nil || !strings.Contains(res.ToolCalls[0].Arguments, `"path":"ABC"`) || !strings.Contains(res.ToolCalls[0].Arguments, `"n":7`) {
		t.Fatalf("schema+: %+v %v", res, err)
	}
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: read_x\n        args: schema\n", req)
	if err != nil || res.ToolCalls[0].Arguments == "" {
		t.Fatalf("schema: %+v %v", res, err)
	}
	if _, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: badschema\n        args: schema\n", req); err == nil {
		t.Fatal("schema on invalid parameters must fail")
	}
	if _, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: read_x\n        args: schema+\n        overlay:\n          path: '{{ nope'\n", req); err == nil {
		t.Fatal("overlay template error must fail")
	}
	// String args are rendered as a template; map args render string values only.
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        args: '{\"a\":\"{{ lower \"X\" }}\"}'\n", req)
	if err != nil || res.ToolCalls[0].Arguments != `{"a":"x"}` {
		t.Fatalf("string args: %+v %v", res, err)
	}
	if _, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        args: '{{ nope'\n", req); err == nil {
		t.Fatal("string args template error must fail")
	}
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        args:\n          a: '{{ lower \"X\" }}'\n          b: 3\n", req)
	if err != nil || !strings.Contains(res.ToolCalls[0].Arguments, `"a":"x"`) || !strings.Contains(res.ToolCalls[0].Arguments, `"b":3`) {
		t.Fatalf("map args: %+v %v", res, err)
	}
	if _, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        args:\n          a: '{{ nope'\n", req); err == nil {
		t.Fatal("map args template error must fail")
	}
	// Any other args form is marshalled as is.
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        args: [1, 2]\n", req)
	if err != nil || res.ToolCalls[0].Arguments != "[1,2]" {
		t.Fatalf("list args: %+v %v", res, err)
	}
	// The tool name itself is templated, and a bad template fails.
	if _, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: '{{ nope'\n", req); err == nil {
		t.Fatal("name template error must fail")
	}
	// Non-parallel steps stop after the first call; Messages routes get toolu_ ids; literal ids win.
	req.Endpoint = core.EndpointMessages
	res, err = run("name: p\nsteps:\n  - parallel: false\n    tool_calls:\n      - name: plain\n        args: {}\n      - name: read_x\n        args: {}\n", req)
	if err != nil || len(res.ToolCalls) != 1 || !strings.HasPrefix(res.ToolCalls[0].ID, "toolu_") {
		t.Fatalf("parallel false: %+v %v", res, err)
	}
	res, err = run("name: p\nsteps:\n  - tool_calls:\n      - name: plain\n        id: mine\n        args: {}\n", req)
	if err != nil || res.ToolCalls[0].ID != "mine" {
		t.Fatalf("literal id: %+v %v", res, err)
	}
	res, err = run("name: p\ntool_call_id_format: native\nsteps:\n  - tool_calls:\n      - name: plain\n        args: {}\n", req)
	if err != nil || res.ToolCalls[0].ID != "" {
		t.Fatalf("native id: %+v %v", res, err)
	}
}

func TestStripMarkerAndSanitize(t *testing.T) {
	if got := stripMarker("plain"); got != "plain" {
		t.Fatalf("no marker: %q", got)
	}
	if got := stripMarker("hello [[wwz:abc seed=1]] there"); got != "hello  there" {
		t.Fatalf("marker: %q", got)
	}
	if got := sanitize("a b_c.d-E9"); got != "a-b-c-d-E9" {
		t.Fatalf("sanitize: %q", got)
	}
	// The semantic match sees the message without the marker.
	st := Step{Expect: &Expect{SemanticMatch: &SemanticMatch{Text: "refund my order", Threshold: 0.5}}}
	if err := st.Expect.SemanticMatch.prepare(); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{}
	state := DeriveState(userReq("[[wwz:x]] refund my order"))
	if !s.stepMatches(&st, &state, userReq("[[wwz:x]] refund my order")) {
		t.Fatal("marker must be stripped before the semantic match")
	}
}

func TestTemplateFunctions(t *testing.T) {
	s := mustParse(t, "name: fn\nsteps:\n  - content: '{{ rand_word }}|{{ rand_float 1.0 2.0 }}|{{ lorem 3 }}|{{ text 4 }}|{{ uuid }}|{{ now }}|{{ pick \"a\" \"b\" }}|{{ last_user_message }}|{{ json .vars }}|{{ upper \"x\" }}'\n")
	res := resolveOne(t, s, userReq("question"))
	parts := strings.Split(res.Content, "|")
	if len(parts) != 10 || parts[7] != "question" || parts[9] != "X" || parts[8] != "{}" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(parts[4]) != 36 {
		t.Fatalf("uuid = %q", parts[4])
	}
	// A runtime error in a template (wrong argument count) surfaces.
	bad := &tplContext{Req: userReq("x"), State: &State{}, rng: generate.NewRNG(1, "x"), tk: generate.Simulated{}}
	if _, err := bad.render("{{ pick }}"); err == nil {
		t.Fatal("runtime template error must fail")
	}
	if _, err := bad.render(`{{ template "missing" }}`); err == nil {
		t.Fatal("execution error must fail")
	}
}

func TestParseTokenRangeForms(t *testing.T) {
	ok := []struct {
		in       any
		min, max int
		lit      string
	}{
		{nil, 0, 0, ""},
		{"hi", 0, 0, "hi"},
		{3, 3, 3, ""},
		{uint64(4), 4, 4, ""},
		{float64(5), 5, 5, ""},
		{map[string]any{"tokens": "10..20"}, 10, 20, ""},
		{map[string]any{"tokens": "7"}, 7, 7, ""},
		{map[string]any{"tokens": 8}, 8, 8, ""},
		{map[string]any{"tokens": uint64(9)}, 9, 9, ""},
		{map[string]any{"tokens": float64(11)}, 11, 11, ""},
		{map[string]any{"text": "lit"}, 0, 0, "lit"},
	}
	for _, c := range ok {
		got, err := parseTokenRange(c.in)
		if err != nil || got.Min != c.min || got.Max != c.max || got.Literal != c.lit {
			t.Fatalf("%v: %+v %v", c.in, got, err)
		}
	}
	if got, _ := parseTokenRange(uint64(1) << 63); got.Min <= 0 {
		t.Fatalf("clamp: %+v", got)
	}
	if got, _ := parseTokenRange(map[string]any{"tokens": uint64(1) << 63}); got.Min <= 0 {
		t.Fatalf("clamp in map: %+v", got)
	}
	for _, in := range []any{
		map[string]any{"tokens": "x"},
		map[string]any{"tokens": "1..x"},
		map[string]any{"tokens": true},
		map[string]any{"text": 3},
		map[string]any{},
		true,
	} {
		if _, err := parseTokenRange(in); err == nil {
			t.Fatalf("%v: expected error", in)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"name required":       "steps:\n  - content: x\n",
		"no steps":            "name: a\n",
		"bad match user":      "name: a\nmatch:\n  user_message_matches: '('\nsteps:\n  - content: x\n",
		"bad match system":    "name: a\nmatch:\n  system_matches: '('\nsteps:\n  - content: x\n",
		"duplicate id":        "name: a\nsteps:\n  - id: s\n    content: x\n  - id: s\n    content: y\n",
		"bad last result re":  "name: a\nsteps:\n  - expect:\n      last_tool_result_matches: '('\n    content: x\n",
		"bad user re":         "name: a\nsteps:\n  - expect:\n      user_message_matches: '('\n    content: x\n",
		"bad semantic":        "name: a\nsteps:\n  - expect:\n      semantic_match:\n        text: hi\n        threshold: 2\n    content: x\n",
		"bad reasoning":       "name: a\nsteps:\n  - reasoning:\n      tokens: zz\n    content: x\n",
		"bad content":         "name: a\nsteps:\n  - content:\n      tokens: zz\n",
		"bad misbehavior":     "name: a\nsteps:\n  - tool_misbehavior: nonsense\n    content: x\n",
		"tool name required":  "name: a\nsteps:\n  - tool_calls:\n      - args: {}\n",
		"goto unknown":        "name: a\nsteps:\n  - goto: nowhere\n    content: x\n",
		"semantic bad type":   "name: a\nsteps:\n  - expect:\n      semantic_match: 3\n    content: x\n",
		"semantic text type":  "name: a\nsteps:\n  - expect:\n      semantic_match:\n        text: 3\n    content: x\n",
		"semantic thr type":   "name: a\nsteps:\n  - expect:\n      semantic_match:\n        text: hi\n        threshold: abc\n    content: x\n",
		"semantic unknown":    "name: a\nsteps:\n  - expect:\n      semantic_match:\n        text: hi\n        other: 1\n    content: x\n",
		"semantic yaml error": "name: a\nsteps:\n  - expect: [\n",
	}
	for name, doc := range cases {
		if _, err := ParseYAML([]byte(doc)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestRegexLazyCompileAndToFloat(t *testing.T) {
	s := &Scenario{}
	if s.re("") != nil {
		t.Fatal("empty pattern")
	}
	if s.re("(") != nil {
		t.Fatal("invalid pattern must yield nil")
	}
	re := s.re("^a+$")
	if re == nil || s.re("^a+$") != re {
		t.Fatal("pattern must compile once and be cached")
	}
	for _, v := range []any{1.5, 2, int64(3), uint64(4)} {
		if _, ok := toFloat(v); !ok {
			t.Fatalf("toFloat(%T)", v)
		}
	}
	if _, ok := toFloat("x"); ok {
		t.Fatal("toFloat(string)")
	}
}
