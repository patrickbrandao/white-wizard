package scenario

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

func fileTools() []core.ToolDef {
	p := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}`)
	return []core.ToolDef{
		{Name: "read_file", Parameters: p},
		{Name: "write_file", Parameters: p},
		{Name: "edit_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}`)},
		{Name: "delete_file", Parameters: p},
	}
}

func loadFileFlow(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	loaded, errs := r.LoadDir("../../scenarios")
	if len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(loaded) < 3 {
		t.Fatalf("expected scenarios, got %v", loaded)
	}
	return r
}

func TestFileAgentFlowSequence(t *testing.T) {
	reg := loadFileFlow(t)
	rv := &Resolver{Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 7}
	req := &core.Request{Model: "m", Tools: fileTools(), Messages: []core.Message{{Role: "user", Content: "organise my notes"}}}
	conv := ConversationID(req, "")
	var path string
	expect := []string{"read", "write", "edit", "delete", "done"}
	for i, want := range expect {
		sel, err := reg.Select(req, []string{"header", "metadata", "marker", "model_suffix", "match"}, []string{"file-agent-flow"})
		if err != nil || sel.Scenario == nil {
			t.Fatalf("turn %d: select: %v %+v", i, err, sel)
		}
		res, err := rv.Resolve(sel, req, conv)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if res.Step != want {
			t.Fatalf("turn %d: expected step %s got %s", i, want, res.Step)
		}
		if want != "done" {
			if len(res.ToolCalls) != 1 {
				t.Fatalf("turn %d: expected one tool call, got %+v", i, res.ToolCalls)
			}
			tc := res.ToolCalls[0]
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				t.Fatalf("args not json: %s", tc.Arguments)
			}
			if p, _ := args["path"].(string); want == "read" {
				path = p
				if !strings.HasPrefix(path, "/tmp/") {
					t.Fatalf("path template not rendered: %q", path)
				}
			} else if want == "write" || want == "delete" {
				if p != path {
					t.Fatalf("path differs across steps: %q vs %q", p, path)
				}
			}
			if !strings.HasPrefix(tc.ID, "call_file-agent-flow_"+want+"_") {
				t.Fatalf("tool call id not decodable: %s", tc.ID)
			}
			// Append assistant turn + tool result to the conversation.
			req.Messages = append(req.Messages, core.Message{Role: "assistant", ToolCalls: []core.ToolCall{tc}})
			req.Messages = append(req.Messages, core.Message{Role: "tool", ToolCallID: tc.ID, Content: "ok"})
		} else {
			if res.ContentTokens < 50 || res.ContentTokens > 200 {
				t.Fatalf("done content tokens: %d", res.ContentTokens)
			}
			if res.FinishReason != core.FinishStop {
				t.Fatalf("finish: %s", res.FinishReason)
			}
		}
	}
}

func TestEditFailedBranch(t *testing.T) {
	reg := loadFileFlow(t)
	rv := &Resolver{Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 1}
	req := &core.Request{Model: "m", Tools: fileTools(), Messages: []core.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Name: "write_file", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "permission denied"},
	}}
	sel, _ := reg.Select(req, []string{"match"}, []string{"file-agent-flow"})
	res, err := rv.Resolve(sel, req, "c")
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != "edit-failed" || !strings.Contains(res.Content, "permission denied") {
		t.Fatalf("branch: %+v", res)
	}
}

