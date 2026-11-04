package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/drift"
)

// defaultKeysFile is the keys file read when --keys-file is not given.
const defaultKeysFile = "~/.config/white-wizard/real-keys.env"

// Seams replaced in tests: no test reads the real keys file or the real
// environment keys.
var (
	userHomeDir = os.UserHomeDir
	getenv      = os.Getenv
	now         = time.Now
)

// pairs is a repeatable P=V flag (--model, --base-url).
type pairs map[string]string

func (p pairs) String() string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + p[k]
	}
	return strings.Join(out, ",")
}

func (p pairs) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if !ok || k == "" || v == "" {
		return fmt.Errorf("esperado PROVEDOR=VALOR, recebido %q", s)
	}
	p[k] = v
	return nil
}

// splitList splits a comma list, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// resolveKeysFile expands the default path; without a home directory there
// is no default file (not an error).
func resolveKeysFile(path string, explicit bool) string {
	if explicit || path != defaultKeysFile {
		return path
	}
	home, err := userHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "white-wizard", "real-keys.env")
}

// common are the flags shared by check and record.
type common struct {
	opts         drift.Options
	keysFile     string
	keysExplicit bool
	providers    *string
	probes       *string
	models       pairs
	bases        pairs
}

// addCommon registers the flags shared by check and record on fs.
func addCommon(fs *flag.FlagSet, c *common) {
	c.models, c.bases = pairs{}, pairs{}
	c.providers = fs.String("providers", strings.Join(drift.DefaultProviders, ","), "provedores a sondar (azure só com --base-url)")
	fs.StringVar(&c.keysFile, "keys-file", defaultKeysFile, "arquivo CHAVE=valor com as chaves reais (a variável de ambiente vence)")
	fs.Var(c.models, "model", "modelo da sonda por provedor, P=M (repetível)")
	fs.Var(c.bases, "base-url", "base do provedor real, P=URL (repetível)")
	fs.BoolVar(&c.opts.Real, "real", false, "confirma sondar a base real dos provedores com chave e sem --base-url")
	c.probes = fs.String("probes", strings.Join(drift.ProbeNames, ","), "subconjunto das sondas")
	fs.StringVar(&c.opts.BudgetFile, "budget-file", drift.DefaultBudgetFile, "gasto acumulado por provedor")
	fs.Float64Var(&c.opts.BudgetUSD, "budget-usd", drift.DefaultBudgetUSD, "teto de gasto por provedor (US$)")
	fs.DurationVar(&c.opts.Timeout, "timeout", drift.DefaultTimeout, "teto por sonda")
}

// finish validates the arguments left and fills the options from the
// shared flags, after fs.Parse.
func (c *common) finish(fs *flag.FlagSet) error {
	fs.Visit(func(f *flag.Flag) { c.keysExplicit = c.keysExplicit || f.Name == "keys-file" })
	if fs.NArg() > 0 {
		return fmt.Errorf("argumento inesperado %q", fs.Arg(0))
	}
	c.opts.Providers, c.opts.Probes = splitList(*c.providers), splitList(*c.probes)
	if len(c.opts.Providers) == 0 || len(c.opts.Probes) == 0 {
		return errors.New("--providers e --probes não podem ser vazios")
	}
	if len(c.models) > 0 {
		c.opts.Models = c.models
	}
	if len(c.bases) > 0 {
		c.opts.BaseURLs = c.bases
	}
	return nil
}

// loadKeys reads the real keys into the options; the redactor masks them
// in every message.
func (c *common) loadKeys() (*drift.Redactor, error) {
	keys, err := drift.LoadKeys(resolveKeysFile(c.keysFile, c.keysExplicit), c.keysExplicit, getenv)
	if err != nil {
		return nil, err
	}
	secrets := make([]string, 0, len(keys))
	for _, k := range keys {
		secrets = append(secrets, k)
	}
	c.opts.Keys = keys
	c.opts.ToolVersion = toolVersion()
	c.opts.Now = now
	return drift.NewRedactor(secrets...), nil
}

// checkFlags are the parsed flags of check.
type checkFlags struct {
	common
	ignore string
	output string
	out    string
}

func parseCheck(args []string, stderr io.Writer) (*checkFlags, error) {
	fs := flag.NewFlagSet("wwz-drift check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &checkFlags{}
	addCommon(fs, &c.common)
	fs.StringVar(&c.opts.WWZURL, "wwz", "", "base do data plane do wwz (padrão: wwz em processo)")
	fs.StringVar(&c.opts.WWZKey, "wwz-key", drift.DefaultWWZKey, "chave enviada ao wwz")
	fs.StringVar(&c.ignore, "ignore", "", "YAML com divergências aceitas [{provider, probe, kind, path, reason}]")
	fs.StringVar(&c.opts.SaveRaw, "save-raw", "", "grava as respostas brutas neste diretório (sob tmp/)")
	fs.StringVar(&c.output, "output", "text", "formato do relatório: text|json")
	fs.StringVar(&c.out, "out", "", "arquivo do relatório (padrão: stdout)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := c.finish(fs); err != nil {
		return nil, err
	}
	if c.output != "text" && c.output != "json" {
		return nil, fmt.Errorf("--output %q inválido (use text ou json)", c.output)
	}
	return c, nil
}

// runCheck is the check subcommand.
func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, err := parseCheck(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-drift check: %v\n", err)
		return exitUsage
	}
	red, err := c.loadKeys()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-drift check: %v\n", err)
		return exitError
	}
	fail := func(code int, err error) int {
		_, _ = fmt.Fprintf(stderr, "wwz-drift check: %s\n", red.String(err.Error()))
		return code
	}
	if c.ignore != "" {
		data, err := os.ReadFile(filepath.Clean(c.ignore))
		if err != nil {
			return fail(exitError, err)
		}
		if c.opts.Ignores, err = drift.ParseIgnores(data); err != nil {
			return fail(exitUsage, err)
		}
	}
	rep, err := drift.Run(ctx, c.opts)
	code := drift.ExitCode(rep, err)
	if err != nil {
		fail(code, err)
	}
	if rep == nil {
		return code
	}
	if werr := writeReport(rep, c.output, c.out, stdout); werr != nil {
		fail(exitError, werr)
		if code == exitOK {
			code = exitError
		}
	}
	return code
}

// writeReport writes the report in format to path (empty = w).
func writeReport(rep *drift.Report, format, path string, w io.Writer) error {
	var b bytes.Buffer
	if format == "json" {
		_ = rep.WriteJSON(&b) // a bytes.Buffer never fails
	} else {
		_ = rep.WriteText(&b)
	}
	if path == "" {
		_, err := w.Write(b.Bytes())
		return err
	}
	return os.WriteFile(filepath.Clean(path), b.Bytes(), 0o600)
}
