// Command wwz is the White Wizard simulator: a fake LLM provider (OpenAI,
// Anthropic, OpenRouter, OpenCode Go) with a fasthttp data plane and a
// net/http control plane.
//
//	wwz [serve] [flags]   start the data and control planes (default)
//	wwz validate [files]  check the configuration and scenario files
//	wwz simulate          run a scenario in memory, turn by turn
//	wwz version           print the version
//
// Configuration precedence: flag > env (WWZ_*) > YAML > default (spec 003).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
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

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the command line args (without the program name) and returns
// the process exit code. ctx ending (SIGINT/SIGTERM) shuts serve down
// gracefully.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(ctx, args, stdout, stderr, nil)
	case "validate":
		return runValidate(args, stdout, stderr)
	case "simulate":
		return runSimulate(ctx, args, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help":
		usage(stdout)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "wwz: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: wwz [command] [flags]

Commands:
  serve      start the data plane and the control plane (default)
  validate   check the configuration and scenario files
  simulate   run a scenario in memory and print each response
  version    print the version
  help       show this help

Run "wwz <command> -h" for the flags of a command.
`)
}

// readBuildInfo is debug.ReadBuildInfo (tests replace it).
var readBuildInfo = debug.ReadBuildInfo

// versionString is "wwz <version>", falling back to the module version
// recorded by the Go toolchain when no -ldflags version was given.
func versionString() string {
	v := version
	if v == "dev" {
		if bi, ok := readBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	if commit != "" {
		return "wwz " + v + " (commit " + commit + ")"
	}
	return "wwz " + v
}
