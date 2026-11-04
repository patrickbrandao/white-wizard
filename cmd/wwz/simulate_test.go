package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/")

// Wall-clock parts of a response body: the created fields and the Unix time
// inside OpenRouter generation ids (gen-<unix>-<random>).
var (
	clockFieldRe = regexp.MustCompile(`"(created|created_at)": \d+`)
	genIDRe      = regexp.MustCompile(`"gen-\d+-`)
)

func normalizeClock(s string) string {
	return genIDRe.ReplaceAllString(clockFieldRe.ReplaceAllString(s, `"$1": 0`), `"gen-0-`)
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: test fixture path
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if got != string(want) {
		t.Fatalf("output differs from %s (go test -update to accept):\n%s", path, got)
	}
}

func TestSimulateGolden(t *testing.T) {
	cleanEnv(t)
	for _, tc := range []struct {
		file string
		args []string
	}{
		{"simulate-file-agent-flow-openai.golden", []string{"-scenario", "file-agent-flow", "-turns", "6", "-seed", "42"}},
		{"simulate-file-agent-flow-anthropic.golden", []string{"-scenario", "file-agent-flow", "-turns", "6", "-seed", "42", "-driver", "anthropic"}},
		{"simulate-retry-on-error-openrouter.golden", []string{"-scenario", "../../scenarios/retry-on-error.yaml", "-turns", "3", "-seed", "42", "-driver", "openrouter"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			args := append([]string{"simulate", "-scenarios-dir", "../../scenarios"}, tc.args...)
			code, out, errs := runWWZ(t, args...)
			if code != exitOK || errs != "" {
				t.Fatalf("code %d stderr %s", code, errs)
			}
			// Same seed, same output (apart from the wall clock).
			_, again, _ := runWWZ(t, args...)
			if normalizeClock(out) != normalizeClock(again) {
				t.Fatal("two runs with the same seed differ")
			}
			checkGolden(t, tc.file, normalizeClock(out))
		})
	}
}

func TestSimulateFileAgentFlowSteps(t *testing.T) {
	cleanEnv(t)
	for _, drv := range []string{"openai", "anthropic", "opencode", "openrouter"} {
		t.Run(drv, func(t *testing.T) {
			code, out, errs := runWWZ(t, "simulate", "-scenarios-dir", "../../scenarios", "-scenario", "file-agent-flow", "-turns", "6", "-driver", drv, "-json")
			if code != exitOK {
				t.Fatalf("code %d stderr %s", code, errs)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) != 6 {
				t.Fatalf("want 6 turns, got %d:\n%s", len(lines), out)
			}
			var steps []string
			for i, l := range lines {
				var turn simTurn
				if err := json.Unmarshal([]byte(l), &turn); err != nil || turn.Turn != i+1 || turn.Status != 200 || len(turn.Body) == 0 {
					t.Fatalf("turn %d: %v %s", i+1, err, l)
				}
				steps = append(steps, turn.Step)
			}
			if got := strings.Join(steps, ","); got != "read,write,edit,delete,done,done" {
				t.Fatalf("steps %s", got)
			}
		})
	}
	// Another seed changes the content.
	_, a, _ := runWWZ(t, "simulate", "-scenarios-dir", "../../scenarios", "-scenario", "file-agent-flow", "-seed", "1")
	_, b, _ := runWWZ(t, "simulate", "-scenarios-dir", "../../scenarios", "-scenario", "file-agent-flow", "-seed", "2")
	if normalizeClock(a) == normalizeClock(b) {
		t.Fatal("different seeds gave the same output")
	}
}