func TestMarkerAndHeaderSelection(t *testing.T) {
	reg := loadFileFlow(t)
	req := &core.Request{Model: "m", Messages: []core.Message{{Role: "user", Content: "hello [[wwz:retry-on-error seed=5 speed=2]]"}}}
	sel, err := reg.Select(req, []string{"marker"}, nil)
	if err != nil || sel.Scenario == nil || sel.Scenario.Name != "retry-on-error" || sel.Seed != 5 || sel.Speed != 2 || sel.CleanLastMessage != "hello" {
		t.Fatalf("marker: %+v %v", sel, err)
	}
	req2 := &core.Request{Model: "m", Headers: map[string]string{"x-wwz-scenario": "nope"}}
	if _, err := reg.Select(req2, []string{"header"}, nil); err == nil {
		t.Fatal("unknown scenario should error")
	}
	req3 := &core.Request{Model: "wwz-fast-scenario-search-then-answer"}
	sel, _ = reg.Select(req3, []string{"model_suffix"}, nil)
	if sel.Scenario == nil || sel.Scenario.Name != "search-then-answer" {
		t.Fatalf("model suffix: %+v", sel)
	}
}

// TestMarkerLlmsimAlias (I10): [[llmsim:nome seed=N speed=X]] selects the
// scenario with its seed and speed exactly like [[wwz:...]]; so does [[sim:.
func TestMarkerLlmsimAlias(t *testing.T) {
	reg := loadFileFlow(t)
	for _, prefix := range []string{"wwz", "llmsim", "sim"} {
		req := &core.Request{Model: "m", Messages: []core.Message{{Role: "user", Content: "hello [[" + prefix + ":retry-on-error seed=5 speed=2]]"}}}
		sel, err := reg.Select(req, []string{"marker"}, nil)
		if err != nil || sel.Scenario == nil || sel.Scenario.Name != "retry-on-error" || sel.Seed != 5 || sel.Speed != 2 ||
			sel.CleanLastMessage != "hello" || sel.Source != "marker" {
			t.Fatalf("%s: %+v %v", prefix, sel, err)
		}
	}
	req := &core.Request{Model: "m", Messages: []core.Message{{Role: "user", Content: "[[llmsim:nope]]"}}}
	if _, err := reg.Select(req, []string{"marker"}, nil); err == nil {
		t.Fatal("unknown scenario through the alias should error")
	}
}

// TestModelSuffixLastSeparator: a model id containing "-scenario-" keeps it;
// the scenario is the part after the last "-scenario-" (spec 011, task 152).
func TestModelSuffixLastSeparator(t *testing.T) {
	reg := loadFileFlow(t)
	req := &core.Request{Model: "a-scenario-b-scenario-file-agent-flow"}
	sel, err := reg.Select(req, []string{"model_suffix"}, nil)
	if err != nil || sel.Scenario == nil || sel.Scenario.Name != "file-agent-flow" || sel.Source != "model_suffix" {
		t.Fatalf("model suffix: %+v %v", sel, err)
	}
	req.Model = "a-scenario-b-scenario-nope"
	if _, err := reg.Select(req, []string{"model_suffix"}, nil); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("unknown suffix: %v", err)
	}
	req.Model = "-scenario-file-agent-flow"
	sel, err = reg.Select(req, []string{"model_suffix"}, nil)
	if err != nil || sel.Scenario != nil {
		t.Fatalf("leading separator is not a suffix: %+v %v", sel, err)
	}
}

func TestRegistryAddRemoveNames(t *testing.T) {
	r := NewRegistry()
	r.Add(&Scenario{Name: "a"})
	r.Add(&Scenario{Name: "b"})
	r.Add(&Scenario{Name: "a"})
	if got := strings.Join(r.Names(), ","); got != "a,b" {
		t.Fatalf("names: %s", got)
	}
	r.Remove("a")
	r.Remove("missing")
	if got := strings.Join(r.Names(), ","); got != "b" || r.Get("a") != nil {
		t.Fatalf("after remove: %s", got)
	}
}

