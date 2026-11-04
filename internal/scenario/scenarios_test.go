package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

// shippedDir holds the ready-made scenarios of the repository.
const shippedDir = "../../scenarios"

// shippedFinal is the step each shipped scenario must end on. A new file in
// scenarios/ must be added here.
var shippedFinal = map[string]string{
	"file-agent-flow":     "done",
	"long-reasoning":      "answer",
	"multi-tool-parallel": "answer",
	"refusal":             "normal", // the single-turn conversation of runShipped never reaches "refused"
	"retry-on-error":      "answer",
	"search-then-answer":  "answer",
}

func loadShipped(t *testing.T) []*Scenario {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(shippedDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios in %s: %v", shippedDir, err)
	}
	var out []*Scenario
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G304: test fixture path
		if err != nil {
			t.Fatal(err)
		}
		s, err := ParseYAML(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, s)
	}
	return out
}

// offeredTools are the tools a client of the scenario offers: the declared
// and literal ones plus a generic read, fetch and write tool, so any/any_read
// /any_write have more than one candidate.
func offeredTools(s *Scenario) []core.ToolDef {
	names := []string{"read_file", "fetch_url", "write_file"}
	add := func(n string) {
		if n != "" && !strings.Contains(n, "{{") && !contains(genericTools, n) && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if s.Match != nil {
		for _, n := range s.Match.ToolsInclude {
			add(n)
		}
	}
	for _, st := range s.Steps {
		for _, tc := range st.ToolCalls {
			add(tc.Name)
		}
	}
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"url":{"type":"string"},"query":{"type":"string"},"content":{"type":"string"}},"required":["path"]}`)
	out := make([]core.ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, core.ToolDef{Name: n, Parameters: schema})
	}
	return out
}

// runShipped plays a conversation against s like `wwz simulate` does: tool
// calls get a successful result, an injected error is retried with the same
// history. It returns the steps served, stopping at the first text-only
// answer or after maxTurns.
func runShipped(t *testing.T, s *Scenario, seed int64, anthropic bool, maxTurns int) []string {
	t.Helper()
	reg := NewRegistry()
	reg.Add(s)
	attempts := map[string]int{}
	rv := &Resolver{
		Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: seed,
		Attempts: func(conv, step string) int { attempts[conv+"/"+step]++; return attempts[conv+"/"+step] },
	}
	req := &core.Request{
		Model: "m", Tools: offeredTools(s),
		Headers:  map[string]string{"x-wwz-scenario": s.Name},
		Messages: []core.Message{{Role: "user", Content: "find the latest release notes"}},
	}
	conv := ConversationID(req, "")
	var steps []string
	for turn := 0; turn < maxTurns; turn++ {
		sel, err := reg.Select(req, []string{"header"}, nil)
		if err != nil || sel.Scenario == nil {
			t.Fatalf("turn %d: select: %v", turn, err)
		}
		res, err := rv.Resolve(sel, req, conv)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if res.Fallback || res.ExhaustedError {
			t.Fatalf("turn %d: scenario left its steps (fallback=%v exhausted=%v) after %v", turn, res.Fallback, res.ExhaustedError, steps)
		}
		steps = append(steps, res.Step)
		if res.Error != nil {
			continue // retry with the same history
		}
		if len(res.ToolCalls) == 0 {
			return steps
		}
		req.Messages = append(req.Messages, core.Message{Role: "assistant", ToolCalls: res.ToolCalls})
		results := make([]core.ToolResult, 0, len(res.ToolCalls))
		for _, tc := range res.ToolCalls {
			results = append(results, core.ToolResult{ToolCallID: tc.ID, Content: fmt.Sprintf(`{"ok":true,"tool":%q}`, tc.Name)})
		}
		if anthropic {
			req.Messages = append(req.Messages, core.Message{Role: "user", ToolResults: results})
		} else {
			for _, r := range results {
				req.Messages = append(req.Messages, core.Message{Role: "tool", ToolCallID: r.ToolCallID, Content: r.Content})
			}
		}
	}
	return steps
}

// Every shipped scenario passes `wwz validate` with no warnings.
func TestShippedScenariosCheckClean(t *testing.T) {
	for _, s := range loadShipped(t) {
		rep := s.Check(CheckOptions{})
		if len(rep.Errors) > 0 || len(rep.Warnings) > 0 {
			t.Errorf("%s: errors %v warnings %v", s.Name, rep.Errors, rep.Warnings)
		}
	}
}

// Every shipped scenario reaches its final step, whatever tool the generic
// calls pick (several seeds) and in both history dialects.
func TestShippedScenariosReachFinalStep(t *testing.T) {
	for _, s := range loadShipped(t) {
		final, ok := shippedFinal[s.Name]
		if !ok {
			t.Errorf("scenario %q has no expected final step in shippedFinal", s.Name)
			continue
		}
		for _, anthropic := range []bool{false, true} {
			for seed := int64(1); seed <= 32; seed++ {
				steps := runShipped(t, s, seed, anthropic, 2*len(s.Steps)+2)
				if last := steps[len(steps)-1]; last != final {
					t.Fatalf("%s seed %d anthropic=%v: ended on %q, want %q (steps %v)", s.Name, seed, anthropic, last, final, steps)
				}
				if seen := slices.Index(steps, final); seen != len(steps)-1 {
					t.Fatalf("%s seed %d: final step %q reached early (steps %v)", s.Name, seed, final, steps)
				}
			}
		}
	}
}

// search-then-answer runs fetch once even when any_read picks web_search
// again (the step used to repeat forever).
func TestSearchThenAnswerFetchOnce(t *testing.T) {
	var s *Scenario
	for _, sc := range loadShipped(t) {
		if sc.Name == "search-then-answer" {
			s = sc
		}
	}
	if s == nil {
		t.Fatal("search-then-answer not found")
	}
	for seed := int64(1); seed <= 32; seed++ {
		steps := runShipped(t, s, seed, false, 6)
		if want := []string{"search", "fetch", "answer"}; !slices.Equal(steps, want) {
			t.Fatalf("seed %d: steps %v, want %v", seed, steps, want)
		}
	}
}

// multi-tool-parallel emits three tool calls in one response; long-reasoning
// reasons for about 2000 tokens; refusal ends the second user turn with
// content_filter.
func TestEmbeddedScenarioShapes(t *testing.T) {
	byName := map[string]*Scenario{}
	for _, s := range loadShipped(t) {
		byName[s.Name] = s
	}
	rv := &Resolver{Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 1}
	resolve := func(name string, msgs []core.Message) *Resolved {
		t.Helper()
		s := byName[name]
		reg := NewRegistry()
		reg.Add(s)
		req := &core.Request{Model: "m", Tools: offeredTools(s), Headers: map[string]string{"x-wwz-scenario": name}, Messages: msgs}
		sel, err := reg.Select(req, []string{"header"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := rv.Resolve(sel, req, ConversationID(req, ""))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	user := core.Message{Role: "user", Content: "hello"}

	if res := resolve("multi-tool-parallel", []core.Message{user}); len(res.ToolCalls) != 3 {
		t.Errorf("multi-tool-parallel: %d tool calls, want 3", len(res.ToolCalls))
	}
	if res := resolve("long-reasoning", []core.Message{user}); res.ReasoningTokens < 1800 || res.ReasoningTokens > 2200 || len(res.ToolCalls) != 1 {
		t.Errorf("long-reasoning: reasoning %d tool calls %d", res.ReasoningTokens, len(res.ToolCalls))
	}
	second := []core.Message{user, {Role: "assistant", Content: "hi"}, {Role: "user", Content: "again"}}
	if res := resolve("refusal", []core.Message{user}); res.Step != "normal" || res.FinishReason != core.FinishStop {
		t.Errorf("refusal turn 1: %s %s", res.Step, res.FinishReason)
	}
	if res := resolve("refusal", second); res.Step != "refused" || res.FinishReason != core.FinishContentFilter {
		t.Errorf("refusal turn 2: %s %s", res.Step, res.FinishReason)
	}
}
