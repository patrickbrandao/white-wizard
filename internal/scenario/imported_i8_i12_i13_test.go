package scenario

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// I8 (tool_call_id_format), I12 (semantic_match) and I13 (on_exhausted /
// step_limit) at the scenario level (spec 011, ADR 0027).

func mustParse(t *testing.T, src string) *Scenario {
	t.Helper()
	s, err := ParseYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func toolReq(endpoint core.Endpoint, msgs ...core.Message) *core.Request {
	if len(msgs) == 0 {
		msgs = []core.Message{{Role: "user", Content: "go"}}
	}
	return &core.Request{Model: "m", Endpoint: endpoint, Messages: msgs, Tools: []core.ToolDef{{Name: "read_file"}}}
}

const i8Src = "name: i8\ntool_call_id_format: FMT\nsteps:\n  - id: s\n    tool_calls: [{name: read_file, args: {path: /a}}]\n"

func i8Resolve(t *testing.T, format string, ep core.Endpoint) string {
	t.Helper()
	s := mustParse(t, strings.ReplaceAll(i8Src, "FMT", format))
	res, err := (&Resolver{}).Resolve(Selection{Scenario: s}, toolReq(ep), "c")
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("resolve: %v %+v", err, res)
	}
	return res.ToolCalls[0].ID
}

func TestI8EncodedPrefixByEndpoint(t *testing.T) {
	cases := map[core.Endpoint]string{
		core.EndpointChat:      "call_i8_s_0_",
		core.EndpointResponses: "call_i8_s_0_",
		core.EndpointMessages:  "toolu_i8_s_0_",
	}
	for ep, want := range cases {
		for _, format := range []string{"encoded", ""} {
			src := i8Src
			if format == "" {
				src = strings.Replace(src, "tool_call_id_format: FMT\n", "", 1)
			}
			s := mustParse(t, strings.ReplaceAll(src, "FMT", format))
			res, err := (&Resolver{}).Resolve(Selection{Scenario: s}, toolReq(ep), "c")
			if err != nil {
				t.Fatal(err)
			}
			id := res.ToolCalls[0].ID
			if !strings.HasPrefix(id, want) {
				t.Errorf("%s format %q: id %q, want prefix %q", ep, format, id, want)
			}
			if m := callIDRe.FindStringSubmatch(id); m == nil || m[1] != "i8" || m[2] != "s" {
				t.Errorf("%s: id %q does not decode", ep, id)
			}
		}
	}
}

func TestI8NativeLeavesIDToDriver(t *testing.T) {
	if id := i8Resolve(t, "native", core.EndpointMessages); id != "" {
		t.Fatalf("native id = %q, want empty (minted by the driver)", id)
	}
}

func TestI8DecodeBothPrefixes(t *testing.T) {
	for _, id := range []string{"call_flow_read_0_abc", "toolu_flow_read_0_abc"} {
		req := toolReq(core.EndpointChat,
			core.Message{Role: "user", Content: "go"},
			core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: id, Name: "read_file"}}},
		)
		st := DeriveState(req)
		if st.LastScenario != "flow" || st.LastStep != "read" {
			t.Errorf("%s: scenario %q step %q", id, st.LastScenario, st.LastStep)
		}
	}
	// Native ids do not decode.
	for _, id := range []string{"toolu_01AbCdEfGhIjKlMnOpQrStUv", "call_AbCdEfGhIjKlMnOpQrStUvWx"} {
		req := toolReq(core.EndpointChat, core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: id, Name: "read_file"}}})
		if st := DeriveState(req); st.LastStep != "" {
			t.Errorf("native id %q decoded as step %q", id, st.LastStep)
		}
	}
}

func TestI8ValidateFormat(t *testing.T) {
	s := mustParse(t, strings.ReplaceAll(i8Src, "FMT", "opaque"))
	rep := s.Check(CheckOptions{})
	if rep.OK() || !strings.Contains(strings.Join(rep.Errors, "\n"), "tool_call_id_format") {
		t.Fatalf("unknown format accepted: %+v", rep)
	}
	for _, f := range []string{"encoded", "native"} {
		if rep := mustParse(t, strings.ReplaceAll(i8Src, "FMT", f)).Check(CheckOptions{}); !rep.OK() {
			t.Fatalf("%s: %+v", f, rep)
		}
	}
}

