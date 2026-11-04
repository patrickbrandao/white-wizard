package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

const scenarioCfg = `
server: {seed: 42}
drivers: [{name: openai}, {name: anthropic}]
profiles:
  latency:
    zero: {ttft: 0ms, itl: 0ms}
  generation:
    short:
      output_tokens: {dist: fixed, value: 8}
      reasoning_tokens: {min: 0, max: 0, probability: 0}
      tool_calls: {probability: 0, optional_param_probability: 1}
models:
  - {id: agent, protocols: [openai, anthropic], latency: zero, generation: short, errors: none,
     scenarios: [file-agent-flow, search-then-answer]}
`

// startScenarioWWZ serves the wwz in process with the scenarios of
// ./scenarios selected by match.
func startScenarioWWZ(t *testing.T) string {
	t.Helper()
	c, err := config.Parse([]byte(scenarioCfg))
	if err != nil {
		t.Fatal(err)
	}
	reg := scenario.NewRegistry()
	if _, errs := reg.LoadDir("../../scenarios"); len(errs) > 0 {
		t.Fatal(errs)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(c, limits.NewMemory(), reg, engine.NewMetrics(metrics.New(1000), nil), log)
	s, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

// TestConversationSeedStep (ADR 0033): conversation i sends x-wwz-seed
// Seed + SeedStep·i.
func TestConversationSeedStep(t *testing.T) {
	url, seeds := seedServer(t)
	s, turns := loadScenarioFile(t, "file-agent-flow.yaml")
	plan := convPlan(url, OpenAI, false, s, turns)
	plan.Template.URL, plan.Seed, plan.SeedStep = url, 2, 3
	runConversations(t, plan, 3)
	if got := seeds(); !slices.Equal(got, []string{"2", "5", "8"}) {
		t.Fatal(got)
	}
}

func loadScenarioFile(t *testing.T, name string) (*scenario.Scenario, int) {
	t.Helper()
	data, err := os.ReadFile("../../scenarios/" + name)
	if err != nil {
		t.Fatal(err)
	}
	s, turns, err := LoadScenario(data)
	if err != nil {
		t.Fatal(err)
	}
	return s, turns
}

func convPlan(base string, p Protocol, stream bool, s *scenario.Scenario, turns int) ConversationPlan {
	return ConversationPlan{
		Scenario: s, MaxTurns: turns, Seed: 7, DrawSeed: 7, Prompt: "faça o trabalho",
		Template: Request{Protocol: p, URL: base + p.DefaultPath(), Key: "wwz-key-01", Model: "agent", Stream: stream, MaxTokens: 512},
	}
}

func runConversations(t *testing.T, plan ConversationPlan, n int) (*RunResult, *ConversationReport) {
	t.Helper()
	unit, convs, err := NewConversationUnit(NewClient(Options{Timeout: 10 * time.Second}), plan)
	if err != nil {
		t.Fatal(err)
	}
	res := mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: n, Requests: int64(n)}, unit)
	return res, convs.Report()
}

// TestConversationScenariosAgainstWWZ: file-agent-flow (read → write → edit
// → delete → answer) and search-then-answer complete in both protocols,
// with and without stream, preserving ids, order and reasoning.
func TestConversationScenariosAgainstWWZ(t *testing.T) {
	base := startScenarioWWZ(t)
	cases := []struct {
		file             string
		turns, toolCalls int
	}{{"file-agent-flow.yaml", 5, 4}, {"search-then-answer.yaml", 3, 2}}
	for _, c := range cases {
		for _, p := range []Protocol{OpenAI, Anthropic} {
			for _, stream := range []bool{true, false} {
				t.Run(c.file+"/"+string(p)+"/"+map[bool]string{true: "stream", false: "nostream"}[stream], func(t *testing.T) {
					t.Parallel()
					s, turns := loadScenarioFile(t, c.file)
					const n = 3
					res, rep := runConversations(t, convPlan(base, p, stream, s, turns), n)
					tot := res.Total.Stats
					if tot.Requests != int64(n*c.turns) || tot.Errors != 0 || (p == OpenAI && tot.ReasoningTokens == 0) {
						t.Fatalf("requests %d errors %v reasoning %d", tot.Requests, tot.ErrorsByType, tot.ReasoningTokens)
					}
					if rep.Started != n || rep.Completed != n || rep.Failed != 0 || rep.Interrupted != 0 ||
						rep.ToolCalls != int64(n*c.toolCalls) || rep.Turns.P50 != float64(c.turns) || rep.Turns.Max != float64(c.turns) ||
						rep.Turns.Count != n || rep.E2E.Count != n {
						t.Fatalf("report %+v", rep)
					}
					if rep.OrderViolations != 0 || rep.IDMismatches != 0 || rep.ReasoningMissing != 0 {
						t.Fatalf("checks %+v", rep)
					}
				})
			}
		}
	}
}

