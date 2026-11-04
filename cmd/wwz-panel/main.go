// Command wwz-panel is the White Wizard panel (spec 016): screens, the JSON
// API of cmd/wwz-panel/openapi.yaml, the ev:stream consumer and the SQLite
// backup.
//
//	wwz-panel [serve] [-config F] [-listen A]  start the panel (default)
//	wwz-panel backup [--dir D] [-config F]     snapshot the SQLite
//	wwz-panel restore [-config F] <file>       restore a snapshot (panel stopped)
//	wwz-panel hash-password                    argon2id hash of the password on stdin
//	wwz-panel version                          version, commit and Go
//
// Exit codes: 0 ok, 1 failure, 2 invalid usage. Configuration precedence:
// flag > WWZ_PANEL_* > panel.yaml > default.
package main

import (
	"bufio"
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

	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
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

// Password hashing: hasher for the bootstrap and hash-password,
// authHasher for the dummy hash of unknown users. Tests lower the cost.
var (
	hasher     store.Hasher = auth.Argon2Hasher{}
	authHasher              = auth.Argon2Hasher{}
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the command line (without the program name) and returns the
// exit code. ctx ending (SIGINT/SIGTERM) shuts serve down gracefully.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(ctx, args, stdout, stderr, nil)
	case "backup":
		return runBackup(ctx, args, stdout, stderr)
	case "restore":
		return runRestore(ctx, args, stdout, stderr)
	case "hash-password":
		return runHashPassword(args, stdin, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help":
		usage(stdout)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "wwz-panel: comando desconhecido %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Uso: wwz-panel [comando] [flags]

Comandos:
  serve          sobe o painel (padrão); flags -config e -listen
  backup         snapshot do SQLite (--dir D; padrão WWZ_PANEL_BACKUP_DIR)
  restore        restaura um snapshot com o painel parado (restore <arquivo>)
  hash-password  lê a senha do stdin e imprime o hash argon2id (PHC) para o panel.yaml
  version        versão, commit e Go
  help           esta ajuda
`)
}

// runHashPassword reads one line from stdin and prints its argon2id hash.
func runHashPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(stderr, "wwz-panel hash-password: sem argumentos; a senha vem do stdin")
		return exitUsage
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	pw := strings.TrimRight(line, "\r\n")
	if err != nil && err != io.EOF {
		_, _ = fmt.Fprintln(stderr, "wwz-panel hash-password:", err)
		return exitError
	}
	if pw == "" {
		_, _ = fmt.Fprintln(stderr, "wwz-panel hash-password: senha vazia no stdin")
		return exitUsage
	}
	h, err := hasher.Hash(pw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz-panel hash-password:", err)
		return exitError
	}
	_, _ = fmt.Fprintln(stdout, h)
	return exitOK
}

// readBuildInfo is debug.ReadBuildInfo (tests replace it).
var readBuildInfo = debug.ReadBuildInfo

// versionString is "wwz-panel <version> (commit <rev>, <go>)", the version
// falling back to the module version recorded by the Go toolchain.
func versionString() string {
	v, rev := version, ""
	if bi, ok := readBuildInfo(); ok {
		if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev = s.Value
			}
		}
	}
	return fmt.Sprintf("wwz-panel %s (commit %s, %s)", v, cmp.Or(commit, rev, "unknown"), runtime.Version())
}
