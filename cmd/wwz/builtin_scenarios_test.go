package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

var builtinNames = []string{"file-agent-flow", "long-reasoning", "multi-tool-parallel", "refusal", "retry-on-error", "search-then-answer"}

func TestValidateListsBuiltinScenarios(t *testing.T) {
	cleanEnv(t)
	code, out, errs := runWWZ(t, "validate")
	if code != exitOK || errs != "" || !strings.Contains(out, "valid: config ok; 6 scenarios, 0 failed, 0 warnings") {
		t.Fatalf("code %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	for _, n := range builtinNames {
		if !strings.Contains(out, "(built-in) "+n+".yaml") {
			t.Errorf("validate lacks %s:\n%s", n, out)
		}
	}

	// scenarios.builtin: false removes them.
	dir := t.TempDir()
	cfg := tmpYAML(t, dir, "off.yaml", "scenarios: {builtin: false}\n")
	code, out, _ = runWWZ(t, "validate", "-config", cfg)
	if code != exitOK || !strings.Contains(out, "0 scenarios") {
		t.Fatalf("builtin false: code %d\n%s", code, out)
	}

	// A file in scenarios.dir with a built-in name replaces it, no warning.
	sdir := t.TempDir()
	tmpYAML(t, sdir, "refusal.yaml", "name: refusal\nsteps:\n  - id: s\n    content: no\n")
	cfg = tmpYAML(t, dir, "dir.yaml", "scenarios: {dir: "+sdir+"}\n")
	code, out, _ = runWWZ(t, "validate", "-config", cfg)
	if code != exitOK || !strings.Contains(out, "7 scenarios, 0 failed, 0 warnings") {
		t.Fatalf("override: code %d\n%s", code, out)
	}
}

func TestValidatePrintsConfigWarnings(t *testing.T) {
	cleanEnv(t)
	cfg := tmpYAML(t, t.TempDir(), "w.yaml", "accounts: [{id: orphan}]\n")
	code, out, _ := runWWZ(t, "validate", "-config", cfg)
	if code != exitOK || !strings.Contains(out, `accounts[0] "orphan" is not referenced`) {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestLoadScenariosOrderAndOverride(t *testing.T) {
	dir := t.TempDir()
	tmpYAML(t, dir, "refusal.yaml", "name: refusal\ndescription: from dir\nsteps:\n  - id: s\n    content: no\n")
	file := tmpYAML(t, t.TempDir(), "extra.yaml", "name: file-agent-flow\ndescription: from file\nsteps:\n  - id: s\n    content: no\n")
	cfg := &config.Config{}
	cfg.Scenarios.Dir = dir
	cfg.Scenarios.Files = []string{file}
	var logs bytes.Buffer
	log := newLogger(&config.Config{Logging: config.LoggingConfig{Level: "info"}}, &logs)
	reg := scenario.NewRegistry()
	loadScenarios(reg, cfg, log)
	if got := reg.Names(); len(got) != 6 {
		t.Fatalf("names %v", got)
	}
	if reg.Get("refusal").Description != "from dir" || reg.Get("file-agent-flow").Description != "from file" {
		t.Fatal("later source did not replace the built-in")
	}
	if n := strings.Count(logs.String(), "scenario overridden"); n != 2 {
		t.Fatalf("overridden logged %d times:\n%s", n, logs.String())
	}

	// Built-ins off: a reload drops them.
	off := false
	cfg2 := &config.Config{}
	cfg2.Scenarios.Builtin = &off
	loadScenarios(reg, cfg2, log)
	if got := reg.Names(); len(got) != 0 {
		t.Fatalf("after builtin false: %v", got)
	}
}

func TestServeBuiltinScenariosInAdmin(t *testing.T) {
	cleanEnv(t)
	in := startServe(t)
	code, _, body := do(t, "GET", url(in.a.Admin, "/admin/scenarios"), nil, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, n := range builtinNames {
		if !strings.Contains(body, n) {
			t.Errorf("/admin/scenarios lacks %s: %s", n, body)
		}
	}
}

func TestSimulateBuiltinScenarios(t *testing.T) {
	cleanEnv(t)
	t.Chdir(t.TempDir()) // no ./scenarios: the names come from the binary
	for _, n := range builtinNames {
		code, out, errs := runWWZ(t, "simulate", "-scenario", n, "-seed", "1")
		if code != exitOK || errs != "" || !strings.Contains(out, "scenario="+n) {
			t.Errorf("%s: code %d stderr %s", n, code, errs)
		}
	}
	off := tmpYAML(t, t.TempDir(), "off.yaml", "scenarios: {builtin: false}\n")
	if code, _, errs := runWWZ(t, "simulate", "-config", off, "-scenario", "refusal"); code == exitOK || !strings.Contains(errs, "not found") {
		t.Fatalf("builtin false: code %d %s", code, errs)
	}
}

func TestSimulateRefusalAndParallel(t *testing.T) {
	cleanEnv(t)
	t.Chdir(t.TempDir())
	_, out, _ := runWWZ(t, "simulate", "-scenario", "refusal", "-turns", "2", "-seed", "1")
	if !strings.Contains(out, "step=refused") && !strings.Contains(out, "content_filter") {
		t.Logf("refusal output:\n%s", out)
	}
	_, out, _ = runWWZ(t, "simulate", "-scenario", "multi-tool-parallel", "-turns", "1", "-seed", "1")
	if n := strings.Count(out, `"type": "function"`); n != 3 {
		t.Fatalf("parallel tool calls: %d\n%s", n, out)
	}
}