// TestConversationToolErrorsAgainstWWZ: with every tool result an error the
// file agent keeps retrying the write (edit-failed) until --max-turns; the
// prediction follows it (no order violation).
func TestConversationToolErrorsAgainstWWZ(t *testing.T) {
	base := startScenarioWWZ(t)
	for _, p := range []Protocol{OpenAI, Anthropic} {
		s, _ := loadScenarioFile(t, "file-agent-flow.yaml")
		plan := convPlan(base, p, true, s, 4)
		plan.ToolResultErrorRate = 100
		_, rep := runConversations(t, plan, 2)
		if rep.Failed != 2 || rep.MaxTurns != 2 || rep.FailedByReason[ReasonMaxTurns] != 2 || rep.Turns.Max != 4 || rep.E2E.Count != 0 {
			t.Fatalf("%s: %+v", p, rep)
		}
		if rep.OrderViolations != 0 || rep.IDMismatches != 0 || rep.ReasoningMissing != 0 {
			t.Fatalf("%s checks %+v", p, rep)
		}
	}
}

// fakeTurns serves scripted OpenAI non-stream responses by turn (number of
// assistant messages in the request).
func fakeTurns(t *testing.T, bodies ...string) (string, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	url := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		turn := 0
		for _, m := range req.Messages {
			if m.Role == "assistant" {
				turn++
			}
		}
		b := bodies[min(turn, len(bodies)-1)]
		if strings.HasPrefix(b, "500") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"x"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b))
	})
	return url, &calls
}

