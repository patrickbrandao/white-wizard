package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/dist"
)

// Seams of the worker for tests: attempts and pause of POST /v1/result.
var (
	workerSendAttempts = 3
	workerRetryDelay   = time.Second
	workerSendTimeout  = 30 * time.Second
	// workerPollInterval separates the GET /v1/status of the run (ADR 0033).
	workerPollInterval = time.Second
)

// requestSpec is the request side of the plan (dist.Plan.Request): the run
// flags that are not load, with the prompts and the scenario inlined so the
// load machines need no files.
type requestSpec struct {
	Target              string           `json:"target"`
	Protocol            loadgen.Protocol `json:"protocol"`
	Path                string           `json:"path"`
	Model               string           `json:"model"`
	Key                 string           `json:"key"`
	Stream              bool             `json:"stream"`
	MaxTokens           int              `json:"max_tokens"`
	Prompts             []string         `json:"prompts"`
	Scenario            string           `json:"scenario,omitempty"`
	ScenarioYAML        string           `json:"scenario_yaml,omitempty"`
	MaxTurns            int              `json:"max_turns,omitempty"`
	ToolResultErrorRate float64          `json:"tool_result_error_rate,omitempty"`
	Headers             []loadgen.Header `json:"headers,omitempty"`
}

// unit builds the unit of the worker's run.
func (s *requestSpec) unit(ctx context.Context, cl *loadgen.Client, p *dist.Plan) (loadgen.Unit, *loadgen.Conversations, error) {
	tmpl := loadgen.Request{
		Protocol: s.Protocol, URL: s.Target + s.Path, Key: s.Key, Model: s.Model, Stream: s.Stream,
		MaxTokens: s.MaxTokens, Headers: s.Headers,
	}
	if s.ScenarioYAML == "" {
		unit, err := newUnit(cl, loadgen.RequestPlan{Template: tmpl, Prompts: s.Prompts, Seed: p.Seed, SeedStep: p.SeedStep, DrawSeed: p.DrawSeed})
		return unit, nil, err
	}
	scn, turns, err := loadgen.LoadScenario([]byte(s.ScenarioYAML))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", s.Scenario, err)
	}
	if s.MaxTurns > 0 {
		turns = s.MaxTurns
	}
	return newConversationUnit(cl, loadgen.ConversationPlan{
		Scenario: scn, Template: tmpl, Prompt: s.Prompts[0], MaxTurns: turns, Seed: p.Seed, SeedStep: p.SeedStep, DrawSeed: p.DrawSeed,
		ToolResultErrorRate: s.ToolResultErrorRate, Interrupt: ctx.Done(),
	})
}

// options rebuilds the run options of the worker's slice, for its report.
func (s *requestSpec) options(a *dist.Assignment) *runOptions {
	o := &runOptions{
		target: s.Target, protocol: s.Protocol, path: s.Path, model: s.Model, stream: s.Stream,
		maxTokens: s.MaxTokens, scenario: s.Scenario, headers: s.Headers,
		timeout: a.Plan.Timeout, cfg: a.Plan.Load.RunConfig(),
	}
	if o.cfg.Mode == loadgen.ModeRamp {
		o.ramp = strconv.FormatFloat(o.cfg.RampFrom, 'f', -1, 64) + ":" + strconv.FormatFloat(o.cfg.RampTo, 'f', -1, 64) + ":" + o.cfg.RampDuration.String()
	}
	return o
}

// runWorker is the worker subcommand.
func runWorker(ctx context.Context, args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("wwz-load worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ctrl := fs.String("controller", "", "controller URL, e.g. http://host:9100 (required)")
	token := fs.String("token", "", "shared token (default $WWZ_LOAD_TOKEN)")
	host, _ := os.Hostname()
	id := fs.String("id", fmt.Sprintf("%s-%d", host, os.Getpid()), "worker name in the report")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if u, err := url.Parse(*ctrl); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "wwz-load worker: --controller %q: want http(s)://host:port and no arguments\n", *ctrl)
		return exitUsage
	}
	if *token == "" {
		*token = os.Getenv("WWZ_LOAD_TOKEN")
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "wwz-load worker: "+format+"\n", a...)
		return exitError
	}
	cl := &dist.Client{URL: *ctrl, Token: *token, HTTP: &http.Client{}}
	a, err := cl.Register(ctx, dist.Registration{ID: *id, Hostname: host, Version: versionTag()})
	if err != nil {
		return fail("register: %v", err)
	}
	_, _ = fmt.Fprintf(stderr, "wwz-load worker: %s is instance %d of %d, start at %s\n", *id, a.Index, a.Instances, a.StartAt.Format(time.RFC3339Nano))
	res := work(ctx, a, cl)
	var sendErr error
	for i := range workerSendAttempts {
		if i > 0 {
			time.Sleep(workerRetryDelay)
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerSendTimeout)
		sendErr = cl.SendResult(sctx, res)
		cancel()
		if sendErr == nil {
			break
		}
	}
	switch {
	case sendErr != nil:
		return fail("send result: %v", sendErr)
	case res.Error != "":
		return fail("%s", res.Error)
	}
	_, _ = fmt.Fprintf(stderr, "wwz-load worker: %d requests reported\n", res.Histograms.Run.Total.Stats.Requests)
	return exitOK
}

// watch polls GET /v1/status every workerPollInterval until ctx ends and
// calls stop when the controller is stopping; a failed poll is ignored (the
// controller may just be slow).
func watch(ctx context.Context, ctrl *dist.Client, stop context.CancelFunc) {
	t := time.NewTicker(workerPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, workerPollInterval)
		st, err := ctrl.Status(pctx)
		cancel()
		if err == nil && st.State == dist.StateStopping {
			stop()
			return
		}
	}
}

// work runs the assigned slice from start_at and returns its result; a
// failure is returned in Result.Error. A controller in the stopping state
// interrupts the run like a signal: the partial result goes out.
func work(ctx context.Context, a *dist.Assignment, ctrl *dist.Client) *dist.Result {
	ctx, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watch(ctx, ctrl, stop)
	}()
	// The watcher is joined before returning: it must not outlive the run.
	defer func() {
		stop()
		<-watched
	}()
	res := &dist.Result{Index: a.Index}
	var spec requestSpec
	if err := json.Unmarshal(a.Plan.Request, &spec); err != nil || len(spec.Prompts) == 0 {
		res.Error = "invalid plan request (controller of another build?)"
		return res
	}
	cl := loadgen.NewClient(loadgen.Options{Timeout: a.Plan.Timeout})
	defer cl.CloseIdleConnections()
	unit, convs, err := spec.unit(ctx, cl, &a.Plan)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	timer := time.NewTimer(time.Until(a.StartAt))
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		res.Error = "interrupted before start_at"
		return res
	}
	if late := time.Since(a.StartAt); late > time.Millisecond {
		res.LateStartMs = float64(late.Microseconds()) / 1000
	}
	o := spec.options(a)
	run, err := runLoadgen(ctx, o.cfg, unit)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	rc := o.reportConfig(a.Plan.DrawSeed)
	rc.Instances = a.Instances
	rep := loadgen.NewReport(rc, run, versionTag())
	res.Histograms = &dist.Histograms{Run: run}
	if convs != nil {
		res.Histograms.Conversations = convs.Stats()
		rep.Conversations = convs.Report()
	}
	b, _ := json.Marshal(rep) // a report always encodes
	res.Report = b
	return res
}
