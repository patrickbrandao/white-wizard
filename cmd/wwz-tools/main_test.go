package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// noSleep skips the simulated latency, still honouring cancellation.
func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestMain(m *testing.M) {
	execOptions = []tools.Option{tools.WithSleep(noSleep)}
	os.Exit(m.Run())
}

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMainFunc(t *testing.T) {
	origArgs, origExit := os.Args, exit
	t.Cleanup(func() { os.Args, exit = origArgs, origExit })
	got := -1
	exit = func(c int) { got = c }
	os.Args = []string{"wwz-tools", "help"}
	main()
	if got != exitOK {
		t.Fatalf("main exit %d", got)
	}
}

func TestRunDispatch(t *testing.T) {
	if code, _, errs := runCmd(t); code != exitUsage || !strings.Contains(errs, "Usage") {
		t.Fatalf("no args: %d %q", code, errs)
	}
	for _, h := range []string{"help", "-h", "--help"} {
		if code, out, _ := runCmd(t, h); code != exitOK || !strings.Contains(out, "serve") {
			t.Fatalf("%s: %d %q", h, code, out)
		}
	}
	if code, _, errs := runCmd(t, "dance"); code != exitUsage || !strings.Contains(errs, `"dance"`) {
		t.Fatalf("unknown: %d %q", code, errs)
	}
	if code, out, _ := runCmd(t, "version"); code != exitOK || !strings.HasPrefix(out, "wwz-tools ") || !strings.Contains(out, "commit") {
		t.Fatalf("version: %d %q", code, out)
	}
}

func TestVersionFrom(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}, Settings: []debug.BuildSetting{{Key: "vcs.time", Value: "x"}, {Key: "vcs.revision", Value: "abc"}}}
	cases := []struct {
		v, rev string
		bi     *debug.BuildInfo
		want   string
	}{
		{"", "", nil, "wwz-tools dev (commit unknown, "},
		{"", "", bi, "wwz-tools v1.2.3 (commit abc, "},
		{"v0.1.0", "def", bi, "wwz-tools v0.1.0 (commit def, "},
		{"", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "wwz-tools dev (commit unknown, "},
	}
	for _, c := range cases {
		if got := versionFrom(c.v, c.rev, c.bi); !strings.HasPrefix(got, c.want) {
			t.Errorf("versionFrom(%q,%q): %q want prefix %q", c.v, c.rev, got, c.want)
		}
	}
	orig := readBuildInfo
	t.Cleanup(func() { readBuildInfo = orig })
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, true }
	if got := versionString(); !strings.HasPrefix(got, "wwz-tools v1.2.3 (commit abc, go") {
		t.Fatalf("versionString: %q", got)
	}
}

func TestList(t *testing.T) {
	code, out, _ := runCmd(t, "list")
	var raw []map[string]any
	if code != exitOK || json.Unmarshal([]byte(out), &raw) != nil || len(raw) != 8 || raw[0]["name"] != "edit_file" {
		t.Fatalf("list: %d %s", code, out)
	}
	code, out, _ = runCmd(t, "list", "--profile", "pesquisa", "--format", "anthropic")
	if code != exitOK || json.Unmarshal([]byte(out), &raw) != nil || len(raw) != 3 || raw[0]["input_schema"] == nil {
		t.Fatalf("list anthropic: %d %s", code, out)
	}
	if code, out, _ := runCmd(t, "list", "--profile", "chat"); code != exitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("chat: %d %q", code, out)
	}
	for _, args := range [][]string{
		{"list", "--profile", "dev"},
		{"list", "--format", "xml"},
		{"list", "--bogus"},
		{"list", "extra"},
	} {
		if code, _, errs := runCmd(t, args...); code != exitUsage || errs == "" {
			t.Errorf("%v: %d %q", args, code, errs)
		}
	}
	if code, _, _ := runCmd(t, "list", "-h"); code != exitOK {
		t.Fatalf("list -h: %d", code)
	}
}

func decodeResult(t *testing.T, out string) callResult {
	t.Helper()
	var r callResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	return r
}

func TestCall(t *testing.T) {
	t.Setenv("WWZ_TOOLS_SEED", "")
	code, out, errs := runCmd(t, "call", "--name", "read_file", "--args", `{"path":"README.md"}`, "--seed", "7", "--profile", "coder")
	r := decodeResult(t, out)
	if code != exitOK || r.Outcome != tools.OutcomeSuccess || r.IsError || r.Content == "" {
		t.Fatalf("read_file: %d %+v %q", code, r, errs)
	}
	// Same seed → identical result (determinism).
	_, out2, _ := runCmd(t, "call", "--name", "read_file", "--args", `{"path":"README.md"}`, "--seed", "7", "--profile", "coder")
	if out2 != out {
		t.Fatalf("not deterministic:\n%s\n%s", out, out2)
	}
	// A tool error still exits 0.
	code, out, _ = runCmd(t, "call", "--name", "read_file", "--args", `{"path":"nao/existe.go"}`)
	if r := decodeResult(t, out); code != exitOK || r.Outcome != tools.OutcomeNatural || !r.IsError {
		t.Fatalf("natural: %d %+v", code, r)
	}
	for _, o := range []tools.Outcome{tools.OutcomeError, tools.OutcomeTimeout, tools.OutcomeInvalidPayload} {
		code, out, _ = runCmd(t, "call", "--name", "list_dir", "--outcome", string(o))
		if r := decodeResult(t, out); code != exitOK || r.Outcome != o {
			t.Fatalf("%s: %d %+v", o, code, r)
		}
	}
	cfg := filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(cfg, []byte("timeout: 2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCmd(t, "call", "--name", "list_dir", "--outcome", "timeout", "--config", cfg)
	if r := decodeResult(t, out); code != exitOK || r.Content != "tool timed out after 2s" || r.LatencyMS != 2000 {
		t.Fatalf("config timeout: %d %+v", code, r)
	}
}

func TestCallFailures(t *testing.T) {
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"call"}, exitUsage, "--name"},
		{[]string{"call", "--name", "x"}, exitUsage, "unknown tool"},
		{[]string{"call", "--name", "web_search", "--profile", "coder"}, exitUsage, "not in profile"},
		{[]string{"call", "--name", "list_dir", "--profile", "dev"}, exitUsage, "invalid profile"},
		{[]string{"call", "--name", "list_dir", "--outcome", "natural_error"}, exitUsage, "invalid outcome"},
		{[]string{"call", "--name", "list_dir", "--seed", "abc"}, exitUsage, "invalid integer"},
		{[]string{"call", "--name", "list_dir", "--config", "/nao/existe.yaml"}, exitError, "tools config"},
	}
	for _, c := range cases {
		if code, _, errs := runCmd(t, c.args...); code != c.code || !strings.Contains(errs, c.msg) {
			t.Errorf("%v: %d %q", c.args, code, errs)
		}
	}
	if code, _, _ := runCmd(t, "call", "-h"); code != exitOK {
		t.Fatalf("call -h: %d", code)
	}
	// Interrupted (SIGINT) during the simulated wait → 1.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"call", "--name", "list_dir"}, &out, &errb); code != exitError || !strings.Contains(errb.String(), "canceled") {
		t.Fatalf("canceled: %d %q", code, errb.String())
	}
}