func oaResp(finish, reasoning string, calls ...[2]string) string {
	var tcs []string
	for _, c := range calls {
		tcs = append(tcs, `{"id":"`+c[0]+`","type":"function","function":{"name":"`+c[1]+`","arguments":"{}"}}`)
	}
	msg := `{"role":"assistant","content":"x"`
	if reasoning != "" {
		msg += `,"reasoning_content":"` + reasoning + `"`
	}
	if len(tcs) > 0 {
		msg += `,"tool_calls":[` + strings.Join(tcs, ",") + `]`
	}
	return `{"choices":[{"message":` + msg + `},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
}

const fileFlow = `
name: flow
match: {tools_include: [read_file, write_file]}
steps:
  - id: read
    expect: {tool_results: 0}
    reasoning: {tokens: "5..9"}
    tool_calls: [{name: read_file}]
  - id: write
    expect: {last_tool: read_file, tool_results: 1}
    tool_calls: [{name: write_file}, {name: any}]
  - id: done
    expect: {tool_results_gte: 3}
    finish_reason: stop
`

func fakePlan(t *testing.T, url, yml string, turns int) ConversationPlan {
	t.Helper()
	s, def, err := LoadScenario([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	if turns == 0 {
		turns = def
	}
	return ConversationPlan{Scenario: s, MaxTurns: turns, Template: Request{Protocol: OpenAI, URL: url + "/v1/chat/completions", Model: "m", MaxTokens: 16}}
}

// TestConversationChecksAndOutcomes: violations detected against a fake
// server, and each way a conversation ends.
func TestConversationChecksAndOutcomes(t *testing.T) {
	good := []string{
		oaResp("tool_calls", "pensando", [2]string{"call_flow_read_0_a", "read_file"}),
		oaResp("tool_calls", "", [2]string{"call_flow_write_0_b", "write_file"}, [2]string{"call_flow_write_1_c", "read_file"}),
		oaResp("stop", ""),
	}
	url, _ := fakeTurns(t, good...)
	_, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 2)
	if rep.Completed != 2 || rep.OrderViolations+rep.IDMismatches+rep.ReasoningMissing != 0 || rep.ToolCalls != 6 {
		t.Fatalf("good %+v", rep)
	}

	// Missing reasoning, wrong name, index out of order, duplicated and
	// empty ids; ids of another scenario are only checked for uniqueness.
	bad := []string{
		oaResp("tool_calls", "", [2]string{"call_flow_read_1_a", "write_file"}),
		oaResp("tool_calls", "", [2]string{"call_flow_read_1_a", "write_file"}, [2]string{"", "read_file"}),
		oaResp("tool_calls", "", [2]string{"call_other_x_5_a", "write_file"}, [2]string{"call_other_x_5_a", "write_file"}),
		oaResp("stop", ""),
	}
	url, _ = fakeTurns(t, bad...)
	_, rep = runConversations(t, fakePlan(t, url, fileFlow, 0), 1)
	// Turn 1: decoded step read → reasoning missing, name and index wrong.
	// Turn 2: decoded read again (count 2 ≠ 1) → order; empty id.
	// Turn 3: other scenario's ids, duplicated → predicted done (3
	// results), which has no tool calls → order. Turn 4: done, as predicted.
	if rep.Completed != 1 || rep.ReasoningMissing != 2 || rep.OrderViolations != 3 || rep.IDMismatches != 2 {
		t.Fatalf("bad %+v", rep)
	}

	for _, c := range []struct {
		bodies []string
		reason string
	}{
		{[]string{oaResp("tool_calls", "")}, ReasonNoToolCalls},
		{[]string{oaResp("content_filter", "")}, ReasonFinishPrefix + "content_filter"},
		{[]string{"500"}, "http_500/server_error"},
	} {
		url, _ := fakeTurns(t, c.bodies...)
		_, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 1)
		if rep.Failed != 1 || rep.FailedByReason[c.reason] != 1 || rep.Turns.Max != 1 {
			t.Fatalf("%s: %+v", c.reason, rep)
		}
	}

	// Interrupt before the first turn; a canceled context mid-run.
	url, calls := fakeTurns(t, good...)
	plan := fakePlan(t, url, fileFlow, 0)
	stop := make(chan struct{})
	close(stop)
	plan.Interrupt = stop
	_, rep = runConversations(t, plan, 1)
	if rep.Interrupted != 1 || rep.Started != 1 || calls.Load() != 0 || rep.Turns.Count != 0 {
		t.Fatalf("interrupt %+v", rep)
	}
	plan.Interrupt = make(chan struct{})
	unit, convs, _ := NewConversationUnit(NewClient(Options{}), plan)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	mustRun(t, bg, RunConfig{Mode: ModeConcurrency, Concurrency: 1, Requests: 1}, func(_ context.Context, i int64, rec *Recorder) {
		unit(ctx, i, rec)
	})
	if r := convs.Report(); r.Interrupted != 1 || r.Started != 1 {
		t.Fatalf("canceled %+v", r)
	}
}

// anResp is a non-stream Anthropic message with tool_use blocks.
func anResp(stop string, calls ...[2]string) string {
	blocks := []string{`{"type":"text","text":"x"}`}
	for _, c := range calls {
		blocks = append(blocks, `{"type":"tool_use","id":"`+c[0]+`","name":"`+c[1]+`","input":{}}`)
	}
	return `{"id":"msg_1","type":"message","role":"assistant","content":[` + strings.Join(blocks, ",") +
		`],"stop_reason":"` + stop + `","usage":{"input_tokens":1,"output_tokens":1}}`
}

// TestConversationAnthropicToolUseIDs: on Messages routes the wwz encodes
// the step in toolu_ ids; the client checks against the decoded step, not
// the prediction (which would be "read" on the first turn and flag missing
// reasoning and a wrong tool order).
func TestConversationAnthropicToolUseIDs(t *testing.T) {
	url, _ := fakeTurns(t,
		anResp("tool_use", [2]string{"toolu_flow_write_0_a", "write_file"}, [2]string{"toolu_flow_write_1_b", "read_file"}),
		anResp("end_turn"),
	)
	plan := fakePlan(t, url, fileFlow, 0)
	plan.Template.Protocol, plan.Template.URL = Anthropic, url+Anthropic.DefaultPath()
	_, rep := runConversations(t, plan, 1)
	if rep.Completed != 1 || rep.ToolCalls != 2 || rep.OrderViolations+rep.IDMismatches+rep.ReasoningMissing != 0 {
		t.Fatalf("anthropic %+v", rep)
	}
}

// TestPredict: the client-side step choice.
func TestPredict(t *testing.T) {
	yml := `
name: p
step_limit: 2
on_exhausted: error
steps:
  - id: first
    expect: {turn: 1, turn_gte: 1, tool_results: 0}
  - id: weighted-a
    expect: {tool_result_for: read_file, last_tool_result_matches: "(?i)^ok$"}
    weight: 2
  - id: weighted-b
    expect: {tool_result_for: read_file}
  - id: always
    expect: {always: true}
`
	s, _, err := LoadScenario([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	d := &dialog{p: ConversationPlan{Scenario: s}}
	st := &convState{resultsFor: map[string]int{}}
	if got := d.predict(st); got == nil || got.ID != "first" {
		t.Fatalf("first: %v", got)
	}
	st.toolResult("read_file", "ok", false, OpenAI)
	if got := d.predict(st); got != nil {
		t.Fatalf("weighted tie: %v", got.ID)
	}
	st.toolResult("read_file", "ok", true, OpenAI) // server sees "error: ok"
	if got := d.predict(st); got == nil || got.ID != "weighted-b" {
		t.Fatalf("error prefix: %v", got)
	}
	st.toolResult("read_file", "ok", true, Anthropic)
	if st.lastResult != "ok" {
		t.Fatal(st.lastResult)
	}
	st.steps = 2
	if got := d.predict(st); got != nil {
		t.Fatalf("exhausted: %v", got.ID)
	}
	// No match at all.
	s2, _, _ := LoadScenario([]byte("name: q\nsteps:\n  - {id: a, expect: {tool_results: 5}}\n"))
	if got := (&dialog{p: ConversationPlan{Scenario: s2}}).predict(&convState{}); got != nil {
		t.Fatal(got.ID)
	}
	// Conditions the client cannot see make the prediction unknown.
	for _, e := range []scenario.Expect{
		{Model: "m"},
		{Header: map[string]string{"a": "b"}},
		{LastStep: "x"},
		{UserMessageMatches: "x"},
		{LatestMessageRole: "user"},
		{LastToolResultMatches: "("},
	} {
		if _, known := stepMatches(&e, &convState{}); known {
			t.Fatalf("known %+v", e)
		}
	}
	if m, k := stepMatches(nil, &convState{}); !m || !k {
		t.Fatal("nil expect")
	}
	s3, _, _ := LoadScenario([]byte("name: r\nsteps:\n  - {id: a, expect: {model: x}}\n"))
	if got := (&dialog{p: ConversationPlan{Scenario: s3}}).predict(&convState{}); got != nil {
		t.Fatal(got.ID)
	}
	if got := sanitizeID("a_b.c-D9"); got != "a-b-c-D9" {
		t.Fatal(got)
	}
}

// TestCheckSkips: misbehaving or timeline steps and parallel: false.
func TestCheckSkips(t *testing.T) {
	yml := `
name: c
steps:
  - id: mis
    expect: {tool_results: 0}
    tool_misbehavior: truncated
    tool_calls: [{name: a}]
  - id: seq
    expect: {tool_results: 1}
    parallel: false
    reasoning: "pensei"
    tool_calls: [{name: a}, {name: b}]
`
	s, _, err := LoadScenario([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	d := &dialog{p: ConversationPlan{Scenario: s}, steps: map[string]*scenario.Step{}, scn: "c"}
	st := &convState{resultsFor: map[string]int{}}
	var o outcome
	res := &Result{ToolCalls: []ToolCall{{ID: "1", Name: "zz"}, {ID: "2", Name: "zz"}}, Message: &Message{}}
	d.check(st, res, &o)
	if o.order != 0 || o.reasoningMissing != 0 {
		t.Fatalf("misbehaving step checked: %+v", o)
	}
	st.toolResults = 1
	res = &Result{ToolCalls: []ToolCall{{ID: "3", Name: "a"}}, Message: &Message{Reasoning: "r"}}
	d.check(st, res, &o)
	if o.order != 0 || o.reasoningMissing != 0 || o.ids != 0 {
		t.Fatalf("parallel false: %+v", o)
	}
}

func TestMessageMatching(t *testing.T) {
	for _, pat := range []string{
		`(?i)refactor\s+\w+`, `^deploy (prod|staging)$`, `a{2,3}b?c*`, `[^a-z]x`, `.+`, `\bword\b`,
		`[\x00-\x05]`, `[!-#]`, `(?s).y`, `x+`,
	} {
		got, err := MessageMatching(pat)
		if err != nil {
			t.Fatalf("%s: %v", pat, err)
		}
		if got == "" && pat != "" {
			t.Logf("%s → empty", pat)
		}
	}
	if _, err := MessageMatching(`(`); err == nil {
		t.Fatal("invalid pattern")
	}
	if _, err := MessageMatching(`a\bb`); err == nil {
		t.Fatal("unsatisfiable pattern")
	}
	if got, _ := MessageMatching(`^deploy (prod|staging)$`); got != "deploy prod" {
		t.Fatal(got)
	}
}

