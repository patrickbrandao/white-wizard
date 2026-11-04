package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// loadConfig reads the configuration like serve does: -config, else
// WWZ_CONFIG, else built-in defaults; env (WWZ_*) is applied over the YAML
// and the result must pass both config.Validate and the data plane checks.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := server.CheckConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// configPathFlag registers -config with the WWZ_CONFIG fallback.
func configPathFlag(fs *flag.FlagSet, dst *string) {
	fs.StringVar(dst, "config", "", "YAML configuration file (env "+envConfig+"; empty = built-in defaults)")
}

// resolveConfigPath applies the WWZ_CONFIG fallback when -config was not set.
func resolveConfigPath(fs *flag.FlagSet, path string) string {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			set = true
		}
	})
	if set {
		return path
	}
	return os.Getenv(envConfig)
}

// splitList parses a comma-separated flag value.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// configResult is the configuration part of the validate report.
type configResult struct {
	Path    string `json:"path"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Models  int    `json:"models,omitempty"`
	Drivers int    `json:"drivers,omitempty"`
	Keys    int    `json:"keys,omitempty"`
	// Warnings are Config.Warnings(): legacy forms translated and entries
	// nothing refers to.
	Warnings []string `json:"warnings,omitempty"`
}

// scenarioResult is one scenario file in the validate report.
type scenarioResult struct {
	File     string   `json:"file"`
	Name     string   `json:"name,omitempty"`
	Steps    int      `json:"steps,omitempty"`
	OK       bool     `json:"ok"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// validateReport is the --json output of wwz validate.
type validateReport struct {
	OK        bool             `json:"ok"`
	Config    configResult     `json:"config"`
	Scenarios []scenarioResult `json:"scenarios"`
}

// runValidate checks the configuration and scenario files without starting
// anything (specs 003 and 011). Exit 0 when everything is valid, 1 when
// something is not, 2 on bad usage.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wwz validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage: wwz validate [flags] [scenario.yaml | dir ...]\n\nChecks the configuration and the scenarios (the ones given, else scenarios.dir/files).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	var path, tools string
	var asJSON bool
	configPathFlag(fs, &path)
	fs.StringVar(&tools, "tools", "", "comma-separated tools the client under test offers; a scenario step calling another literal tool is an error")
	fs.BoolVar(&asJSON, "json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	path = resolveConfigPath(fs, path)

	rep := validateReport{OK: true, Config: configResult{Path: path, OK: true}, Scenarios: []scenarioResult{}}
	if rep.Config.Path == "" {
		rep.Config.Path = "(built-in defaults)"
	}
	cfg, err := loadConfig(path)
	if err != nil {
		rep.OK, rep.Config.OK, rep.Config.Error = false, false, err.Error()
	} else {
		rep.Config.Models, rep.Config.Drivers, rep.Config.Keys = len(cfg.Models), len(cfg.Drivers), len(cfg.Keys)
	}

	files, err := scenarioFiles(fs.Args(), cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz validate:", err)
		return exitUsage
	}
	opts := scenario.CheckOptions{Tools: splitList(tools)}
	names := map[string]string{}
	var results []scenarioResult
	if len(fs.Args()) == 0 && (cfg == nil || cfg.Scenarios.UseBuiltin()) {
		for _, f := range builtinFiles() {
			r := checkScenarioData(builtinPrefix+f.Name, f.Data, opts)
			if r.Name != "" {
				names[r.Name] = r.File
			}
			results = append(results, r)
		}
	}
	for _, f := range files {
		results = append(results, checkScenarioFile(f, opts))
	}
	for _, r := range results {
		if r.Name == "" || strings.HasPrefix(r.File, builtinPrefix) {
			rep.OK = rep.OK && r.OK
			rep.Scenarios = append(rep.Scenarios, r)
			continue
		}
		// A file replacing a built-in is the documented override.
		if prev, dup := names[r.Name]; dup && !strings.HasPrefix(prev, builtinPrefix) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("scenario %q is also defined in %s; the last one loaded wins", r.Name, prev))
		}
		names[r.Name] = r.File
		rep.OK = rep.OK && r.OK
		rep.Scenarios = append(rep.Scenarios, r)
	}
	rep.Config.Warnings = cfgWarnings(cfg)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printValidate(stdout, stderr, &rep)
	}
	if !rep.OK {
		return exitError
	}
	return exitOK
}

