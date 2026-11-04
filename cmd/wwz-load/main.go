// Command wwz-load is the load generator of White Wizard (spec 015): it
// speaks OpenAI and Anthropic with streaming and reports TTFT, ITL, E2E,
// req/s, tokens/s and errors by type.
//
//	wwz-load [run] [flags]   generate load and print the report (default)
//	wwz-load version         print version, commit and Go version
//	wwz-load controller      coordinate N workers (--instances N)
//	wwz-load worker          run a slice of the load of a controller
//	wwz-load top             live /admin/stats of one or more wwz (ADR 0036)
//	wwz-load agents          concurrent multi-turn agents with tools (ADR 0036)
//	wwz-load claude-code     a client that behaves like Claude Code (ADR 0074)
//
// Exit codes: 0 done (even with request errors), 1 execution failure,
// 2 invalid usage, 3 --fail-on-error-pct exceeded, 4 missing worker.
package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
)

// version and commit are set at build time with
// -ldflags "-X main.version=v0.1.0 -X main.commit=<rev>".
var (
	version = "dev"
	commit  string
)

// Exit codes (spec 015).
const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitErrorRate = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// commands is the subcommand dispatch table.
var commands = map[string]func(ctx context.Context, args []string, stdout, stderr io.Writer) int{
	"run":         runLoad,
	"controller":  runController,
	"worker":      runWorker,
	"agents":      runAgents,
	"top":         runTop,
	"claude-code": runClaudeCode,
}

// run executes the command line args (without the program name) and returns
// the exit code. ctx ending (SIGINT/SIGTERM) interrupts the load and still
// emits the partial report.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help":
		usage(stdout)
		return exitOK
	}
	f, ok := commands[cmd]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "wwz-load: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
	return f(ctx, args, stdout, stderr)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: wwz-load [command] [flags]

Commands:
  run         generate load and print the report (default)
  version     print version, commit and Go version
  controller  coordinate N workers: --listen :9100 --instances N [run flags]
  worker      run a slice of the load: --controller http://host:9100
  agents      N concurrent multi-turn agents with tool calls: --agents N --profile coder --turns M
  top         live /admin/stats: --admin URL (repeatable) --interval 1s [--once]
  claude-code a client that behaves like Claude Code 2.1.308: login, refresh, ask, replay, run
  help        show this help

Run "wwz-load <command> -h" for the flags of a command.
`)
}

// versionString is "wwz-load <version> (commit <rev>, <go version>)".
func versionString() string {
	bi, _ := debug.ReadBuildInfo()
	return versionFrom(version, bi)
}

// versionFrom falls back to the module version and takes the commit from
// -ldflags or else from the build info recorded by the Go toolchain.
func versionFrom(v string, bi *debug.BuildInfo) string {
	rev := ""
	if bi != nil {
		if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
		}
	}
	return fmt.Sprintf("wwz-load %s (commit %s, %s)", v, cmp.Or(commit, rev, "unknown"), runtime.Version())
}
