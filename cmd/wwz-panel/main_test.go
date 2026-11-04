package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
)

var fast = auth.Argon2Hasher{Memory: 64, Time: 1, Threads: 1}

// TestMain lets the test binary act as wwz-panel: with
// WWZ_PANEL_TEST_MAIN=1 it runs main() (arguments in WWZ_PANEL_TEST_ARGS)
// instead of the tests, to exercise real signals and exit codes.
func TestMain(m *testing.M) {
	hasher, authHasher = fast, fast
	if os.Getenv("WWZ_PANEL_TEST_MAIN") == "1" {
		os.Args = append([]string{"wwz-panel"}, strings.Fields(os.Getenv("WWZ_PANEL_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

func runCmd(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunCommands(t *testing.T) {
	if code, out, _ := runCmd(t, "", "version"); code != exitOK || !strings.HasPrefix(out, "wwz-panel ") || !strings.Contains(out, "commit") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := runCmd(t, "", "help"); code != exitOK || !strings.Contains(out, "hash-password") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, errs := runCmd(t, "", "dance"); code != exitUsage || !strings.Contains(errs, `"dance"`) {
		t.Fatalf("desconhecido: %d %q", code, errs)
	}
	t.Setenv("WWZ_PANEL_BACKUP_DIR", "")
	if code, _, errs := runCmd(t, "", "backup"); code != exitUsage || !strings.Contains(errs, "sem diretório") {
		t.Fatalf("backup sem diretório: %d %q", code, errs)
	}
	if code, _, errs := runCmd(t, "", "restore"); code != exitUsage || !strings.Contains(errs, "um arquivo") {
		t.Fatalf("restore sem arquivo: %d %q", code, errs)
	}
}

func TestVersionString(t *testing.T) {
	orig := readBuildInfo
	t.Cleanup(func() { readBuildInfo = orig })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main:     debug.Module{Version: "v1.2.3"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.time", Value: "x"}},
		}, true
	}
	if s := versionString(); !strings.HasPrefix(s, "wwz-panel v1.2.3 (commit abc123, go") {
		t.Fatal(s)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if s := versionString(); !strings.HasPrefix(s, "wwz-panel dev (commit unknown, ") {
		t.Fatal(s)
	}
	// -ldflags "-X main.commit=<rev>" wins over the VCS revision.
	oldCommit := commit
	t.Cleanup(func() { commit = oldCommit })
	commit = "def456"
	if s := versionString(); !strings.HasPrefix(s, "wwz-panel dev (commit def456, ") {
		t.Fatal(s)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("stdin quebrado") }

type failHasher struct{}

func (failHasher) Hash(string) (string, error) { return "", errors.New("sem entropia") }

func TestHashPassword(t *testing.T) {
	code, out, _ := runCmd(t, "segredo\r\n", "hash-password")
	ok, err := auth.Verify("segredo", strings.TrimSpace(out))
	if code != exitOK || !ok || err != nil || !strings.HasPrefix(out, "$argon2id$") {
		t.Fatalf("hash: %d %q %v", code, out, err)
	}
	if code, out, _ := runCmd(t, "sem-quebra", "hash-password"); code != exitOK || out == "" {
		t.Fatalf("sem quebra de linha: %d", code)
	}
	if code, _, _ := runCmd(t, "x\n", "hash-password", "extra"); code != exitUsage {
		t.Fatalf("argumento: %d", code)
	}
	if code, _, errs := runCmd(t, "\n", "hash-password"); code != exitUsage || !strings.Contains(errs, "vazia") {
		t.Fatalf("vazia: %d %q", code, errs)
	}
	var out2, errb bytes.Buffer
	if code := runHashPassword(nil, errReader{}, &out2, &errb); code != exitError {
		t.Fatalf("stdin quebrado: %d", code)
	}
	hasher = failHasher{}
	t.Cleanup(func() { hasher = fast })
	if code, _, errs := runCmd(t, "x\n", "hash-password"); code != exitError || !strings.Contains(errs, "entropia") {
		t.Fatalf("hash falhando: %d %q", code, errs)
	}
}
