package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// Defaults of the run flags (spec 015, "Flags de run").
const (
	defaultTarget = "http://127.0.0.1:8000"
	defaultModel  = "wwz-fast"
	defaultKey    = "wwz-key-01"
	defaultPrompt = "Escreva um parágrafo curto sobre testes de carga."
)

// Seams for tests: the plan is validated before Run and the template has no
// tools, so these cannot fail from the command line.
var (
	newUnit             = loadgen.NewRequestUnit
	newConversationUnit = loadgen.NewConversationUnit
	runLoadgen          = loadgen.Run
)

// errUsage marks an invalid command line (exit 2).
var errUsage = errors.New("usage")

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// headerList is the repeatable --header flag.
type headerList []loadgen.Header

func (h *headerList) String() string {
	parts := make([]string, len(*h))
	for i, x := range *h {
		parts[i] = x.Key + ": " + x.Value
	}
	return strings.Join(parts, ", ")
}

func (h *headerList) Set(s string) error {
	x, err := loadgen.ParseHeader(s)
	if err != nil {
		return err
	}
	*h = append(*h, x)
	return nil
}

// runOptions are the parsed and validated flags of run.
type runOptions struct {
	target, protocolS, path, model, key string
	protocol                            loadgen.Protocol
	stream                              bool
	maxTokens                           int
	prompt, promptFile                  string
	scenario                            string
	maxTurns                            int
	toolResultErrorRate                 float64
	seed                                int64
	headers                             headerList
	testID                              string
	duration, timeout, warmup           time.Duration
	requests                            int64
	rps                                 float64
	concurrency                         int
	ramp                                string
	sweep                               bool
	sweepRates                          string
	sweepFrom, sweepTo                  float64
	sweepSteps                          int
	stepDuration                        time.Duration
	sweepStopErrorPct                   float64
	maxInflight                         int
	output, out                         string
	admin, adminToken                   string
	failOnErrorPct                      float64
	progress                            time.Duration

	set map[string]bool
	cfg loadgen.RunConfig
}

func newRunFlags(o *runOptions, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("wwz-load run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.target, "target", defaultTarget, "base URL of the target (wwz or router)")
	fs.StringVar(&o.protocolS, "protocol", "openai", "dialect: openai or anthropic")
	fs.StringVar(&o.path, "path", "", "path added to --target (default by protocol)")
	fs.StringVar(&o.model, "model", defaultModel, "model field")
	fs.StringVar(&o.key, "key", "", "API key (default $WWZ_LOAD_KEY, else "+defaultKey+")")
	fs.BoolVar(&o.stream, "stream", true, "stream the response (--stream=false for non-stream)")
	fs.IntVar(&o.maxTokens, "max-tokens", 256, "max_tokens")
	fs.StringVar(&o.prompt, "prompt", defaultPrompt, "single user message")
	fs.StringVar(&o.promptFile, "prompt-file", "", "one message per non-empty line; each request draws one")
	fs.StringVar(&o.scenario, "scenario", "", "conversation mode with a scenario file")
	fs.IntVar(&o.maxTurns, "max-turns", 0, "turn ceiling per conversation (with --scenario)")
	fs.Float64Var(&o.toolResultErrorRate, "tool-result-error-rate", 0, "percent of tool results sent as errors (with --scenario)")
	fs.Int64Var(&o.seed, "seed", 0, "seed; 0 = random (recorded in the report); N ≠ 0 sends x-wwz-seed: N+i")
	fs.Var(&o.headers, "header", "extra header 'k: v' (repeatable)")
	fs.StringVar(&o.testID, "test-id", "", "shortcut for --header 'x-test-id: ID'")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "measurement duration, without --warmup")
	fs.Int64Var(&o.requests, "requests", 0, "stop after starting N measured requests (0 = no limit)")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "ceiling of a whole request")
	fs.DurationVar(&o.warmup, "warmup", 0, "initial period with the same load, not measured")
	fs.Float64Var(&o.rps, "rps", 0, "fixed rate mode (open loop), req/s")
	fs.IntVar(&o.concurrency, "concurrency", 0, "fixed concurrency mode (closed loop)")
	fs.StringVar(&o.ramp, "ramp", "", "ramp mode FROM:TO:DURATION, e.g. 100:20000:5m")
	fs.BoolVar(&o.sweep, "sweep", false, "sweep mode: one step per rate")
	fs.StringVar(&o.sweepRates, "sweep-rates", "", "explicit sweep rates, e.g. 100,500,1000")
	fs.Float64Var(&o.sweepFrom, "sweep-from", 10, "first sweep rate (geometric progression)")
	fs.Float64Var(&o.sweepTo, "sweep-to", 1000, "last sweep rate (geometric progression)")
	fs.IntVar(&o.sweepSteps, "sweep-steps", 8, "number of sweep rates (geometric progression)")
	fs.DurationVar(&o.stepDuration, "step-duration", 0, "duration of each step (default 10s ramp, 30s sweep)")
	fs.Float64Var(&o.sweepStopErrorPct, "sweep-stop-error-pct", 50, "stop the sweep after a step above P% errors (0 = off)")
	fs.IntVar(&o.maxInflight, "max-inflight", loadgen.DefaultMaxInflight, "in-flight ceiling in --rps/--ramp/--sweep")
	fs.StringVar(&o.output, "output", loadgen.FormatText, "report format: text, json or csv")
	fs.StringVar(&o.out, "out", "", "write the report to this file (default stdout)")
	fs.StringVar(&o.admin, "admin", "", "wwz control plane; GET /admin/stats goes in the report")
	fs.StringVar(&o.adminToken, "admin-token", "", "bearer of --admin (default $WWZ_ADMIN_TOKEN)")
	fs.Float64Var(&o.failOnErrorPct, "fail-on-error-pct", 0, "exit 3 if errors/requests exceeds P% (0 = off)")
	fs.DurationVar(&o.progress, "progress", 5*time.Second, "progress line interval on stderr (0 = off)")
	return fs
}

