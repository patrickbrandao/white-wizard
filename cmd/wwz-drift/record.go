package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/patrickbrandao/white-wizard/internal/drift"
)

// recordFlags are the parsed flags of record.
type recordFlags struct {
	common
	out string
}

func parseRecord(args []string, stderr io.Writer) (*recordFlags, error) {
	fs := flag.NewFlagSet("wwz-drift record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &recordFlags{}
	addCommon(fs, &c.common)
	fs.StringVar(&c.out, "out", drift.DefaultRecordDir, "diretório das fixtures (<out>/<provedor>/shape/<sonda>.json)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := c.finish(fs); err != nil {
		return nil, err
	}
	if c.out == "" {
		return nil, errors.New("--out não pode ser vazio")
	}
	return c, nil
}

// runRecord is the record subcommand: the probes of check against the real
// providers only, each answer written as a normalized fixture (ADR 0014).
// Exit codes: 0 every selected probe recorded (or skipped), 1 a probe in
// error or an execution failure, 2 invalid usage.
func runRecord(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, err := parseRecord(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-drift record: %v\n", err)
		return exitUsage
	}
	red, err := c.loadKeys()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-drift record: %v\n", err)
		return exitError
	}
	rep, err := drift.Record(ctx, c.opts, c.out)
	code := drift.ExitCode(rep, err)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-drift record: %s\n", red.String(err.Error()))
	}
	if rep != nil {
		_ = rep.WriteText(stdout)
	}
	return code
}