func TestSelectMetadataAndUnknown(t *testing.T) {
	reg := loadFileFlow(t)
	order := []string{"metadata", "marker"}
	req := &core.Request{Metadata: map[string]string{"user_id": "scenario:retry-on-error"}}
	if sel, err := reg.Select(req, order, nil); err != nil || sel.Source != "metadata" || sel.Scenario.Name != "retry-on-error" {
		t.Fatalf("user_id: %+v %v", sel, err)
	}
	req.Metadata = map[string]string{"scenario": "nope"}
	if _, err := reg.Select(req, order, nil); err == nil {
		t.Fatal("unknown metadata scenario should error")
	}
	req.Metadata = map[string]string{"user_id": "someone"}
	req.Messages = []core.Message{{Role: "user", Content: "[[wwz:nope]]"}}
	if _, err := reg.Select(req, order, nil); err == nil {
		t.Fatal("unknown marker scenario should error")
	}
}

func TestMatchRules(t *testing.T) {
	s := &Scenario{Name: "m", Match: &Match{
		Model:              "gpt-*",
		Driver:             "openai",
		ToolsInclude:       []string{"read_file"},
		UserMessageMatches: "notes",
		SystemMatches:      "^agent",
		Header:             map[string]string{"X-Team": "a"},
	}}
	ok := func() *core.Request {
		return &core.Request{
			Model: "gpt-4o", Driver: "openai", System: "agent mode",
			Tools:    []core.ToolDef{{Name: "read_file"}},
			Messages: []core.Message{{Role: "user", Content: "my notes"}},
			Headers:  map[string]string{"x-team": "a"},
		}
	}
	if !s.matches(ok()) {
		t.Fatal("all rules hold")
	}
	breaks := []func(*core.Request){
		func(r *core.Request) { r.Model = "claude" },
		func(r *core.Request) { r.Driver = "anthropic" },
		func(r *core.Request) { r.Tools = nil },
		func(r *core.Request) { r.Messages[0].Content = "hi" },
		func(r *core.Request) { r.System = "user" },
		func(r *core.Request) { r.Headers = nil },
	}
	for i, b := range breaks {
		r := ok()
		b(r)
		if s.matches(r) {
			t.Fatalf("rule %d should fail", i)
		}
	}
	if s.re("(") != nil {
		t.Fatal("invalid regexp compiles to nil")
	}
	for _, c := range []struct {
		p, s string
		want bool
	}{{"*", "x", true}, {"x", "x", true}, {"*-mini", "gpt-mini", true}, {"*-mini", "gpt", false}, {"a?", "a", false}} {
		if glob(c.p, c.s) != c.want {
			t.Fatalf("glob(%q,%q)", c.p, c.s)
		}
	}
}

func TestFailFirst(t *testing.T) {
	reg := loadFileFlow(t)
	attempts := map[string]int{}
	rv := &Resolver{Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 1, Attempts: func(c, s string) int { attempts[c+s]++; return attempts[c+s] }}
	req := &core.Request{Model: "m", Tools: fileTools(), Messages: []core.Message{{Role: "user", Content: "do it"}}}
	_, _ = reg.Select(req, []string{"header"}, nil)
	req.Headers = map[string]string{"x-wwz-scenario": "retry-on-error"}
	sel, _ := reg.Select(req, []string{"header"}, nil)
	res, _ := rv.Resolve(sel, req, "c1")
	if res.Error == nil || res.Error.Kind != core.ErrRateLimit || res.Error.RetryAfter.Milliseconds() != 500 {
		t.Fatalf("first attempt should fail: %+v", res)
	}
	res, _ = rv.Resolve(sel, req, "c1")
	if res.Error != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("second attempt should succeed: %+v", res)
	}
}

func TestUnknownToolNamedByStep(t *testing.T) {
	s, err := ParseYAML([]byte("name: x\nsteps:\n  - tool_calls: [{name: nope}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	rv := &Resolver{Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions()}
	req := &core.Request{Model: "m", Tools: fileTools(), Messages: []core.Message{{Role: "user", Content: "x"}}}
	_, err = rv.Resolve(Selection{Scenario: s}, req, "c")
	if err == nil {
		t.Fatal("expected error for tool not offered")
	}
}
