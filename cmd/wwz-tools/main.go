// Command wwz-tools is the fake tool server of White Wizard (spec 017,
// "Contrato do wwz-tools"; ADR 0034). Nothing is executed for real: the
// file system is virtual, commands are simulated and nothing reaches the
// network. The core lives in internal/tools.
//
//	wwz-tools serve [--listen :9200] [--config ARQ] [--seed N]
//	wwz-tools list [--profile P] [--format openai|anthropic|raw]
//	wwz-tools call --name N [--args JSON] [--profile P] [--seed N] [--outcome O] [--config ARQ]
//	wwz-tools version
//
// Exit codes: 0 success (including call with is_error: true), 1 execution
// failure (port in use, invalid configuration), 2 invalid usage.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// Set at build time: -ldflags "-X main.version=v0.1.0 -X main.commit=<sha>".
var version, commit string

// Exit codes (spec 017).
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Seams for tests.
var (
	exit          = os.Exit
	readBuildInfo = debug.ReadBuildInfo
	// execOptions are passed to every tools.New (tests inject the clock
	// and the sleep).
	execOptions []tools.Option
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	exit(code)
}

// run executes the command line args (without the program name) and returns
// the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "serve":
		return runServe(ctx, args, stderr)
	case "list":
		return runList(args, stdout, stderr)
	case "call":
		return runCall(ctx, args, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return exitOK
	}
	_, _ = fmt.Fprintf(stderr, "wwz-tools: unknown command %q\n\n", cmd)
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: wwz-tools <command> [flags]

Commands:
  serve    HTTP server: --listen :9200 --config tools.yaml --seed N
  list     print the catalogue: --profile coder|pesquisa|chat|all --format raw|openai|anthropic
  call     run one call in process: --name N --args JSON --profile P --seed N --outcome O
  version  print version, commit and Go version
  help     show this help

Environment: WWZ_TOOLS_LISTEN, WWZ_TOOLS_SEED, WWZ_TOOLS_TOKEN.
`)
}

// newFlagSet returns a flag set that reports to stderr.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("wwz-tools "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args; done reports that the command must return code
// (help → 0, invalid flag or stray argument → 2).
func parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, true
		}
		return exitUsage, true
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return exitUsage, true
	}
	return 0, false
}

// seedFlag registers --seed; the pointer stays nil when the flag is absent
// (so env and file still apply: flag > env > file > default).
func seedFlag(fs *flag.FlagSet) **int64 {
	var p *int64
	fs.Func("seed", "seed (0 = random); overrides WWZ_TOOLS_SEED and the file", func(s string) error {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return errors.New("invalid integer")
		}
		p = &n
		return nil
	})
	return &p
}

// runList prints the catalogue as JSON.
func runList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("list", stderr)
	profile := fs.String("profile", "all", "profile: coder, pesquisa, chat or all")
	format := fs.String("format", "raw", "format: raw, openai or anthropic")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	raw, err := tools.Catalog(tools.Profile(*profile), tools.Format(*format))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-tools list: %v\n", err)
		return exitUsage
	}
	return printJSON(stdout, raw)
}

// callResult is the JSON of a call (CallResult in openapi.yaml).
type callResult struct {
	ToolCallID string        `json:"tool_call_id"`
	Content    string        `json:"content"`
	IsError    bool          `json:"is_error"`
	Outcome    tools.Outcome `json:"outcome"`
	LatencyMS  int64         `json:"latency_ms"`
}

func toCallResult(r tools.Result) callResult {
	return callResult{
		ToolCallID: r.ToolCallID,
		Content:    r.Content,
		IsError:    r.IsError,
		Outcome:    r.Outcome,
		LatencyMS:  r.Latency.Milliseconds(),
	}
}

// runCall executes one call in process on a new session and prints the
// result. A tool error (is_error) still exits 0.
func runCall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("call", stderr)
	name := fs.String("name", "", "tool name (required)")
	argsJSON := fs.String("args", "", "arguments: JSON object (default {})")
	profile := fs.String("profile", "all", "session profile: coder, pesquisa, chat or all")
	outcome := fs.String("outcome", "", "force the outcome: success, error, timeout or invalid_payload")
	cfgPath := fs.String("config", "", "tools.yaml (latency, rates, timeout)")
	seed := seedFlag(fs)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	fail := func(code int, err error) int {
		_, _ = fmt.Fprintf(stderr, "wwz-tools call: %v\n", err)
		return code
	}
	if *name == "" {
		return fail(exitUsage, errors.New("--name is required"))
	}
	p, err := tools.ParseProfile(*profile)
	if err != nil {
		return fail(exitUsage, err)
	}
	o, err := tools.ParseOutcome(*outcome)
	if err != nil {
		return fail(exitUsage, err)
	}
	cfg, err := tools.Load(tools.Flags{Config: *cfgPath, Seed: *seed}, os.Getenv)
	if err != nil {
		return fail(exitError, err)
	}
	ex, _ := tools.New(cfg, execOptions...) // cfg was validated by Load
	info, _ := ex.NewSession(tools.SessionOptions{Profile: p})
	res, err := ex.Call(ctx, tools.CallRequest{
		Session:      info.ID,
		Name:         *name,
		Arguments:    json.RawMessage(*argsJSON),
		ForceOutcome: o,
	})
	switch {
	case errors.Is(err, tools.ErrUnknownTool):
		return fail(exitUsage, err)
	case err != nil:
		return fail(exitError, err)
	}
	b, _ := json.Marshal(toCallResult(res))
	return printJSON(stdout, b)
}

// printJSON writes valid JSON (always produced by encoding/json) indented
// with two spaces.
func printJSON(w io.Writer, raw []byte) int {
	var b bytes.Buffer
	_ = json.Indent(&b, raw, "", "  ")
	b.WriteByte('\n')
	_, _ = w.Write(b.Bytes())
	return exitOK
}

// versionString is "wwz-tools <version> (commit <rev>, <go version>)".
func versionString() string {
	bi, _ := readBuildInfo()
	return versionFrom(version, commit, bi)
}

// versionFrom fills the ldflags values from the build info: the module
// version and the VCS revision recorded by the Go toolchain.
func versionFrom(v, rev string, bi *debug.BuildInfo) string {
	if bi != nil {
		if v == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if rev == "" && s.Key == "vcs.revision" {
				rev = s.Value
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	if rev == "" {
		rev = "unknown"
	}
	return fmt.Sprintf("wwz-tools %s (commit %s, %s)", v, rev, runtime.Version())
}
