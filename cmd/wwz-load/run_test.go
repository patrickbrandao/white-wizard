package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
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
  - {id: wwz-fast, protocols: [openai, anthropic], latency: zero, generation: short, errors: none,
     scenarios: [file-agent-flow, search-then-answer]}
`

const fileAgentFlow = "../../scenarios/file-agent-flow.yaml"

// startScenarioWWZ serves the wwz in process with the scenarios of
// ./scenarios selected by match.
func startScenarioWWZ(t *testing.T) string {
	t.Helper()
	return startWWZWith(t, scenarioCfg)
}

// startWWZWith serves the wwz in process with cfg and the scenarios of
// ./scenarios.
func startWWZWith(t *testing.T, cfg string) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

// convReport is the report with the conversations object decoded.
type convReport struct {
	loadgen.Report
	Conversations loadgen.ConversationReport `json:"conversations"`
}

// TestScenarioAcceptance: --scenario file-agent-flow --concurrency 50
// --duration 5s completes whole conversations (read → write → edit →
// delete → answer) in both protocols, ids and order preserved.
func TestScenarioAcceptance(t *testing.T) {
	base := startScenarioWWZ(t)
	for _, p := range []string{"openai", "anthropic"} {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			code, out, errOut := runArgs(t, context.Background(), "--target", base, "--protocol", p, "--scenario", fileAgentFlow,
				"--concurrency", "50", "--duration", "5s", "--output", "json", "--progress", "0", "--seed", "9")
			var rep convReport
			if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			c := rep.Conversations
			if c.Completed < 50 || c.Failed != 0 || c.Interrupted != 0 || c.Started != c.Completed {
				t.Fatalf("conversations %+v", c)
			}
			if c.Turns.P50 != 5 || c.Turns.Max != 5 || c.ToolCalls != 4*c.Completed || c.E2E.P50 < 2000 {
				t.Fatalf("turns/tool calls %+v", c)
			}
			if c.OrderViolations != 0 || c.IDMismatches != 0 || c.ReasoningMissing != 0 {
				t.Fatalf("checks %+v", c)
			}
			if rep.Total.Requests != 5*c.Completed || rep.Total.Errors != 0 || rep.Config.Scenario != fileAgentFlow {
				t.Fatalf("total %+v config %+v", rep.Total, rep.Config)
			}
		})
	}
}

// TestScenario1000Conversations (spec 011): 1 000 simultaneous conversations
// of file-agent-flow (the delete step holds 2 s) keep their order and ids.
// Starts are paced at 500/s instead of --concurrency 1000: a single burst of
// 1 000 connects overflows the listen backlog of macOS (somaxconn 128).
func TestScenario1000Conversations(t *testing.T) {
	base := startScenarioWWZ(t)
	code, out, errOut := runArgs(t, context.Background(), "--target", base, "--scenario", fileAgentFlow,
		"--rps", "500", "--requests", "1000", "--output", "json", "--progress", "0", "--seed", "9")
	var rep convReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	c := rep.Conversations
	if c.Completed < 1000 || c.Failed != 0 || c.Interrupted != 0 || c.Started != c.Completed {
		t.Fatalf("conversations %+v", c)
	}
	if c.OrderViolations != 0 || c.IDMismatches != 0 || c.ReasoningMissing != 0 || c.ToolCalls != 4*c.Completed {
		t.Fatalf("checks %+v", c)
	}
	if rep.Total.Requests != 5*c.Completed || rep.Total.Errors != 0 {
		t.Fatalf("total %+v", rep.Total)
	}
}

// writeFailOn fails the write that contains marker.
type writeFailOn struct {
	bytes.Buffer
	marker string
}

func (w *writeFailOn) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		return 0, os.ErrClosed
	}
	return w.Buffer.Write(p)
}

// TestScenarioTextAndFailures: text block, --max-turns and
// --tool-result-error-rate; unreadable or invalid scenarios and unit
// errors exit 1.
func TestScenarioTextAndFailures(t *testing.T) {
	base := startScenarioWWZ(t)
	ctx := context.Background()
	args := []string{
		"--target", base, "--scenario", fileAgentFlow, "--requests", "2", "--concurrency", "2", "--progress", "0",
		"--max-turns", "2", "--tool-result-error-rate", "100",
	}
	code, out, errOut := runArgs(t, ctx, args...)
	if code != 0 || !strings.Contains(out, "conversations: 2 started  0 completed  2 failed (2 max_turns)") {
		t.Fatalf("%d %s\n%s", code, errOut, out)
	}
	var w writeFailOn
	w.marker = "conversations:"
	if code := run(ctx, args, &w, io.Discard); code != exitError {
		t.Fatalf("conversation text write: %d", code)
	}
	// CSV keeps its fixed format (no conversation block).
	if code, out, _ := runArgs(t, ctx, append(args, "--output", "csv")...); code != 0 || strings.Contains(out, "conversations") {
		t.Fatalf("csv: %d %s", code, out)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("name: x\n"), 0o600)
	for _, f := range []string{bad, filepath.Join(t.TempDir(), "missing.yaml")} {
		if code, _, errOut := runArgs(t, ctx, "--target", base, "--scenario", f, "--requests", "1"); code != exitError || !strings.Contains(errOut, f) {
			t.Fatalf("%s: %d %s", f, code, errOut)
		}
	}
	defer func() { newConversationUnit = loadgen.NewConversationUnit }()
	newConversationUnit = func(*loadgen.Client, loadgen.ConversationPlan) (loadgen.Unit, *loadgen.Conversations, error) {
		return nil, nil, errors.New("conv boom")
	}
	if code, _, errOut := runArgs(t, ctx, "--target", base, "--scenario", fileAgentFlow); code != exitError || !strings.Contains(errOut, "conv boom") {
		t.Fatalf("seam: %d %s", code, errOut)
	}
	// The plan carries the flags: default max turns from step_limit (12)
	// and the interrupt channel.
	var got loadgen.ConversationPlan
	newConversationUnit = func(_ *loadgen.Client, p loadgen.ConversationPlan) (loadgen.Unit, *loadgen.Conversations, error) {
		got = p
		return nil, nil, errors.New("stop")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, _, _ = runArgs(t, cctx, "--target", base, "--scenario", fileAgentFlow, "--seed", "5", "--protocol", "anthropic")
	if got.MaxTurns != 12 || got.Seed != 5 || got.DrawSeed != 5 || got.Interrupt == nil || got.Template.Protocol != loadgen.Anthropic ||
		got.Scenario.Name != "file-agent-flow" || got.Prompt != defaultPrompt {
		t.Fatalf("plan %+v", got)
	}
}

// headerCfg: the model does not list the scenarios and the openai driver
// keeps reasoning_field: hidden (ADR 0022), so only x-wwz-scenario selects
// file-agent-flow and only reasoning_tokens shows its reasoning.
const headerCfg = `
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
  - {id: wwz-fast, protocols: [openai, anthropic], latency: zero, generation: short, errors: none}