func TestNewConversationUnitErrors(t *testing.T) {
	if _, _, err := NewConversationUnit(nil, ConversationPlan{}); err == nil {
		t.Fatal("no scenario")
	}
	s, _, err := LoadScenario([]byte("name: u\nmatch: {user_message_matches: 'a\\bb'}\nsteps: [{id: a}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewConversationUnit(nil, ConversationPlan{Scenario: s}); err == nil || !strings.Contains(err.Error(), "user_message_matches") {
		t.Fatal(err)
	}
	if _, _, err := LoadScenario([]byte("name: [")); err == nil {
		t.Fatal("bad yaml")
	}
	// Defaults: max turns, user message from the pattern, tools.
	s, turns, _ := LoadScenario([]byte("name: v\nmatch: {user_message_matches: '^go$'}\nsteps: [{id: a, tool_calls: [{name: any}, {name: '{{x}}'}]}]\n"))
	if turns != DefaultMaxTurns || s.StepLimit != 50 {
		t.Fatal(turns, s.StepLimit)
	}
	var got Request
	url := fake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		got.Tools = nil
		for _, tl := range body["tools"].([]any) {
			got.Tools = append(got.Tools, Tool{Name: tl.(map[string]any)["function"].(map[string]any)["name"].(string)})
		}
		got.Messages = []Message{{Content: body["messages"].([]any)[1].(map[string]any)["content"].(string)}}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaResp("stop", "")))
	})
	plan := fakePlan(t, url, "name: v\nmatch: {user_message_matches: '^go$'}\nsteps: [{id: a, tool_calls: [{name: any}, {name: '{{x}}'}]}]\n", 0)
	plan.MaxTurns = 0
	// The only step declares tool calls: a final answer on it is
	// incomplete (ADR 0041).
	if _, rep := runConversations(t, plan, 1); rep.Failed != 1 || rep.FailedByReason[ReasonIncomplete] != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "read_file" || got.Messages[0].Content != "go" {
		t.Fatalf("%+v", got)
	}
	tools := scenarioTools(&scenario.Scenario{Match: &scenario.Match{ToolsInclude: []string{"a", "b"}}, Steps: []scenario.Step{{ToolCalls: []scenario.ToolCallSpec{{Name: "a"}, {Name: "c"}}}}})
	if len(tools) != 3 || tools[2].Name != "c" {
		t.Fatal(tools)
	}
}

