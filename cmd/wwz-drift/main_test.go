package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

// TestMain keeps every test away from the real keys: the environment keys
// and the home directory (default real-keys.env) are replaced.
func TestMain(m *testing.M) {
	getenv = func(string) string { return "" }
	userHomeDir = func() (string, error) { return "", errors.New("sem home nos testes") }
	os.Exit(m.Run())
}

func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCommands(t *testing.T) {
	if code, out, _ := runCmd(t, "version"); code != exitOK || !strings.HasPrefix(out, "wwz-drift ") || !strings.Contains(out, "commit") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, "help"); code != exitOK || !strings.Contains(out, "record") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, out, errs := runCmd(t, "record", "--providers", "dance"); code != exitUsage || out != "" || !strings.Contains(errs, `"dance"`) {
		t.Errorf("record: %d %q %q", code, out, errs)
	}
	if code, _, errs := runCmd(t, "dance"); code != exitUsage || !strings.Contains(errs, `"dance"`) || !strings.Contains(errs, "Uso:") {
		t.Errorf("unknown: %d %q", code, errs)
	}
}

func TestMainFunc(t *testing.T) {
	var out, errb bytes.Buffer
	origArgs, origExit, origOut, origErr := os.Args, exit, osStdout, osStderr
	t.Cleanup(func() { os.Args, exit, osStdout, osStderr = origArgs, origExit, origOut, origErr })
	got := -1
	exit = func(c int) { got = c }
	osStdout, osStderr = &out, &errb
	os.Args = []string{"wwz-drift", "version"}
	main()
	if got != exitOK || !strings.HasPrefix(out.String(), "wwz-drift ") {
		t.Errorf("main version: %d %q %q", got, out.String(), errb.String())
	}
	os.Args = []string{"wwz-drift", "record", "--bogus"}
	main()
	if got != exitUsage {
		t.Errorf("main record: %d", got)
	}
}

func TestVersionString(t *testing.T) {
	origRead, origVersion, origCommit := readBuildInfo, version, commit
	t.Cleanup(func() { readBuildInfo, version, commit = origRead, origVersion, origCommit })

	version, commit = "", ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main:     debug.Module{Version: "v1.2.3"},
			Settings: []debug.BuildSetting{{Key: "vcs.time", Value: "x"}, {Key: "vcs.revision", Value: "abc123"}},
		}, true
	}
	if s := versionString(); !strings.HasPrefix(s, "wwz-drift v1.2.3 (commit abc123, go") {
		t.Errorf("build info: %s", s)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}
	if s := versionString(); !strings.HasPrefix(s, "wwz-drift dev (commit unknown, ") {
		t.Errorf("devel: %s", s)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if s := versionString(); !strings.HasPrefix(s, "wwz-drift dev (commit unknown, ") {
		t.Errorf("no build info: %s", s)
	}
	version, commit = "v0.1.0", "deadbeef"
	if s := versionString(); !strings.HasPrefix(s, "wwz-drift v0.1.0 (commit deadbeef, ") {
		t.Errorf("ldflags: %s", s)
	}
}
