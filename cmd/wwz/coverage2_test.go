package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestSimulateFailsWhenOutputCannotBeWritten(t *testing.T) {
	cleanEnv(t)
	var errb bytes.Buffer
	code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow", "-turns", "1", "-seed", "1"}, failWriter{}, &errb)
	if code != exitError {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

// A scenario that calls `any` tool offers a default one to the simulated client.
func TestSimulateGenericToolWithoutNamedTools(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "any.yaml")
	writeFile(t, path, "name: any-tool\nsteps:\n  - tool_calls:\n      - name: any\n      - name: any_write\n")
	var out, errb bytes.Buffer
	code := runSimulate(context.Background(), []string{"-scenario", path, "-turns", "1", "-seed", "1"}, &out, &errb)
	if code != exitOK || !bytes.Contains(out.Bytes(), []byte("write_file")) {
		t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
}