func TestConvStatsReport(t *testing.T) {
	a, b := NewConvStats(), NewConvStats()
	(&outcome{status: statusCompleted, turns: 5, toolCalls: 4, e2e: time.Second}).addTo(a)
	(&outcome{status: statusFailed, reason: ReasonMaxTurns, turns: 12, order: 1, ids: 1, reasoningMissing: 1}).addTo(b)
	(&outcome{status: statusFailed, reason: "timeout", turns: 2}).addTo(b)
	(&outcome{status: statusInterrupted}).addTo(a)
	a.Merge(b)
	r := a.Report()
	if r.Started != 4 || r.Completed != 1 || r.Failed != 2 || r.MaxTurns != 1 || r.Interrupted != 1 ||
		r.Turns.Count != 3 || r.Turns.P50 != 5 || r.Turns.Max != 12 || r.Turns.Mean != 6.333 || r.E2E.Count != 1 ||
		r.FailedByReason["timeout"] != 1 || r.OrderViolations != 1 || r.IDMismatches != 1 || r.ReasoningMissing != 1 {
		t.Fatalf("%+v", r)
	}
	var convs Conversations
	key := NewStats()
	first := convs.of(key)
	if convs.of(key) != first {
		t.Fatal("of not stable")
	}
	if s := countSummary(nil); s.Count != 0 {
		t.Fatal(s)
	}
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil || !strings.Contains(buf.String(), "conversations: 4 started") || !strings.Contains(buf.String(), "max_turns") {
		t.Fatalf("%v %s", err, buf.String())
	}
	if err := r.WriteText(&failWriter{}); err == nil {
		t.Fatal("write error")
	}
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"id_mismatches":1`) || !strings.Contains(string(b), `"e2e_ms"`) {
		t.Fatal(string(b))
	}
}

// TestConvStatsJSON: the JSON form (worker → controller, task 045) round
// trips with the same report.
func TestConvStatsJSON(t *testing.T) {
	a := NewConvStats()
	(&outcome{status: statusCompleted, turns: 3, toolCalls: 2, e2e: 40 * time.Millisecond}).addTo(a)
	(&outcome{status: statusFailed, reason: "timeout", turns: 1, order: 1}).addTo(a)
	b, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(b), `"failed_by_reason":{"timeout":1}`) || !strings.Contains(string(b), `"e2e":{"version":`) {
		t.Fatalf("%v %s", err, b)
	}
	var back ConvStats
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(a.Report())
	got, _ := json.Marshal(back.Report())
	if string(got) != string(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

// turnResp is a scripted response of fakeTurnsH: an OpenAI or Anthropic
// non-stream body and its response headers.
type turnResp struct {
	body string
	hdr  map[string]string
}

// fakeTurnsH serves the responses by turn (number of assistant messages)
// with their headers, recording the x-wwz-scenario of every request.
func fakeTurnsH(t *testing.T, resps ...turnResp) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	url := fake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, strings.Join(r.Header.Values("x-wwz-scenario"), ","))
		mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		turn := strings.Count(string(b), `"role":"assistant"`)
		tr := resps[min(turn, len(resps)-1)]
		for k, v := range tr.hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tr.body))
	})
	return url, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func hdr(scn, step string) map[string]string {
	h := map[string]string{}
	if scn != "" {
		h["x-wwz-scenario"] = scn
	}
	if step != "" {
		h["x-wwz-step"] = step
	}
	return h
}

// TestConversationTerminalStep (ADR 0041): a conversation completes only on
// a terminal step; a server that does not apply the scenario (no
// x-wwz-scenario, stop on the first turn) gives scenario_not_applied and
// failed_by_reason.incomplete, with the warning line in the text.
func TestConversationTerminalStep(t *testing.T) {
	read := oaResp("tool_calls", "r", [2]string{"call_flow_read_0_a", "read_file"})
	write := oaResp("tool_calls", "", [2]string{"call_flow_write_0_b", "write_file"}, [2]string{"call_flow_write_1_c", "read_file"})
	stop := oaResp("stop", "")
	cases := []struct {
		name               string
		resps              []turnResp
		completed, notAppl int64
		order              int64
	}{
		// Not applied: stop on turn 1; the prediction (read) asks tools.
		{"not-applied", []turnResp{{stop, nil}}, 0, 1, 1},
		// Headers say done: completed even where the prediction is unknown.
		{"header-done", []turnResp{{read, hdr("flow", "read")}, {write, hdr("flow", "write")}, {stop, hdr("flow", "done")}}, 1, 0, 0},
		// The header step overrides the prediction (read) for the checks.
		{"header-overrides", []turnResp{{stop, hdr("flow", "done")}}, 1, 0, 0},
		// Header step with tool calls, unknown step, another scenario.
		{"header-nonterminal", []turnResp{{stop, hdr("flow", "write")}}, 0, 0, 1},
		{"header-unknown", []turnResp{{stop, hdr("flow", "nope")}}, 0, 0, 0},
		{"other-scenario", []turnResp{{stop, hdr("other", "done")}}, 0, 1, 0},
		// Wrong scenario name without step: prediction (read) → incomplete.
		{"wrong-name", []turnResp{{stop, hdr("flowx", "")}}, 0, 1, 1},
	}
	for _, c := range cases {
		url, _ := fakeTurnsH(t, c.resps...)
		_, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 1)
		if rep.Completed != c.completed || rep.ScenarioNotApplied != c.notAppl*int64(len(c.resps)) || rep.OrderViolations != c.order {
			t.Fatalf("%s: %+v", c.name, rep)
		}
		if c.completed == 0 && (rep.Failed != 1 || rep.FailedByReason[ReasonIncomplete] != 1) {
			t.Fatalf("%s: %+v", c.name, rep)
		}
		if c.name == "not-applied" {
			var buf bytes.Buffer
			_ = rep.WriteText(&buf)
			if !strings.Contains(buf.String(), "aviso: 1 respostas sem x-wwz-scenario: flow (cenário não aplicado ou header removido pelo roteador)") ||
				!strings.Contains(buf.String(), "scenario not applied 1") {
				t.Fatal(buf.String())
			}
			if b, _ := json.Marshal(rep); !strings.Contains(string(b), `"scenario_not_applied":1`) || strings.Contains(string(b), `"scenario":`) {
				t.Fatal(string(b))
			}
		}
	}
	// Without the warning when every response carries the scenario.
	url, _ := fakeTurnsH(t, turnResp{stop, hdr("flow", "done")})
	_, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 1)
	var buf bytes.Buffer
	if _ = rep.WriteText(&buf); strings.Contains(buf.String(), "aviso") {
		t.Fatal(buf.String())
	}
	// A non-2xx response is not counted as not applied.
	url, _ = fakeTurns(t, "500")
	if _, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 1); rep.ScenarioNotApplied != 0 {
		t.Fatalf("500: %+v", rep)
	}
}

// TestConversationHiddenReasoning (ADR 0041): reasoning_tokens > 0 in the
// usage or a redacted_thinking block count as reasoning present.
func TestConversationHiddenReasoning(t *testing.T) {
	withTokens := func(n string) string {
		b := oaResp("tool_calls", "", [2]string{"call_flow_read_0_a", "read_file"})
		return strings.Replace(b, `"completion_tokens":1}`, `"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":`+n+`}}`, 1)
	}
	for _, c := range []struct {
		tokens  string
		missing int64
	}{{"7", 0}, {"0", 1}} {
		url, _ := fakeTurnsH(t, turnResp{withTokens(c.tokens), hdr("flow", "read")}, turnResp{oaResp("stop", ""), hdr("flow", "done")})
		_, rep := runConversations(t, fakePlan(t, url, fileFlow, 0), 1)
		if rep.ReasoningMissing != c.missing || rep.Completed != 1 {
			t.Fatalf("tokens %s: %+v", c.tokens, rep)
		}
	}
	redacted := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"redacted_thinking","data":"xx"},` +
		`{"type":"tool_use","id":"toolu_flow_read_0_a","name":"read_file","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	url, _ := fakeTurnsH(t, turnResp{redacted, hdr("flow", "read")}, turnResp{anResp("end_turn"), hdr("flow", "done")})
	plan := fakePlan(t, url, fileFlow, 0)
	plan.Template.Protocol, plan.Template.URL = Anthropic, url+Anthropic.DefaultPath()
	if _, rep := runConversations(t, plan, 1); rep.ReasoningMissing != 0 || rep.Completed != 1 || rep.OrderViolations != 0 {
		t.Fatalf("redacted: %+v", rep)
	}
}

// TestConvStatsScenarioMerge: scenario_not_applied and the scenario name
// survive the merge and the worker → controller JSON.
func TestConvStatsScenarioMerge(t *testing.T) {
	a, b := NewConvStats(), &ConvStats{FailedByReason: map[string]int64{}, Scenario: "flow", E2E: NewConvStats().E2E}
	(&outcome{status: statusFailed, reason: ReasonIncomplete, turns: 1, notApplied: 1}).addTo(b)
	(&outcome{status: statusFailed, reason: ReasonIncomplete, turns: 1, notApplied: 2}).addTo(a)
	raw, _ := json.Marshal(b)
	var back ConvStats
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	a.Merge(&back)
	if r := a.Report(); r.ScenarioNotApplied != 3 || r.Scenario != "flow" || r.FailedByReason[ReasonIncomplete] != 2 {
		t.Fatalf("%+v", r)
	}
	a.Merge(&ConvStats{FailedByReason: map[string]int64{}, Scenario: "other", E2E: NewConvStats().E2E})
	if a.Scenario != "flow" {
		t.Fatal(a.Scenario)
	}
}