func TestSimulateRetryAndDefaults(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	// scenarios.files in the configuration, server.seed as the default seed.
	cfg := tmpYAML(t, dir, "wwz.yaml", "server: {seed: 9}\nscenarios: {files: [../../scenarios/retry-on-error.yaml]}\n")
	code, out, errs := runWWZ(t, "simulate", "-config", cfg, "-scenario", "retry-on-error", "-turns", "3", "-tools", "lookup")
	if code != exitOK {
		t.Fatalf("code %d stderr %s", code, errs)
	}
	for _, want := range []string{"--- turn 1/3 status=429 retry-after=1 attempt=1 scenario=retry-on-error step=call", "--- turn 2/3 status=200 attempt=2 scenario=retry-on-error seed=9 step=call", `"name": "lookup"`, "step=answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSimulateErrors(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	bad := tmpYAML(t, dir, "bad.yaml", "name: x\n")
	badCfg := tmpYAML(t, dir, "cfg.yaml", "store: {kind: redis}\n")
	onlyOpenAI := tmpYAML(t, dir, "models.yaml", "models: [{id: m1, protocols: [openai]}]\n")
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-scenario", "x", "extra"}, exitUsage, `unexpected argument "extra"`},
		{[]string{"-scenario", "x", "-turns", "0"}, exitUsage, "-turns must be at least 1"},
		{[]string{"-nope"}, exitUsage, "flag provided but not defined"},
		{[]string{"-scenario", "nope", "-scenarios-dir", "../../scenarios"}, exitError, `scenario "nope" not found in ../../scenarios`},
		{[]string{"-scenario", "nope", "-scenarios-dir", filepath.Join(dir, "missing")}, exitError, "no such file"},
		{[]string{"-scenario", bad}, exitError, "at least one step"},
		{[]string{"-scenario", filepath.Join(dir, "missing.yaml")}, exitError, "no such file"},
		{[]string{"-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios", "-config", badCfg}, exitError, "store.kind"},
		{[]string{"-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios", "-driver", "nope"}, exitError, "nope"},
		{[]string{"-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios", "-model", "gpt-9"}, exitError, `model "gpt-9" is not configured for driver "openai"`},
		{[]string{"-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios", "-config", onlyOpenAI, "-driver", "anthropic"}, exitError, `model "" is not configured for driver "anthropic"`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, _, errs := runWWZ(t, append([]string{"simulate"}, tc.args...)...)
			if code != tc.code || !strings.Contains(errs, tc.want) {
				t.Fatalf("code %d (want %d) stderr %s", code, tc.code, errs)
			}
		})
	}
	t.Run("default model and directory", func(t *testing.T) {
		code, out, errs := runWWZ(t, "simulate", "-config", onlyOpenAI, "-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios", "-turns", "1")
		if code != exitOK || !strings.Contains(out, `"model": "m1"`) {
			t.Fatalf("code %d stderr %s\n%s", code, errs, out)
		}
		// Without -scenarios-dir nor scenarios.dir the lookup is ./scenarios, then the built-ins.
		code, _, errs = runWWZ(t, "simulate", "-scenario", "nope")
		if code != exitError || !strings.Contains(errs, "not found in scenarios") && !strings.Contains(errs, "no such file") {
			t.Fatalf("code %d stderr %s", code, errs)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out, errb strings.Builder
		code := run(ctx, []string{"simulate", "-scenario", "file-agent-flow", "-scenarios-dir", "../../scenarios"}, &out, &errb)
		if code != exitError || !strings.Contains(errb.String(), "context canceled") {
			t.Fatalf("code %d stderr %s", code, errb.String())
		}
	})
}

func TestScenarioTools(t *testing.T) {
	s := &scenario.Scenario{Name: "t", Steps: []scenario.Step{
		{ToolCalls: []scenario.ToolCallSpec{{Name: "any_read"}, {Name: "any_write"}, {Name: "{{pick \"a\" \"b\"}}"}, {Name: "web_search"}}},
		{Timeline: []scenario.TimelineEvent{{Type: "tool_call", Name: "fetch_page"}, {Type: "text", Name: "ignored"}}},
	}}
	var got []string
	for _, td := range scenarioTools(s, []string{"create_doc"}) {
		props, _ := td.Schema["properties"].(map[string]any)
		got = append(got, td.Name+":"+strings.Join(sortedKeys(props), "+"))
	}
	want := "web_search:max_results+query,fetch_page:url,create_doc:content+path,read_file:path"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	if tools := scenarioTools(&scenario.Scenario{Steps: []scenario.Step{{ToolCalls: []scenario.ToolCallSpec{{Name: "any"}}}}}, nil); len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("any: %+v", tools)
	}
	if req, _ := toolSchema("edit_file")["required"].([]any); len(req) != 3 {
		t.Fatalf("edit schema %v", req)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSimulateParseAndPrint(t *testing.T) {
	chat := &simulation{kind: driver.KindChat, f: &simulateFlags{turns: 1}}
	msgs := &simulation{kind: driver.KindMessages, f: &simulateFlags{turns: 1, json: true}}
	for _, s := range []*simulation{chat, msgs} {
		if _, _, err := s.parseResponse([]byte("not json")); err == nil {
			t.Fatalf("%s: want decode error", s.kind)
		}
	}
	if _, _, err := chat.parseResponse([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("want no choices error")
	}
	text, calls, err := msgs.parseResponse([]byte(`{"content":[{"type":"thinking"},{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"x","input":{"k":1}}]}`))
	if err != nil || text != "a" || len(calls) != 1 || calls[0].Args != `{"k":1}` {
		t.Fatalf("%q %+v %v", text, calls, err)
	}
	// An assistant turn without text nor tools still has a content block.
	if m := msgs.assistantMessage("", nil); len(m) != 1 {
		t.Fatalf("%+v", m)
	}
	var b strings.Builder
	if err := msgs.print(&b, 1, 502, map[string]string{"X-WWZ-Step": "s", "Content-Type": "text/plain"}, []byte("upstream down")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != `{"turn":1,"status":502,"step":"s","headers":{"x-wwz-step":"s"},"body":"upstream down"}`+"\n" {
		t.Fatalf("json print %s", got)
	}
	b.Reset()
	if err := chat.print(&b, 1, 502, nil, []byte("upstream down")); err != nil || b.String() != "--- turn 1/1 status=502\nupstream down\n" {
		t.Fatalf("text print %q %v", b.String(), err)
	}
}
