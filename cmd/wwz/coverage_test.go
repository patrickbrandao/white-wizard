package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/scenarios"
)

func TestServeAdminPortInUse(t *testing.T) {
	cleanEnv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	var out, errb syncBuffer
	code := runServe(context.Background(), []string{"-port", "0", "-admin-listen", busy.Addr().String()}, &out, &errb, func(addrs) { t.Error("must not start") })
	if code != exitError || !strings.Contains(errb.String(), "control plane listen") {
		t.Fatalf("code %d, stderr:\n%s", code, errb.String())
	}
}

func TestLoadScenariosLogsInvalidFilesOfTheDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.yaml"), "name: bad\nsteps: []\n")
	writeFile(t, filepath.Join(dir, "ok.yaml"), "name: ok\nsteps:\n  - content: hi\n")
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	reg := scenario.NewRegistry()
	loadScenarios(reg, &config.Config{Scenarios: config.ScenariosConfig{Dir: dir}}, log)
	if reg.Get("ok") == nil || reg.Get("bad") != nil {
		t.Fatalf("registry: %v", reg.Names())
	}
	if !strings.Contains(logs.String(), "scenario skipped") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	// A scenario of the directory that replaces a built-in is logged as an override.
	builtin := scenarios.Files()[0]
	s, err := scenario.ParseYAML(builtin.Data)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "over.yaml"), "name: "+s.Name+"\nsteps:\n  - content: mine\n")
	logs = syncBuffer{}
	reg = scenario.NewRegistry()
	loadScenarios(reg, &config.Config{Scenarios: config.ScenariosConfig{Dir: dir}}, log)
	if !strings.Contains(logs.String(), "scenario overridden") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestCheckScenarioFileMissing(t *testing.T) {
	r := checkScenarioFile(filepath.Join(t.TempDir(), "absent.yaml"), scenario.CheckOptions{})
	if len(r.Errors) != 1 || r.Warnings == nil {
		t.Fatalf("result = %+v", r)
	}
}

func TestScenarioFilesUnreadableDirectory(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("directory permissions are not enforced (running as root)")
	}
	if _, err := scenarioFiles([]string{locked}, nil); err == nil {
		t.Fatal("expected a read error")
	}
}

func TestSimulateSkipsUnusableFilesOfTheScenariosDir(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.yaml"), "name: bad\nsteps: []\n")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow", "-scenarios-dir", dir, "-turns", "1", "-seed", "1"}, &out, &errb)
	if code != exitOK {
		t.Fatalf("code %d: %s", code, errb.String())
	}
}

func TestSimulateStopsWhenTheContextIsCancelled(t *testing.T) {
	cleanEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	code := runSimulate(ctx, []string{"-scenario", "file-agent-flow", "-turns", "3", "-seed", "1"}, &out, &errb)
	if code != exitError {
		t.Fatalf("code %d, want %d (stderr %q)", code, exitError, errb.String())
	}
}

func TestLoadConfigRejectsDataPlaneErrors(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, "server: {trusted_proxies: ['garbage']}\n")
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error for an invalid trusted proxy")
	}
	_ = io.Discard
}