// scenarioFiles expands the arguments (files or directories of *.yaml/*.yml);
// with no arguments it uses scenarios.dir and scenarios.files of cfg.
func scenarioFiles(args []string, cfg *config.Config) ([]string, error) {
	if len(args) == 0 && cfg != nil {
		if cfg.Scenarios.Dir != "" {
			args = append(args, cfg.Scenarios.Dir)
		}
		args = append(args, cfg.Scenarios.Files...)
	}
	var out []string
	for _, a := range args {
		st, err := os.Stat(a) //nolint:gosec // G703: the operator names the files to check
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, a)
			continue
		}
		entries, err := os.ReadDir(a)
		if err != nil {
			return nil, err
		}
		var in []string
		for _, e := range entries {
			if ext := strings.ToLower(filepath.Ext(e.Name())); !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
				in = append(in, filepath.Join(a, e.Name()))
			}
		}
		sort.Strings(in)
		out = append(out, in...)
	}
	return out, nil
}

// checkScenarios runs the scenario checks of wwz validate over
// scenarios.dir and scenarios.files of c; any error fails (serve
// -validate-on-load, I23). Warnings do not.
func checkScenarios(c *config.Config) error {
	files, err := scenarioFiles(nil, c)
	if err != nil {
		return fmt.Errorf("scenarios: %w", err)
	}
	var errs []string
	if c.Scenarios.UseBuiltin() {
		for _, f := range builtinFiles() {
			for _, e := range checkScenarioData(builtinPrefix+f.Name, f.Data, scenario.CheckOptions{}).Errors {
				errs = append(errs, builtinPrefix+f.Name+": "+e)
			}
		}
	}
	for _, f := range files {
		for _, e := range checkScenarioFile(f, scenario.CheckOptions{}).Errors {
			errs = append(errs, f+": "+e)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scenarios invalid (validate-on-load): %s", strings.Join(errs, "; "))
	}
	return nil
}

// builtinPrefix labels the embedded scenarios in the validate report.
const builtinPrefix = "(built-in) "

// cfgWarnings is Config.Warnings() of a configuration that may have failed.
func cfgWarnings(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Warnings()
}

// checkScenarioFile parses and checks one scenario file.
func checkScenarioFile(path string, opts scenario.CheckOptions) scenarioResult {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file to check
	if err != nil {
		return scenarioResult{File: path, Errors: []string{err.Error()}, Warnings: []string{}}
	}
	return checkScenarioData(path, data, opts)
}

// checkScenarioData parses and checks the YAML of one scenario; label names
// it in the report.
func checkScenarioData(label string, data []byte, opts scenario.CheckOptions) scenarioResult {
	r := scenarioResult{File: label, Errors: []string{}, Warnings: []string{}}
	s, err := scenario.ParseYAML(data)
	if err == nil {
		rep := s.Check(opts)
		r.Name, r.Steps = s.Name, len(s.Steps)
		r.Errors = append(r.Errors, rep.Errors...)
		r.Warnings = append(r.Warnings, rep.Warnings...)
	} else {
		r.Errors = append(r.Errors, err.Error())
	}
	r.OK = len(r.Errors) == 0
	return r
}

// printValidate writes the human report: problems on stderr, summary on
// stdout.
func printValidate(stdout, stderr io.Writer, rep *validateReport) {
	if rep.Config.OK {
		_, _ = fmt.Fprintf(stdout, "ok    config %s (%d models, %d drivers, %d keys)\n", rep.Config.Path, rep.Config.Models, rep.Config.Drivers, rep.Config.Keys)
	} else {
		_, _ = fmt.Fprintf(stderr, "FAIL  config %s: %s\n", rep.Config.Path, rep.Config.Error)
	}
	failed, warnings := 0, 0
	for _, w := range rep.Config.Warnings {
		warnings++
		_, _ = fmt.Fprintf(stdout, "        warning: %s\n", w)
	}
	for _, s := range rep.Scenarios {
		if s.OK {
			_, _ = fmt.Fprintf(stdout, "ok    scenario %s (%s, %d steps)\n", s.File, s.Name, s.Steps)
		} else {
			failed++
			_, _ = fmt.Fprintf(stderr, "FAIL  scenario %s\n", s.File)
			for _, e := range s.Errors {
				_, _ = fmt.Fprintf(stderr, "        error: %s\n", e)
			}
		}
		for _, w := range s.Warnings {
			warnings++
			_, _ = fmt.Fprintf(stdout, "        warning: %s\n", w)
		}
	}
	verdict := "valid"
	if !rep.OK {
		verdict = "INVALID"
	}
	cfgState := "ok"
	if !rep.Config.OK {
		cfgState = "invalid"
	}
	_, _ = fmt.Fprintf(stdout, "%s: config %s; %d scenarios, %d failed, %d warnings\n", verdict, cfgState, len(rep.Scenarios), failed, warnings)
}
