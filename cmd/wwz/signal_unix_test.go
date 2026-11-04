//go:build unix

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as the wwz binary: with WWZ_TEST_MAIN=1
// it runs main() instead of the tests (used to exercise real signals).
func TestMain(m *testing.M) {
	if os.Getenv("WWZ_TEST_MAIN") == "1" {
		os.Args = append([]string{"wwz"}, strings.Fields(os.Getenv("WWZ_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

var startedRe = regexp.MustCompile(`data=(\S+) admin=(\S+)`)

// TestBinarySIGHUPAndSIGTERM runs the real main() in a child process,
// reloads the configuration with SIGHUP and stops it with SIGTERM.
func TestBinarySIGHUPAndSIGTERM(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, reloadBase)
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-executes the test binary itself
	cmd.Env = append(os.Environ(), "WWZ_TEST_MAIN=1", "WWZ_ADMIN_TOKEN=tok",
		"WWZ_TEST_ARGS=serve -config "+path+" -port 0 -admin-port 0 -log-format text")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	lines := make(chan string, 1000)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		exited <- cmd.Wait()
	}()
	waitFor := func(substr string) string {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("process ended before %q", substr)
				}
				if strings.Contains(l, substr) {
					return l
				}
			case <-deadline:
				t.Fatalf("timeout waiting for %q", substr)
			}
		}
	}
	m := startedRe.FindStringSubmatch(waitFor("wwz started"))
	if m == nil {
		t.Fatal("no addresses in start log")
	}
	data := m[1]
	chat := func(model string) int {
		code, _, _ := do(t, "POST", url(data, "/v1/chat/completions"), oaiAuth, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return code
	}
	if c := chat("before-reload"); c != 200 {
		t.Fatalf("before SIGHUP: %d", c)
	}

	writeFile(t, path, strings.ReplaceAll(reloadBase, "before-reload", "after-sighup"))
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor("configuration reloaded")
	if c := chat("after-sighup"); c != 200 {
		t.Fatalf("after SIGHUP: %d", c)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor("wwz stopped")
	for range lines { //nolint:revive // drain until the pipe closes
	}
	select {
	case err := <-exited:
		exited <- err // for the cleanup
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}