func TestI8NativeRejectsLastStep(t *testing.T) {
	src := "name: n\ntool_call_id_format: FMT\nsteps:\n  - id: a\n    expect: {turn: 1}\n    content: x\n  - id: b\n    expect: {last_step: a}\n    content: y\n"
	rep := mustParse(t, strings.ReplaceAll(src, "FMT", "native")).Check(CheckOptions{})
	if rep.OK() || !strings.Contains(strings.Join(rep.Errors, "\n"), "last_step") {
		t.Fatalf("native with last_step accepted: %+v", rep)
	}
	if rep := mustParse(t, strings.ReplaceAll(src, "FMT", "encoded")).Check(CheckOptions{}); !rep.OK() {
		t.Fatalf("encoded with last_step: %+v", rep)
	}
}

// --- I12 ---------------------------------------------------------------

func TestI12SemanticMatchForms(t *testing.T) {
	s := mustParse(t, "name: sm\nsteps:\n  - id: a\n    expect: {semantic_match: \"deploy the service\"}\n    content: x\n  - id: b\n    expect:\n      semantic_match: {text: \"rollback now\", threshold: 0.9}\n    content: y\n")
	a, b := s.Steps[0].Expect.SemanticMatch, s.Steps[1].Expect.SemanticMatch
	if a.Text != "deploy the service" || a.Threshold != DefaultSemanticThreshold {
		t.Fatalf("string form: %+v", a)
	}
	if b.Text != "rollback now" || b.Threshold != 0.9 {
		t.Fatalf("map form: %+v", b)
	}
	if a.vec == nil || b.vec == nil {
		t.Fatal("vectors not pre-computed at load")
	}
}

func TestI12SemanticMatchThreshold(t *testing.T) {
	m := &SemanticMatch{Text: "Deploy the service to production", Threshold: 0.8, explicit: true}
	if err := m.prepare(); err != nil {
		t.Fatal(err)
	}
	if got := m.Similarity("production to service the deploy"); got < 0.999999 {
		t.Fatalf("same words, other order: %v", got)
	}
	if !m.Matches("PRODUCTION: deploy, the service... to") {
		t.Fatal("case and punctuation should not matter")
	}
	// 3 of 5 words shared: cos = 3/sqrt(5*4) ~ 0.67, below 0.8.
	if m.Matches("deploy the service now") {
		t.Fatalf("below the threshold matched (%v)", m.Similarity("deploy the service now"))
	}
	m.Threshold = 0.6
	if !m.Matches("deploy the service now") {
		t.Fatalf("above the threshold did not match (%v)", m.Similarity("deploy the service now"))
	}
	if m.Matches("") || m.Matches("!!!") {
		t.Fatal("empty message matched")
	}
	// Threshold 1 tolerates floating point rounding.
	one := &SemanticMatch{Text: "a b b c", Threshold: 1, explicit: true}
	if !one.Matches("c b a b") {
		t.Fatal("identical bag of words must match at threshold 1")
	}
	// Unvalidated value: default threshold, vector computed on the fly.
	raw := &SemanticMatch{Text: "olá mundo 42"}
	if !raw.Matches("mundo olá 42") || raw.vec != nil {
		t.Fatal("unvalidated semantic_match")
	}
	if (&SemanticMatch{Text: "x", explicit: true}).Matches("x") {
		t.Fatal("explicit zero threshold must not match")
	}
}

func TestI12SemanticMatchValidation(t *testing.T) {
	bad := []string{
		`{text: "a", threshold: 0}`,
		`{text: "a", threshold: 1.5}`,
		`{text: "a", threshold: -0.1}`,
		`{text: "a", threshold: "x"}`,
		`{text: 3}`,
		`{text: "a", limit: 0.5}`,
		`{threshold: 0.5}`,
		`[a, b]`,
	}
	for _, v := range bad {
		src := "name: sm\nsteps:\n  - id: a\n    expect: {semantic_match: " + v + "}\n    content: x\n"
		if _, err := ParseYAML([]byte(src)); err == nil {
			t.Errorf("semantic_match %s accepted", v)
		}
	}
	for _, v := range []string{`{text: "a", threshold: 1}`, `{text: "a", threshold: 0.01}`, `{text: "a", threshold: 1.0}`} {
		src := "name: sm\nsteps:\n  - id: a\n    expect: {semantic_match: " + v + "}\n    content: x\n"
		if _, err := ParseYAML([]byte(src)); err != nil {
			t.Errorf("semantic_match %s: %v", v, err)
		}
	}
}

