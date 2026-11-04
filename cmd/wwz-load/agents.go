package main

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/agents"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// agentsRefused are the run flags agents refuses with exit 2 (spec 015,
// "wwz-load agents").
var agentsRefused = []string{
	"rps", "concurrency", "ramp", "sweep", "prompt", "prompt-file", "scenario", "warmup", "requests", "instances",
}

// agentsOptions are the parsed and validated flags of agents.
type agentsOptions struct {
	target, protocolS, path, model, key string
	protocol                            loadgen.Protocol
	stream                              bool
	maxTokens                           int
	headers                             headerList
	testID                              string
	timeout                             time.Duration
	seed                                int64
	output, out                         string
	admin, adminToken                   string
	progress                            time.Duration
	failOnErrorPct                      float64

	agents, turns, maxToolRounds, sessions int
	profileS                               string
	profile                                agents.Profile
	duration, thinkTime, spawnInterval     time.Duration
	toolProb                               float64
	toolsURL, toolsConfig, toolsToken      string
	retries                                int
	backoff, backoffMax, retryAfterMax     time.Duration

	set map[string]bool
}

func newAgentsFlags(o *agentsOptions, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("wwz-load agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.target, "target", defaultTarget, "base URL of the target (wwz or router)")
	fs.StringVar(&o.protocolS, "protocol", "openai", "dialect: openai or anthropic")
	fs.StringVar(&o.path, "path", "", "path added to --target (default by protocol)")
	fs.StringVar(&o.model, "model", defaultModel, "model field")
	fs.StringVar(&o.key, "key", "", "API key (default $WWZ_LOAD_KEY, else "+defaultKey+")")
	fs.BoolVar(&o.stream, "stream", true, "stream the response (--stream=false for non-stream)")
	fs.IntVar(&o.maxTokens, "max-tokens", 256, "max_tokens")
	fs.Var(&o.headers, "header", "extra header 'k: v' (repeatable)")
	fs.StringVar(&o.testID, "test-id", "", "shortcut for --header 'x-test-id: ID'")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "ceiling of a whole request")
	fs.Int64Var(&o.seed, "seed", 0, "seed; 0 = random (recorded in the report); N ≠ 0 sends x-wwz-seed: N+g")
	fs.StringVar(&o.output, "output", loadgen.FormatText, "report format: text, json or csv")
	fs.StringVar(&o.out, "out", "", "write the report to this file (default stdout)")
	fs.StringVar(&o.admin, "admin", "", "wwz control plane; GET /admin/stats goes in the report")
	fs.StringVar(&o.adminToken, "admin-token", "", "bearer of --admin (default $WWZ_ADMIN_TOKEN)")
	fs.DurationVar(&o.progress, "progress", 5*time.Second, "progress line interval on stderr (0 = off)")
	fs.Float64Var(&o.failOnErrorPct, "fail-on-error-pct", 0, "exit 3 if failed/started sessions exceeds P% (0 = off)")

	fs.IntVar(&o.agents, "agents", 10, "concurrent agents")
	fs.StringVar(&o.profileS, "profile", string(agents.Coder), "profile: coder, pesquisa, chat or mixed")
	fs.IntVar(&o.turns, "turns", 5, "user turns per session")
	fs.IntVar(&o.maxToolRounds, "max-tool-rounds", 8, "requests per turn while tool calls come")
	fs.IntVar(&o.sessions, "sessions", 1, "sessions per agent when --duration is 0")
	fs.DurationVar(&o.duration, "duration", 0, "0 = --sessions per agent; D > 0 = open sessions until D")
	fs.DurationVar(&o.thinkTime, "think-time", 0, "pause between turns")
	fs.DurationVar(&o.spawnInterval, "spawn-interval", 0, "interval between agent starts")
	fs.Float64Var(&o.toolProb, "tool-prob", 0, "send x-wwz-tool-prob: P (percent of responses with tool calls)")
	fs.StringVar(&o.toolsURL, "tools-url", "", "run the tools on a wwz-tools serve (bearer $WWZ_TOOLS_TOKEN)")
	fs.StringVar(&o.toolsConfig, "tools-config", "", "tools.yaml of the in-process executor")
	fs.IntVar(&o.retries, "retries", 2, "retries per request on transient errors (0 = off)")
	fs.DurationVar(&o.backoff, "backoff", 500*time.Millisecond, "backoff base (full jitter)")
	fs.DurationVar(&o.backoffMax, "backoff-max", 30*time.Second, "backoff ceiling")
	fs.DurationVar(&o.retryAfterMax, "retry-after-max", 60*time.Second, "largest retry-after accepted")

	for _, name := range agentsRefused {
		if name == "sweep" {
			fs.Bool(name, false, "not allowed with agents")
			continue
		}
		fs.String(name, "", "not allowed with agents")
	}
	return fs
}