// parseRun parses and validates args. flag.ErrHelp is returned as is.
func parseRun(args []string, stderr io.Writer) (*runOptions, error) {
	o := &runOptions{set: map[string]bool{}}
	fs := newRunFlags(o, stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return nil, usageErr("unexpected argument %q", fs.Arg(0))
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *runOptions) validate() error {
	var err error
	if o.protocol, err = loadgen.ParseProtocol(o.protocolS); err != nil {
		return usageErr("--protocol: %v", err)
	}
	if o.path == "" {
		o.path = o.protocol.DefaultPath()
	}
	if !strings.HasPrefix(o.path, "/") {
		return usageErr("--path %q: must start with /", o.path)
	}
	u, perr := url.Parse(o.target)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return usageErr("--target %q: want http(s)://host[:port]", o.target)
	}
	o.target = strings.TrimRight(o.target, "/")
	if o.key == "" && !o.set["key"] {
		o.key = os.Getenv("WWZ_LOAD_KEY")
	}
	if o.key == "" && !o.set["key"] {
		o.key = defaultKey
	}
	if o.adminToken == "" && !o.set["admin-token"] {
		o.adminToken = os.Getenv("WWZ_ADMIN_TOKEN")
	}
	if o.testID != "" {
		o.headers = append(o.headers, loadgen.Header{Key: "x-test-id", Value: o.testID})
	}
	if err := o.validateInput(); err != nil {
		return err
	}
	if err := o.validateMode(); err != nil {
		return err
	}
	if err := o.validateRest(); err != nil {
		return err
	}
	o.scenarioHeader()
	return nil
}

// scenarioHeaderKey selects the scenario on the wwz (spec 011, first
// criterion of scenarios.select).
const scenarioHeaderKey = "x-wwz-scenario"

// scenarioHeader (ADR 0041): with --scenario every request carries
// x-wwz-scenario: <name of the YAML>. An explicit --header of the same name
// (any case) wins; the last one with an empty value suppresses the header.
// A scenario that cannot be read adds nothing: the run fails on it later,
// with exit 1.
func (o *runOptions) scenarioHeader() {
	if o.scenario == "" {
		return
	}
	isScn := func(h loadgen.Header) bool { return strings.EqualFold(h.Key, scenarioHeaderKey) }
	if i := lastIndexFunc(o.headers, isScn); i >= 0 {
		if o.headers[i].Value == "" {
			o.headers = slices.DeleteFunc(o.headers, isScn)
		}
		return
	}
	data, err := os.ReadFile(o.scenario) //nolint:gosec // G304: operator-supplied file
	if err != nil {
		return
	}
	if scn, _, err := loadgen.LoadScenario(data); err == nil {
		o.headers = append(o.headers, loadgen.Header{Key: scenarioHeaderKey, Value: scn.Name})
	}
}