func TestI12SemanticMatchUnreachable(t *testing.T) {
	s := mustParse(t, "name: sm\nsteps:\n  - id: a\n    expect: {semantic_match: \"x y\"}\n    content: x\n  - id: b\n    expect: {semantic_match: \"x y\"}\n    content: y\n  - id: c\n    expect: {semantic_match: \"z\"}\n    content: z\n")
	rep := s.Check(CheckOptions{})
	errs := strings.Join(rep.Errors, "\n")
	if !strings.Contains(errs, `step "b" is unreachable`) || strings.Contains(errs, `step "c"`) {
		t.Fatalf("unreachable: %+v", rep)
	}
}

func TestI12SemanticMatchIgnoresMarker(t *testing.T) {
	s := mustParse(t, "name: sm\non_no_match: error\nsteps:\n  - id: a\n    expect: {semantic_match: {text: \"deploy service\", threshold: 1}}\n    content: x\n")
	req := toolReq(core.EndpointChat, core.Message{Role: "user", Content: "[[wwz:sm seed=1]] service deploy"})
	res, err := (&Resolver{}).Resolve(Selection{Scenario: s}, req, "c")
	if err != nil || res.Step != "a" {
		t.Fatalf("marker should be ignored: %v %+v", err, res)
	}
}

// --- I13 ---------------------------------------------------------------

func turns(n int) *core.Request {
	msgs := []core.Message{{Role: "user", Content: "go"}}
	for i := 0; i < n; i++ {
		msgs = append(msgs, core.Message{Role: "assistant", Content: "a"}, core.Message{Role: "user", Content: "again"})
	}
	return toolReq(core.EndpointChat, msgs...)
}

func TestI13ExhaustedBeforeMatching(t *testing.T) {
	src := "name: ex\nstep_limit: 2\non_exhausted: MODE\nsteps:\n  - id: a\n    content: first\n  - id: z\n    expect: {turn: 99}\n    content: last\n"
	rv := &Resolver{}
	for mode, check := range map[string]func(*Resolved) bool{
		"repeat_last": func(r *Resolved) bool { return r.Step == "z" && r.Content == "last" },
		"error": func(r *Resolved) bool {
			return r.Error != nil && r.Error.Kind == core.ErrInvalidRequest && r.ExhaustedError
		},
		"fallback": func(r *Resolved) bool { return r.Fallback },
	} {
		s := mustParse(t, strings.ReplaceAll(src, "MODE", mode))
		below, err := rv.Resolve(Selection{Scenario: s}, turns(1), "c")
		if err != nil || below.Step != "a" {
			t.Fatalf("%s below the limit: %v %+v", mode, err, below)
		}
		at, err := rv.Resolve(Selection{Scenario: s}, turns(2), "c")
		if err != nil || !check(at) {
			t.Fatalf("%s at the limit: %v %+v", mode, err, at)
		}
	}
}

func TestI13OnNoMatchRepeatLastIsLastStep(t *testing.T) {
	s := mustParse(t, "name: nm\non_no_match: repeat_last\nsteps:\n  - id: a\n    expect: {turn: 1}\n    content: one\n  - id: b\n    expect: {turn: 2}\n    content: two\n  - id: c\n    expect: {turn: 3}\n    content: three\n")
	res, err := (&Resolver{}).Resolve(Selection{Scenario: s}, turns(5), "c")
	if err != nil || res.Step != "c" {
		t.Fatalf("repeat_last: %v %+v", err, res)
	}
}

// --- tool_misbehavior on a step ------------------------------------------

func TestStepToolMisbehaviorKind(t *testing.T) {
	src := "name: tm\nsteps:\n  - id: a\n    tool_misbehavior: KIND\n    content: x\n"
	if _, err := ParseYAML([]byte(strings.ReplaceAll(src, "KIND", "garbage"))); err == nil || !strings.Contains(err.Error(), "tool_misbehavior") {
		t.Fatalf("unknown kind accepted: %v", err)
	}
	for _, k := range []string{"invalid_json_truncated", "truncated", "length_mid_tool", "unknown_tool"} {
		if _, err := ParseYAML([]byte(strings.ReplaceAll(src, "KIND", k))); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}
