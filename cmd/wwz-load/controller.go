package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/dist"
)

// exitMissing: a worker did not report in the distributed mode (spec 015).
const exitMissing = 4

// Seams of the controller for tests.
var (
	// controllerGrace is added to the planned duration + timeout before a
	// silent worker is declared missing.
	controllerGrace = 30 * time.Second
	// controllerStartDelay separates the last registration from start_at.
	controllerStartDelay = dist.DefaultStartDelay
	// controllerListening receives the address the controller listens on.
	controllerListening = func(string) {}
)

// controllerOptions are the run options plus the flags of the controller.
type controllerOptions struct {
	*runOptions
	listen     string
	instances  int
	token      string
	regTimeout time.Duration
}

func parseController(args []string, stderr io.Writer) (*controllerOptions, error) {
	o := &controllerOptions{runOptions: &runOptions{set: map[string]bool{}}}
	fs := newRunFlags(o.runOptions, stderr)
	fs.Init("wwz-load controller", flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", ":9100", "address of the controller")
	fs.IntVar(&o.instances, "instances", 1, "number of workers to wait for")
	fs.StringVar(&o.token, "token", "", "shared token of the workers (default $WWZ_LOAD_TOKEN)")
	fs.DurationVar(&o.regTimeout, "register-timeout", 5*time.Minute, "time to wait for the N workers")
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
	switch {
	case o.instances < 1:
		return nil, usageErr("--instances must be ≥ 1, got %d", o.instances)
	case o.regTimeout <= 0:
		return nil, usageErr("--register-timeout must be > 0, got %v", o.regTimeout)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.token == "" {
		o.token = os.Getenv("WWZ_LOAD_TOKEN")
	}
	return o, nil
}

// plan builds the undivided plan: prompts and scenario are read here and
// travel inline.
func (o *controllerOptions) plan(drawSeed int64) (dist.Plan, error) {
	spec := requestSpec{
		Target: o.target, Protocol: o.protocol, Path: o.path, Model: o.model, Key: o.key, Stream: o.stream,
		MaxTokens: o.maxTokens, Prompts: []string{o.prompt}, Scenario: o.scenario, MaxTurns: o.maxTurns,
		ToolResultErrorRate: o.toolResultErrorRate, Headers: o.headers,
	}
	if o.promptFile != "" {
		var err error
		if spec.Prompts, err = readPrompts(o.promptFile); err != nil {
			return dist.Plan{}, err
		}
	}
	if o.scenario != "" {
		data, err := os.ReadFile(o.scenario) //nolint:gosec // G304: operator-supplied file
		if err != nil {
			return dist.Plan{}, err
		}
		if _, _, err := loadgen.LoadScenario(data); err != nil {
			return dist.Plan{}, fmt.Errorf("%s: %w", o.scenario, err)
		}
		spec.ScenarioYAML = string(data)
	}
	req, _ := json.Marshal(spec) // strings, numbers and headers always encode
	return dist.Plan{Load: dist.LoadOf(o.cfg), Seed: o.seed, DrawSeed: drawSeed, Timeout: o.timeout, Request: req}, nil
}

// runController is the controller subcommand.
func runController(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseController(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: %s\n", trimUsage(err))
		return exitUsage
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: %v\n", err)
		return exitError
	}
	seed := o.seed
	for seed == 0 {
		seed = rand.Int64() //nolint:gosec // G404: a load seed, not a secret
	}
	plan, err := o.plan(seed)
	if err != nil {
		return fail(err)
	}
	if err := plan.Check(o.instances); err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: %v\n", err)
		return exitUsage
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
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fail(err)
	}
	coord := dist.NewCoordinator(dist.Config{
		Instances: o.instances, Token: o.token, Version: versionTag(), Plan: plan, StartDelay: controllerStartDelay,
	})
	srv := &http.Server{Handler: coord.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Shutdown, not Close: the 204 of the last result may still be
		// in flight; Close would cut it and fail that worker.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()
	controllerListening(ln.Addr().String())
	_, _ = fmt.Fprintf(stderr, "wwz-load controller: listening on %s, waiting for %d workers\n", ln.Addr(), o.instances)

	regTimer := time.NewTimer(o.regTimeout)
	defer regTimer.Stop()
	select {
	case <-coord.Ready():
	case <-regTimer.C:
		return fail(fmt.Errorf("fewer than %d workers registered in %v", o.instances, o.regTimeout))
	case <-ctx.Done():
		return fail(errors.New("interrupted before the start"))
	}
	var deadline <-chan time.Time
	if exp := plan.Load.Expected(); exp > 0 {
		t := time.NewTimer(time.Until(coord.StartAt().Add(exp + o.timeout + controllerGrace)))
		defer t.Stop()
		deadline = t.C
	}
	interrupted := false
	select {
	case <-coord.Done():
	case <-deadline:
	case <-ctx.Done():
		// Interrupted after the registration: the workers see stopping on
		// GET /v1/status and send their partial results (ADR 0033).
		interrupted = true
		coord.Stop()
		t := time.NewTimer(o.timeout + controllerGrace)
		select {
		case <-coord.Done():
		case <-t.C:
		}
		t.Stop()
	}
	res, conv, inst, errs := coord.Collect()
	for _, e := range errs {
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: worker %s\n", e)
	}
	res.Interrupted = res.Interrupted || interrupted
	return o.emit(ctx, out, stderr, seed, res, conv, inst)
}

// emit writes the aggregated report and returns the exit code: 4 with a
// missing worker, else 3 above --fail-on-error-pct, else 0.
func (o *controllerOptions) emit(ctx context.Context, out, stderr io.Writer, seed int64, res *loadgen.RunResult, conv *loadgen.ConvStats, inst *dist.Instances) int {
	rc := o.reportConfig(seed)
	rc.Instances = o.instances
	rep := loadgen.NewReport(rc, res, versionTag())
	rep.Instances = inst
	if o.admin != "" {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rep.AdminStats = loadgen.FetchAdminStats(actx, http.DefaultClient, o.admin, o.adminToken)
		cancel()
	}
	var convRep *loadgen.ConversationReport
	if o.scenario != "" {
		if conv == nil {
			conv = loadgen.NewConvStats()
		}
		convRep = conv.Report()
		rep.Conversations = convRep
	}
	err := rep.Write(out, o.output)
	if err == nil && o.output == loadgen.FormatText {
		if convRep != nil {
			err = convRep.WriteText(out)
		}
		if err == nil {
			err = writeInstancesText(out, inst)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: %v\n", err)
		return exitError
	}
	switch {
	case len(inst.Missing) > 0:
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: missing workers: %s\n", strings.Join(inst.Missing, ", "))
		return exitMissing
	case rep.Exceeds(o.failOnErrorPct):
		_, _ = fmt.Fprintf(stderr, "wwz-load controller: error rate %.3f%% above --fail-on-error-pct %v\n", rep.Total.ErrorPct(), o.failOnErrorPct)
		return exitErrorRate
	}
	return exitOK
}

func writeInstancesText(w io.Writer, inst *dist.Instances) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\ninstances: %d expected  %d reported", inst.Expected, inst.Reported)
	if len(inst.Missing) > 0 {
		fmt.Fprintf(&b, "  missing: %s", strings.Join(inst.Missing, ", "))
	}
	b.WriteString("\n")
	for _, wr := range inst.Workers {
		fmt.Fprintf(&b, "  %-24s %-24s requests %d  late start %s ms\n", wr.ID, wr.Hostname, wr.Requests, fnum(wr.LateStartMs))
	}
	_, err := w.Write([]byte(b.String()))
	return err
}

// fnum formats a float without trailing zeros.
func fnum(v float64) string { return fmt.Sprint(v) }

// trimUsage strips the errUsage prefix of a parse error.
func trimUsage(err error) string {
	return strings.TrimPrefix(err.Error(), errUsage.Error()+": ")
}
