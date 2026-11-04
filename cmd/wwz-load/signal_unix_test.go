//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// TestMain lets the test binary act as wwz-load: with WWZ_LOAD_TEST_MAIN=1 it
// runs main() instead of the tests (used to exercise real signals).
func TestMain(m *testing.M) {
	if os.Getenv("WWZ_LOAD_TEST_MAIN") == "1" {
		os.Args = append([]string{"wwz-load"}, strings.Fields(os.Getenv("WWZ_LOAD_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

// TestBinarySIGINT: SIGINT stops the starts and still prints the partial
// report with "interrupted": true and exit 0.
func TestBinarySIGINT(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fake.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-executes the test binary itself
	cmd.Env = append(os.Environ(), "WWZ_LOAD_TEST_MAIN=1",
		"WWZ_LOAD_TEST_ARGS=--target "+fake.URL+" --stream=false --rps 50 --duration 1m --output json --progress 0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("no exit after SIGINT")
	}
	var rep loadgen.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || !rep.Interrupted || rep.Total.Requests == 0 {
		t.Fatalf("report %v: %s", err, out.String())
	}
}
