package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func checkYAML(t *testing.T, doc string, opts CheckOptions) Report {
	t.Helper()
	var s Scenario
	if err := yaml.UnmarshalWithOptions([]byte(doc), &s, yaml.Strict()); err != nil {
		t.Fatal(err)
	}
	return s.Check(opts)
}

func hasMsg(list []string, sub string) bool {
	for _, m := range list {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func TestCheckRepositoryScenarios(t *testing.T) {
	files, err := filepath.Glob("../../scenarios/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G304: test fixture path
		if err != nil {
			t.Fatal(err)
		}
		s, err := ParseYAML(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if rep := s.Check(CheckOptions{}); !rep.OK() {
			t.Errorf("%s: %v", f, rep.Errors)
		}
	}
}

func TestCheckFindings(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		opts     CheckOptions
		errSub   string
		warnSub  string
		wantOKAs bool
	}{
		{
			name:   "unreachable after catch-all",
			doc:    "name: x\nsteps:\n  - id: a\n    content: hi\n  - id: b\n    expect: {tool_results: 1}\n    content: bye\n",
			errSub: `step "b" is unreachable: the earlier step "a"`,
		},
		{
			name:   "unreachable same condition",
			doc:    "name: x\nsteps:\n  - id: a\n    expect: {last_tool: read}\n  - id: b\n    expect: {last_tool: read, turn: 2}\n",
			errSub: `step "b" is unreachable`,
		},
		{
			name:     "weights disable the reachability check",
			doc:      "name: x\nsteps:\n  - id: a\n    weight: 1\n  - id: b\n    weight: 2\n",
			wantOKAs: true,
		},
		{
			name:     "distinct conditions are reachable",
			doc:      "name: x\nsteps:\n  - id: a\n    expect: {last_tool: read, header: {X-A: \"1\"}}\n  - id: b\n    expect: {last_tool: read}\n",
			wantOKAs: true,
		},
		{
			name:   "bad content template",
			doc:    "name: x\nsteps:\n  - id: a\n    content: \"{{ .vars.p \"\n",
			errSub: `step "a" content: invalid template`,
		},
		{
			name:   "unknown template function",
			doc:    "name: x\nvars: {p: \"{{nope}}\"}\nsteps:\n  - id: a\n",
			errSub: `vars.p: invalid template`,
		},
		{
			name:   "bad args template",
			doc:    "name: x\nsteps:\n  - id: a\n    tool_calls:\n      - {name: t, args: {path: [\"{{lorem\"]}}\n",
			errSub: `tool_calls[0].args.path[0]: invalid template`,
		},
		{
			name:   "bad overlay and timeline templates",
			doc:    "name: x\nsteps:\n  - id: a\n    tool_calls:\n      - {name: t, args: schema+, overlay: {q: \"{{\"}}\n    timeline:\n      - {type: text, text: \"{{end}}\"}\n",
			errSub: `overlay.q: invalid template`,
		},
		{
			name:   "tool not offered",
			doc:    "name: x\nsteps:\n  - id: a\n    tool_calls:\n      - {name: delete_file}\n      - {name: any}\n    timeline:\n      - {type: tool_call, name: rm}\n",
			opts:   CheckOptions{Tools: []string{"read_file"}},
			errSub: `tool "delete_file" is not offered`,
		},
		{
			name:    "tool outside tools_include is a warning",
			doc:     "name: x\nmatch: {tools_include: [read_file]}\nsteps:\n  - id: a\n    tool_calls:\n      - {name: edit_file}\n",
			warnSub: `tool "edit_file" is not in match.tools_include`,
		},
		{
			name:   "enums",
			doc:    "name: x\non_no_match: explode\nsteps:\n  - id: a\n    moderation: nope\n    timeline:\n      - {type: bogus}\n",
			errSub: `on_no_match must be one of fallback | error | repeat_last, got "explode"`,
		},
		{
			name:   "timeline type",
			doc:    "name: x\nsteps:\n  - id: a\n    timeline:\n      - {type: bogus}\n",
			errSub: `timeline[0].type must be one of`,
		},
		{
			name:   "negative step limit",
			doc:    "name: x\nstep_limit: -1\nsteps:\n  - id: a\n",
			errSub: "step_limit must not be negative",
		},
		{
			name:   "validate error",
			doc:    "name: x\n",
			errSub: "at least one step is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := checkYAML(t, tc.doc, tc.opts)
			if tc.wantOKAs && (!rep.OK() || len(rep.Warnings) > 0) {
				t.Fatalf("want clean, got %+v", rep)
			}
			if tc.errSub != "" && !hasMsg(rep.Errors, tc.errSub) {
				t.Fatalf("want error %q, got %+v", tc.errSub, rep)
			}
			if tc.warnSub != "" && (!rep.OK() || !hasMsg(rep.Warnings, tc.warnSub)) {
				t.Fatalf("want only warning %q, got %+v", tc.warnSub, rep)
			}
		})
	}
}