`

// TestScenarioHeaderDefault (ADR 0041): without --header the conversation
// sends x-wwz-scenario: file-agent-flow and completes against a model that
// does not list the scenario, with hidden reasoning not counted as missing;
// with an empty --header the scenario is not applied: warning,
// scenario_not_applied and failed_by_reason.incomplete, exit 0.
func TestScenarioHeaderDefault(t *testing.T) {
	base := startWWZWith(t, headerCfg)
	ctx := context.Background()
	for _, p := range []string{"openai", "anthropic"} {
		code, out, errOut := runArgs(t, ctx, "--target", base, "--protocol", p, "--scenario", fileAgentFlow,
			"--concurrency", "4", "--requests", "8", "--output", "json", "--progress", "0", "--seed", "3")
		var rep convReport
		if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
			t.Fatalf("%s: %d %s %s", p, code, errOut, out)
		}
		c := rep.Conversations
		if c.Completed < 1 || c.Failed != 0 || c.Turns.P50 != 5 || c.OrderViolations != 0 || c.ScenarioNotApplied != 0 ||
			c.ReasoningMissing != 0 || c.IDMismatches != 0 {
			t.Fatalf("%s: %+v", p, c)
		}
		if !slices.Equal(rep.Config.Headers, []string{"x-wwz-scenario"}) {
			t.Fatalf("%s: headers %v", p, rep.Config.Headers)
		}
	}
	code, out, errOut := runArgs(t, ctx, "--target", base, "--scenario", fileAgentFlow, "--header", "X-WWZ-Scenario:",
		"--concurrency", "2", "--requests", "2", "--progress", "0")
	if code != 0 || !strings.Contains(out, "aviso: 2 respostas sem x-wwz-scenario: file-agent-flow") ||
		!strings.Contains(out, "0 completed  2 failed") || !strings.Contains(out, "failed        2  incomplete") {
		t.Fatalf("suppressed: %d %s\n%s", code, errOut, out)
	}
}

// TestScenarioHeaderPlan (ADR 0041): the header goes into the template of
// the conversation and into the plan of the workers; an explicit value
// wins regardless of case; an empty value suppresses it; an unreadable
// scenario adds nothing (the run fails on it later).
func TestScenarioHeaderPlan(t *testing.T) {
	scnHeaders := func(hs []loadgen.Header) []loadgen.Header {
		var out []loadgen.Header
		for _, h := range hs {
			if strings.EqualFold(h.Key, "x-wwz-scenario") {
				out = append(out, h)
			}
		}
		return out
	}
	cases := []struct {
		args []string
		want []loadgen.Header
	}{
		{nil, []loadgen.Header{{Key: "x-wwz-scenario", Value: "file-agent-flow"}}},
		{[]string{"--header", "X-Wwz-Scenario: outro"}, []loadgen.Header{{Key: "X-Wwz-Scenario", Value: "outro"}}},
		{[]string{"--header", "x-wwz-scenario: outro", "--header", "x-wwz-scenario:"}, nil},
		{[]string{"--header", "x-wwz-scenario:", "--header", "x-wwz-scenario: b"}, []loadgen.Header{{Key: "x-wwz-scenario"}, {Key: "x-wwz-scenario", Value: "b"}}},
	}
	for _, c := range cases {
		args := append([]string{"--scenario", fileAgentFlow, "--header", "x-a: 1"}, c.args...)
		o, err := parseRun(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if got := scnHeaders(o.headers); !slices.Equal(got, c.want) {
			t.Fatalf("%v: %v", c.args, got)
		}
		wantKeys := []string{"x-a"}
		for _, h := range c.want {
			wantKeys = append(wantKeys, h.Key)
		}
		if rc := o.reportConfig(1); !slices.Equal(rc.Headers, wantKeys) {
			t.Fatalf("%v: config.headers %v", c.args, rc.Headers)
		}
		// Distributed: the plan of the workers carries the same headers.
		co, err := parseController(append([]string{"--instances", "1"}, args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := co.plan(1)
		if err != nil {
			t.Fatal(err)
		}
		var spec requestSpec
		if err := json.Unmarshal(plan.Request, &spec); err != nil || !slices.Equal(scnHeaders(spec.Headers), c.want) {
			t.Fatalf("%v: plan %v %v", c.args, err, spec.Headers)
		}
	}
	// The conversation template carries the default header.
	defer func() { newConversationUnit = loadgen.NewConversationUnit }()
	var got loadgen.ConversationPlan
	newConversationUnit = func(_ *loadgen.Client, p loadgen.ConversationPlan) (loadgen.Unit, *loadgen.Conversations, error) {
		got = p
		return nil, nil, errors.New("stop")
	}
	_, _, _ = runArgs(t, context.Background(), "--scenario", fileAgentFlow)
	if !slices.Equal(got.Template.Headers, []loadgen.Header{{Key: "x-wwz-scenario", Value: "file-agent-flow"}}) {
		t.Fatalf("template %v", got.Template.Headers)
	}
	// Unreadable or invalid scenario: no header added.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("name: x\n"), 0o600)
	for _, f := range []string{bad, filepath.Join(t.TempDir(), "missing.yaml")} {
		o, err := parseRun([]string{"--scenario", f}, io.Discard)
		if err != nil || len(o.headers) != 0 {
			t.Fatalf("%s: %v %v", f, err, o.headers)
		}
	}
	// Without --scenario nothing is added.
	if o, _ := parseRun(nil, io.Discard); len(o.headers) != 0 {
		t.Fatal(o.headers)
	}
}