func lastIndexFunc[T any](s []T, f func(T) bool) int {
	for i := len(s) - 1; i >= 0; i-- {
		if f(s[i]) {
			return i
		}
	}
	return -1
}

func (o *runOptions) validateInput() error {
	switch {
	case o.maxTokens < 1:
		return usageErr("--max-tokens must be ≥ 1, got %d", o.maxTokens)
	case o.set["prompt"] && o.set["prompt-file"]:
		return usageErr("--prompt and --prompt-file are exclusive")
	case o.scenario != "" && (o.set["prompt"] || o.set["prompt-file"]):
		return usageErr("--scenario is exclusive with --prompt and --prompt-file")
	case o.scenario == "" && (o.set["max-turns"] || o.set["tool-result-error-rate"]):
		return usageErr("--max-turns and --tool-result-error-rate need --scenario")
	case o.maxTurns < 0:
		return usageErr("--max-turns must be ≥ 1, got %d", o.maxTurns)
	case o.toolResultErrorRate < 0 || o.toolResultErrorRate > 100:
		return usageErr("--tool-result-error-rate must be in 0..100, got %v", o.toolResultErrorRate)
	}
	return nil
}

// validateMode picks the single mode and builds the run plan.
func (o *runOptions) validateMode() error {
	var modes []string
	for _, m := range []string{"rps", "concurrency", "ramp", "sweep"} {
		if o.set[m] && (m != "sweep" || o.sweep) {
			modes = append(modes, "--"+m)
		}
	}
	if len(modes) > 1 {
		return usageErr("only one mode allowed, got %s", strings.Join(modes, " and "))
	}
	c := &o.cfg
	c.Mode = loadgen.ModeConcurrency
	c.Concurrency = 1
	if len(modes) == 1 {
		c.Mode = loadgen.Mode(strings.TrimPrefix(modes[0], "--"))
	}
	stepFlags := o.set["step-duration"]
	sweepFlags := o.set["sweep-rates"] || o.set["sweep-from"] || o.set["sweep-to"] || o.set["sweep-steps"] || o.set["sweep-stop-error-pct"]
	if sweepFlags && c.Mode != loadgen.ModeSweep {
		return usageErr("--sweep-* flags need --sweep")
	}
	if stepFlags && c.Mode != loadgen.ModeRamp && c.Mode != loadgen.ModeSweep {
		return usageErr("--step-duration needs --ramp or --sweep")
	}
	if o.set["duration"] && (c.Mode == loadgen.ModeRamp || c.Mode == loadgen.ModeSweep) {
		return usageErr("--duration is not allowed with --%s (it defines its own duration)", c.Mode)
	}
	if o.set["max-inflight"] && c.Mode == loadgen.ModeConcurrency {
		return usageErr("--max-inflight needs --rps, --ramp or --sweep")
	}
	switch c.Mode {
	case loadgen.ModeRPS:
		if !(o.rps > 0) || math.IsInf(o.rps, 0) {
			return usageErr("--rps must be > 0, got %v", o.rps)
		}
		c.RPS = o.rps
	case loadgen.ModeConcurrency:
		if o.set["concurrency"] {
			if o.concurrency < 1 {
				return usageErr("--concurrency must be ≥ 1, got %d", o.concurrency)
			}
			c.Concurrency = o.concurrency
		}
	case loadgen.ModeRamp:
		if err := o.parseRamp(); err != nil {
			return err
		}
	case loadgen.ModeSweep:
		if err := o.parseSweep(); err != nil {
			return err
		}
	}
	return nil
}

func (o *runOptions) parseRamp() error {
	parts := strings.Split(o.ramp, ":")
	bad := usageErr("--ramp %q: want FROM:TO:DURATION with rates ≥ 0, not both 0, and duration > 0 (e.g. 100:20000:5m)", o.ramp)
	if len(parts) != 3 {
		return bad
	}
	from, err1 := strconv.ParseFloat(parts[0], 64)
	to, err2 := strconv.ParseFloat(parts[1], 64)
	d, err3 := time.ParseDuration(parts[2])
	c := &o.cfg
	c.RampFrom, c.RampTo, c.RampDuration = from, to, d
	c.StepDuration = o.stepOr(10 * time.Second)
	if err1 != nil || err2 != nil || err3 != nil || c.Validate() != nil {
		if c.StepDuration <= 0 {
			return usageErr("--step-duration must be > 0")
		}
		return bad
	}
	return nil
}