// parseAgents parses and validates args. flag.ErrHelp is returned as is.
func parseAgents(args []string, stderr io.Writer) (*agentsOptions, error) {
	o := &agentsOptions{set: map[string]bool{}}
	fs := newAgentsFlags(o, stderr)
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
	for _, name := range agentsRefused {
		if o.set[name] {
			return nil, usageErr("--%s is not allowed with agents", name)
		}
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (o *agentsOptions) validate() error {
	var err error
	if o.protocol, err = loadgen.ParseProtocol(o.protocolS); err != nil {
		return usageErr("--protocol: %v", err)
	}
	if o.profile, err = agents.ParseProfile(o.profileS); err != nil {
		return usageErr("--profile: %v", err)
	}
	if o.path == "" {
		o.path = o.protocol.DefaultPath()
	}
	switch {
	case !strings.HasPrefix(o.path, "/"):
		return usageErr("--path %q: must start with /", o.path)
	case !validURL(o.target):
		return usageErr("--target %q: want http(s)://host[:port]", o.target)
	case o.admin != "" && !validURL(o.admin):
		return usageErr("--admin %q: want http(s)://host[:port]", o.admin)
	case o.toolsURL != "" && !validURL(o.toolsURL):
		return usageErr("--tools-url %q: want http(s)://host[:port]", o.toolsURL)
	case o.toolsURL != "" && o.toolsConfig != "":
		return usageErr("--tools-url and --tools-config are exclusive")
	case o.maxTokens < 1:
		return usageErr("--max-tokens must be ≥ 1, got %d", o.maxTokens)
	case o.timeout <= 0:
		return usageErr("--timeout must be > 0, got %v", o.timeout)
	case o.output != loadgen.FormatText && o.output != loadgen.FormatJSON && o.output != loadgen.FormatCSV:
		return usageErr("--output %q: want text, json or csv", o.output)
	case o.progress < 0:
		return usageErr("--progress must be ≥ 0, got %v", o.progress)
	case o.failOnErrorPct < 0 || o.failOnErrorPct > 100 || math.IsNaN(o.failOnErrorPct):
		return usageErr("--fail-on-error-pct must be in 0..100, got %v", o.failOnErrorPct)
	case o.set["tool-prob"] && !(o.toolProb >= 0 && o.toolProb <= 100):
		return usageErr("--tool-prob must be in 0..100, got %v", o.toolProb)
	case o.set["sessions"] && o.duration > 0:
		return usageErr("--sessions applies only with --duration 0")
	}
	if err := o.validateCounts(); err != nil {
		return err
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
	o.toolsToken = os.Getenv("WWZ_TOOLS_TOKEN")
	if o.testID != "" {
		o.headers = append(o.headers, loadgen.Header{Key: "x-test-id", Value: o.testID})
	}
	if o.set["tool-prob"] {
		o.headers = append(o.headers, loadgen.Header{Key: "x-wwz-tool-prob", Value: strconv.FormatFloat(o.toolProb, 'f', -1, 64)})
	}
	return nil
}

func (o *agentsOptions) validateCounts() error {
	for _, c := range []struct {
		name string
		v    int
	}{{"agents", o.agents}, {"turns", o.turns}, {"max-tool-rounds", o.maxToolRounds}, {"sessions", o.sessions}} {
		if c.v < 1 {
			return usageErr("--%s must be ≥ 1, got %d", c.name, c.v)
		}
	}
	if o.retries < 0 {
		return usageErr("--retries must be ≥ 0, got %d", o.retries)
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"duration", o.duration},
		{"think-time", o.thinkTime},
		{"spawn-interval", o.spawnInterval},
		{"backoff", o.backoff},
		{"backoff-max", o.backoffMax},
		{"retry-after-max", o.retryAfterMax},
	} {
		if d.v < 0 {
			return usageErr("--%s must be ≥ 0, got %v", d.name, d.v)
		}
	}
	return nil
}

// toolRunner builds the in-process executor or the wwz-tools client.
func (o *agentsOptions) toolRunner() (agents.ToolRunner, string, error) {
	if o.toolsURL != "" {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = o.agents
		return agents.Remote{URL: o.toolsURL, Token: o.toolsToken, Client: &http.Client{Transport: tr}}, o.toolsURL, nil
	}
	cfg := tools.DefaultConfig()
	if o.toolsConfig != "" {
		data, err := os.ReadFile(o.toolsConfig) //nolint:gosec // G304/G703: operator-supplied file
		if err != nil {
			return nil, "", err
		}
		if cfg, err = tools.Parse(data); err != nil {
			return nil, "", fmt.Errorf("%s: %w", o.toolsConfig, err)
		}
	}
	ex, _ := tools.New(cfg) // the defaults and Parse are already validated
	return agents.InProcess{Ex: ex}, agents.ToolsInProcess, nil
}

func (o *agentsOptions) reportConfig(seed int64, toolsDesc string) agents.ReportConfig {
	rc := agents.ReportConfig{
		Target: o.target, Protocol: string(o.protocol), Path: o.path, Model: o.model, Stream: o.stream,
		Agents: o.agents, Profile: string(o.profile), Turns: o.turns, MaxToolRounds: o.maxToolRounds,
		Sessions: o.sessions, DurationMs: ms(o.duration), Retries: o.retries, Seed: seed, Tools: toolsDesc,
	}
	for _, h := range o.headers {
		rc.Headers = append(rc.Headers, h.Key)
	}
	return rc
}

// runAgentsFn is a seam for tests.
var runAgentsFn = agents.Run

// runAgents is the agents subcommand.
func runAgents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseAgents(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load agents: %s\n", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		return exitUsage
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "wwz-load agents: %v\n", err)
		return exitError
	}
	runner, toolsDesc, err := o.toolRunner()
	if err != nil {
		return fail(err)
	}
	seed := o.seed
	for seed == 0 {
		seed = rand.Int64() //nolint:gosec // G404: a load seed, not a secret
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
	cl := loadgen.NewClient(loadgen.Options{Timeout: o.timeout})
	defer cl.CloseIdleConnections()
	cfg := agents.Config{
		Template: loadgen.Request{
			Protocol: o.protocol, URL: o.target + o.path, Key: o.key, Model: o.model, Stream: o.stream,
			MaxTokens: o.maxTokens, Headers: o.headers,
		},
		Agents: o.agents, Profile: o.profile, Turns: o.turns, MaxToolRounds: o.maxToolRounds, Sessions: o.sessions,
		Duration: o.duration, ThinkTime: o.thinkTime, SpawnInterval: o.spawnInterval, Seed: o.seed, DrawSeed: seed,
		Retries: o.retries, Backoff: o.backoff, BackoffMax: o.backoffMax, RetryAfterMax: o.retryAfterMax, Tools: runner,
	}
	if o.progress > 0 {
		cfg.ProgressEvery = o.progress
		cfg.Progress = func(p agents.Progress) {
			_, _ = fmt.Fprintf(stderr, "wwz-load agents: %s  sessions %d started  %d ended  %d failed  requests %d  errors %d  retries %d\n",
				p.Elapsed.Truncate(time.Second), p.SessionsStarted, p.SessionsEnded, p.SessionsFailed, p.Requests, p.Errors, p.Retries)
		}
	}
	res, err := runAgentsFn(ctx, cl, cfg)
	if err != nil {
		return fail(err)
	}
	rep := agents.NewReport(o.reportConfig(seed, toolsDesc), res, versionTag())
	if o.admin != "" {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rep.AdminStats = loadgen.FetchAdminStats(actx, http.DefaultClient, o.admin, o.adminToken)
		cancel()
	}
	if err := rep.Write(out, o.output); err != nil {
		return fail(err)
	}
	if rep.Exceeds(o.failOnErrorPct) {
		_, _ = fmt.Fprintf(stderr, "wwz-load agents: failed sessions %.3f%% above --fail-on-error-pct %v\n", rep.FailedPct(), o.failOnErrorPct)
		return exitErrorRate
	}
	return exitOK
}
