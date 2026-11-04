package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/patrickbrandao/white-wizard/internal/loadgen/top"
)

// Defaults of the top flags (spec 015, "wwz-load top").
const (
	defaultTopAdmin    = "http://127.0.0.1:9000"
	minTopInterval     = 100 * time.Millisecond
	defaultTopWidth    = 80
	ansiAltScreenOn    = "\x1b[?1049h\x1b[?25l"
	ansiAltScreenOff   = "\x1b[?25h\x1b[?1049l"
	topOutputText      = "text"
	topOutputJSON      = "json"
	topDefaultRows     = 10
	topDefaultInterval = time.Second
	topDefaultTimeout  = 2 * time.Second
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type topOptions struct {
	admins            stringList
	token             string
	interval, timeout time.Duration
	once              bool
	output, sort      string
	rows              int
}

func parseTop(args []string, stderr io.Writer) (*topOptions, error) {
	o := &topOptions{}
	fs := flag.NewFlagSet("wwz-load top", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(&o.admins, "admin", "wwz control plane (repeatable; default "+defaultTopAdmin+")")
	fs.StringVar(&o.token, "token", "", "bearer (default $WWZ_ADMIN_TOKEN)")
	fs.DurationVar(&o.interval, "interval", topDefaultInterval, "interval between samples (≥ 100ms)")
	fs.BoolVar(&o.once, "once", false, "take two samples --interval apart, print one frame and exit")
	fs.StringVar(&o.output, "output", topOutputText, "frame format with --once: text or json")
	fs.StringVar(&o.sort, "sort", top.Sorts[0], "model table order: "+strings.Join(top.Sorts, ", "))
	fs.IntVar(&o.rows, "rows", topDefaultRows, "rows per table")
	fs.DurationVar(&o.timeout, "timeout", topDefaultTimeout, "ceiling of each GET")
	set := map[string]bool{}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case fs.NArg() > 0:
		return nil, usageErr("unexpected argument %q", fs.Arg(0))
	case o.interval < minTopInterval:
		return nil, usageErr("--interval %s: must be ≥ %s", o.interval, minTopInterval)
	case o.timeout <= 0:
		return nil, usageErr("--timeout %s: must be > 0", o.timeout)
	case o.output != topOutputText && o.output != topOutputJSON:
		return nil, usageErr("--output %q: want text or json", o.output)
	case o.output == topOutputJSON && !o.once:
		return nil, usageErr("--output json requires --once")
	case !top.ValidSort(o.sort):
		return nil, usageErr("--sort %q: want %s", o.sort, strings.Join(top.Sorts, ", "))
	case o.rows < 1:
		return nil, usageErr("--rows %d: must be ≥ 1", o.rows)
	}
	if len(o.admins) == 0 {
		o.admins = stringList{defaultTopAdmin}
	}
	for _, a := range o.admins {
		u, err := url.Parse(a)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, usageErr("--admin %q: want http(s)://host[:port]", a)
		}
	}
	if !set["token"] {
		o.token = os.Getenv("WWZ_ADMIN_TOKEN")
	}
	return o, nil
}

// terminal is the TTY seen by top: raw mode, width and the keys.
type terminal interface {
	MakeRaw() (restore func() error, err error)
	Width() int
	Keys() io.Reader
}

// osTerminal is the real terminal (golang.org/x/term, ADR 0036).
type osTerminal struct{ in, out *os.File }

func (t osTerminal) MakeRaw() (func() error, error) {
	st, err := termMakeRaw(int(t.in.Fd()))
	if err != nil {
		return nil, err
	}
	return func() error { return termRestore(int(t.in.Fd()), st) }, nil
}

func (t osTerminal) Width() int {
	w, _, err := termGetSize(int(t.out.Fd()))
	if err != nil || w <= 0 {
		return defaultTopWidth
	}
	return w
}

func (t osTerminal) Keys() io.Reader { return t.in }

// Seams for tests.
var (
	termMakeRaw    = term.MakeRaw
	termRestore    = term.Restore
	termGetSize    = term.GetSize
	termIsTerminal = term.IsTerminal
	topStdin       = os.Stdin
	openTerminal   = func(stdout io.Writer) (terminal, bool) {
		out, ok := stdout.(*os.File)
		if !ok || !termIsTerminal(int(out.Fd())) || !termIsTerminal(int(topStdin.Fd())) {
			return nil, false
		}
		return osTerminal{in: topStdin, out: out}, true
	}
	topClient = &http.Client{}
)

// runTop is `wwz-load top`: exit 0 on q/signal/end of --once, 1 when no
// instance answers the first sample or on 401, 2 on invalid usage.
func runTop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseTop(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load top: %v\n", err)
		return exitUsage
	}
	mon := top.New(top.Config{
		Admins: o.admins, Token: o.token, Interval: o.interval, Timeout: o.timeout,
		Sort: o.sort, Rows: o.rows, Client: topClient,
	})
	switch t, tty := openTerminal(stdout); {
	case o.once:
		width := 0
		if tty {
			width = t.Width()
		}
		err = mon.Once(ctx, stdout, o.output == topOutputJSON, width)
	case tty:
		err = interactive(ctx, mon, t, stdout)
	default:
		err = mon.Stream(ctx, stdout, 0)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load top: %v\n", err)
		return exitError
	}
	return exitOK
}

// interactive runs the TUI on the alternate screen in raw mode; the terminal
// is restored on return, on a signal (ctx) and on panic (deferred).
func interactive(ctx context.Context, mon *top.Monitor, t terminal, stdout io.Writer) error {
	restore, err := t.MakeRaw()
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	_, _ = io.WriteString(stdout, ansiAltScreenOn)
	defer func() {
		_, _ = io.WriteString(stdout, ansiAltScreenOff)
		_ = restore()
	}()
	return mon.Interactive(ctx, top.Screen{Keys: t.Keys(), Out: stdout, Width: t.Width})
}