func (o *runOptions) stepOr(def time.Duration) time.Duration {
	if o.set["step-duration"] {
		return o.stepDuration
	}
	return def
}

func (o *runOptions) parseSweep() error {
	c := &o.cfg
	c.StepDuration = o.stepOr(30 * time.Second)
	if c.StepDuration <= 0 {
		return usageErr("--step-duration must be > 0")
	}
	geo := o.set["sweep-from"] || o.set["sweep-to"] || o.set["sweep-steps"]
	if o.set["sweep-rates"] {
		if geo {
			return usageErr("--sweep-rates is exclusive with --sweep-from/--sweep-to/--sweep-steps")
		}
		for _, s := range strings.Split(o.sweepRates, ",") {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil || !(v > 0) || math.IsInf(v, 0) {
				return usageErr("--sweep-rates %q: want positive rates separated by commas", o.sweepRates)
			}
			c.SweepRates = append(c.SweepRates, v)
		}
	} else {
		if !(o.sweepFrom > 0) || !(o.sweepTo > 0) || math.IsInf(o.sweepFrom, 0) || math.IsInf(o.sweepTo, 0) {
			return usageErr("--sweep-from and --sweep-to must be > 0")
		}
		if o.sweepSteps < 1 {
			return usageErr("--sweep-steps must be ≥ 1, got %d", o.sweepSteps)
		}
		c.SweepRates = loadgen.GeometricRates(o.sweepFrom, o.sweepTo, o.sweepSteps)
	}
	if o.sweepStopErrorPct < 0 || o.sweepStopErrorPct > 100 {
		return usageErr("--sweep-stop-error-pct must be in 0..100, got %v", o.sweepStopErrorPct)
	}
	c.SweepStopErrorPct = o.sweepStopErrorPct
	return nil
}

func (o *runOptions) validateRest() error {
	c := &o.cfg
	switch {
	case o.duration < 0:
		return usageErr("--duration must be ≥ 0, got %v", o.duration)
	case o.requests < 0:
		return usageErr("--requests must be ≥ 0, got %d", o.requests)
	case o.timeout <= 0:
		return usageErr("--timeout must be > 0, got %v", o.timeout)
	case o.warmup < 0:
		return usageErr("--warmup must be ≥ 0, got %v", o.warmup)
	case o.maxInflight < 1:
		return usageErr("--max-inflight must be ≥ 1, got %d", o.maxInflight)
	case o.output != loadgen.FormatText && o.output != loadgen.FormatJSON && o.output != loadgen.FormatCSV:
		return usageErr("--output %q: want text, json or csv", o.output)
	case o.failOnErrorPct < 0 || o.failOnErrorPct > 100:
		return usageErr("--fail-on-error-pct must be in 0..100, got %v", o.failOnErrorPct)
	case o.progress < 0:
		return usageErr("--progress must be ≥ 0, got %v", o.progress)
	case o.admin != "":
		if u, err := url.Parse(o.admin); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return usageErr("--admin %q: want http(s)://host[:port]", o.admin)
		}
	}
	if c.Mode == loadgen.ModeRPS || c.Mode == loadgen.ModeConcurrency {
		c.Duration = o.duration
	}
	c.Requests, c.Warmup, c.MaxInflight = o.requests, o.warmup, o.maxInflight
	return nil
}

// readPrompts loads --prompt-file (one message per non-empty line).
func readPrompts(path string) ([]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied file
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, len(b)+1)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no non-empty line", path)
	}
	return out, nil
}

func (o *runOptions) reportConfig(seed int64) loadgen.ReportConfig {
	c := &o.cfg
	rc := loadgen.ReportConfig{
		Target: o.target, Protocol: string(o.protocol), Path: o.path, Model: o.model, Stream: o.stream,
		MaxTokens: o.maxTokens, Mode: c.Mode, RPS: c.RPS, Ramp: o.ramp, SweepRates: c.SweepRates,
		StepDurationMs: ms(c.StepDuration), DurationMs: ms(c.Duration), Requests: c.Requests,
		WarmupMs: ms(c.Warmup), TimeoutMs: ms(o.timeout), Seed: seed, Scenario: o.scenario, Instances: 1,
	}
	if c.Mode == loadgen.ModeConcurrency {
		rc.Concurrency = c.Concurrency
	}
	switch c.Mode {
	case loadgen.ModeRamp:
		rc.DurationMs = ms(c.RampDuration)
	case loadgen.ModeSweep:
		rc.DurationMs = ms(c.StepDuration * time.Duration(len(c.SweepRates)))
	default:
		rc.StepDurationMs = 0
	}
	for _, h := range o.headers {
		rc.Headers = append(rc.Headers, h.Key)
	}
	return rc
}

