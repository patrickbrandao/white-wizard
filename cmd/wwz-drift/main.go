// Command wwz-drift compares the dialects of the wwz drivers with the real
// provider APIs (I24, spec 017, ADR 0035). It runs only on the human's
// machine or in CI with secrets, never on the heavy server (ADR 0014).
//
//	wwz-drift [check] [flags]   live probes, real × wwz, divergence report (default)
//	wwz-drift record [flags]    the probes on the real side only, written as
//	                            normalized fixtures under testdata/real (ADR 0014)
//	wwz-drift version           version, commit and Go version
//
// Exit codes of check: 0 no divergence, 3 a counted divergence (wins over
// 1), 1 a probe in error or an execution failure, 2 invalid usage. record
// uses 0, 1 and 2 with the same meaning.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/patrickbrandao/white-wizard/internal/drift"
)

// version and commit are set at build time with
// -ldflags "-X main.version=v0.1.0 -X main.commit=<rev>".
var version, commit string

// Exit codes (spec 017).
const (
	exitOK    = drift.ExitOK
	exitError = 1
	exitUsage = 2
)

// Seams replaced in tests.
var (
	exit                         = os.Exit
	readBuildInfo                = debug.ReadBuildInfo
	osStdout, osStderr io.Writer = os.Stdout, os.Stderr
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], osStdout, osStderr)
	stop()
	exit(code)
}

// run executes the command line (without the program name) and returns the
// exit code; ctx ending (SIGINT/SIGTERM) interrupts the probes.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "check"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "check":
		return runCheck(ctx, args, stdout, stderr)
	case "record":
		return runRecord(ctx, args, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help":
		usage(stdout)
		return exitOK
	}
	_, _ = fmt.Fprintf(stderr, "wwz-drift: comando desconhecido %q\n\n", cmd)
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Uso: wwz-drift [comando] [flags]

Comandos:
  check     sondas ao vivo, real × wwz, e relatório de divergências (padrão)
  record    grava as respostas reais das sondas como fixtures normalizadas
  version   versão, commit e versão do Go
  help      esta ajuda

"wwz-drift check -h" e "wwz-drift record -h" listam as flags. Códigos de
saída: 0 sem divergência (record: tudo gravado), 3 divergência, 1 erro de
execução ou sonda em erro, 2 uso inválido.
`)
}

// toolVersion is the injected version, else the module version recorded by
// the Go toolchain, else "dev".
func toolVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := readBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// versionString is "wwz-drift <version> (commit <rev>, <go version>)".
func versionString() string {
	rev := commit
	if rev == "" {
		rev = "unknown"
		if bi, ok := readBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					rev = s.Value
				}
			}
		}
	}
	return fmt.Sprintf("wwz-drift %s (commit %s, %s)", toolVersion(), rev, runtime.Version())
}
