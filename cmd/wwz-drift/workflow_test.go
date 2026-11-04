package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// realKeys are the provider keys wwz-drift check --real reads (spec 017).
var realKeys = []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "OPENCODE_API_KEY", "GEMINI_API_KEY"}

// TestDriftWorkflow (I24, ADR 0035): .github/workflows/drift.yml runs on a
// schedule and by hand, calls wwz-drift check --real with the keys taken from
// secrets.* and uploads the report; no key is written in the file.
func TestDriftWorkflow(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "drift.yml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
				With map[string]any    `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	cron := regexp.MustCompile(`^\S+ \S+ \S+ \S+ \S+$`)
	sched, _ := wf.On["schedule"].([]any)
	if len(sched) == 0 {
		t.Fatalf("on.schedule missing: %v", wf.On)
	}
	for _, s := range sched {
		m, _ := s.(map[string]any)
		if c, _ := m["cron"].(string); !cron.MatchString(c) {
			t.Errorf("cron %q", c)
		}
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Errorf("on.workflow_dispatch missing: %v", wf.On)
	}
	var checked, uploaded bool
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "wwz-drift check --real") {
				checked = true
				for _, k := range realKeys {
					if want := fmt.Sprintf("${{ secrets.%s }}", k); s.Env[k] != want {
						t.Errorf("%s = %q, want %q", k, s.Env[k], want)
					}
				}
			}
			if strings.HasPrefix(s.Uses, "actions/upload-artifact") && strings.Contains(fmt.Sprint(s.With["path"]), "drift.json") {
				uploaded = true
			}
		}
	}
	if !checked || !uploaded {
		t.Errorf("check step %v, report upload %v", checked, uploaded)
	}
	// No literal key anywhere: every *_API_KEY assignment is a secrets reference.
	assign := regexp.MustCompile(`(?m)_API_KEY\s*[:=]\s*(\S+)`)
	for _, m := range assign.FindAllStringSubmatch(string(raw), -1) {
		if !strings.HasPrefix(m[1], "${{") {
			t.Errorf("literal key assignment %q", m[0])
		}
	}
	if regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}|AIza[0-9A-Za-z_-]{20,}`).Match(raw) {
		t.Error("key-like literal in drift.yml")
	}
}