func ms(d time.Duration) float64 { return float64(d.Milliseconds()) }

// runLoad is the run subcommand.
func runLoad(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseRun(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load: %s\n", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		return exitUsage
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "wwz-load: %v\n", err)
		return exitError
	}
	seed := o.seed
	for seed == 0 {
		seed = rand.Int64() //nolint:gosec // G404: a load seed, not a secret
	}
	cl := loadgen.NewClient(loadgen.Options{Timeout: o.timeout})
	defer cl.CloseIdleConnections()
	unit, convs, err := o.buildUnit(ctx, cl, seed)
	if err != nil {
		return fail(err)
	}
	out := stdout
	if o.out != "" {
		f, err := os.Create(o.out)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	if o.progress > 0 {
		o.cfg.ProgressEvery = o.progress
		o.cfg.Progress = func(p loadgen.Progress) {
			phase := ""
			if p.Warmup {
				phase = " (warmup)"
			}
			_, _ = fmt.Fprintf(stderr, "wwz-load: %s%s  started %d  done %d  errors %d  in flight %d  dropped %d\n",
				p.Elapsed.Truncate(time.Second), phase, p.Started, p.Completed, p.Errors, p.Inflight, p.Dropped)
		}
	}
	res, err := runLoadgen(ctx, o.cfg, unit)
	if err != nil {
		return fail(err)
	}
	rep := loadgen.NewReport(o.reportConfig(seed), res, versionTag())
	if o.admin != "" {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rep.AdminStats = loadgen.FetchAdminStats(actx, http.DefaultClient, o.admin, o.adminToken)
		cancel()
	}
	var convRep *loadgen.ConversationReport
	if convs != nil {
		convRep = convs.Report()
		rep.Conversations = convRep
	}
	if err := rep.Write(out, o.output); err != nil {
		return fail(err)
	}
	if convRep != nil && o.output == loadgen.FormatText {
		if err := convRep.WriteText(out); err != nil {
			return fail(err)
		}
	}
	if rep.Exceeds(o.failOnErrorPct) {
		_, _ = fmt.Fprintf(stderr, "wwz-load: error rate %.3f%% above --fail-on-error-pct %v\n", rep.Total.ErrorPct(), o.failOnErrorPct)
		return exitErrorRate
	}
	return exitOK
}

// buildUnit returns the unit of the run: one request, or one conversation
// with --scenario (then with its collector).
func (o *runOptions) buildUnit(ctx context.Context, cl *loadgen.Client, seed int64) (loadgen.Unit, *loadgen.Conversations, error) {
	tmpl := loadgen.Request{
		Protocol: o.protocol, URL: o.target + o.path, Key: o.key, Model: o.model, Stream: o.stream,
		MaxTokens: o.maxTokens, Headers: o.headers,
	}
	if o.scenario != "" {
		data, err := os.ReadFile(o.scenario) //nolint:gosec // G304: operator-supplied file
		if err != nil {
			return nil, nil, err
		}
		scn, turns, err := loadgen.LoadScenario(data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", o.scenario, err)
		}
		if o.maxTurns > 0 {
			turns = o.maxTurns
		}
		return newConversationUnit(cl, loadgen.ConversationPlan{
			Scenario: scn, Template: tmpl, Prompt: o.prompt, MaxTurns: turns, Seed: o.seed, DrawSeed: seed,
			ToolResultErrorRate: o.toolResultErrorRate, Interrupt: ctx.Done(),
		})
	}
	prompts := []string{o.prompt}
	if o.promptFile != "" {
		var err error
		if prompts, err = readPrompts(o.promptFile); err != nil {
			return nil, nil, err
		}
	}
	unit, err := newUnit(cl, loadgen.RequestPlan{Template: tmpl, Prompts: prompts, Seed: o.seed, DrawSeed: seed})
	return unit, nil, err
}

// versionTag is the tool_version of the report.
func versionTag() string {
	return strings.Fields(strings.TrimPrefix(versionString(), "wwz-load "))[0]
}
