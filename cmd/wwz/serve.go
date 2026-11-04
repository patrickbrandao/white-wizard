package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/admin"
	"github.com/patrickbrandao/white-wizard/internal/config"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
	"github.com/patrickbrandao/white-wizard/scenarios"
)

// Environment variables read by the command itself; the other WWZ_* (port,
// listen, seed, auth, store, log level...) are applied by config.ApplyEnv.
const (
	envConfig         = "WWZ_CONFIG"
	envAdminPort      = "WWZ_ADMIN_PORT"
	envValidateOnLoad = "WWZ_VALIDATE_ON_LOAD"
)

// serveFlags are the serve command-line options. Only flags explicitly set
// override the configuration (flag > env > YAML > default).
type serveFlags struct {
	config          string
	listen          string
	port            int
	adminListen     string
	adminPort       int
	seed            int64
	auth            string
	scenariosDir    string
	logLevel        string
	logFormat       string
	shutdownTimeout time.Duration
	validateOnLoad  bool
	version         bool
	set             map[string]bool
}

func parseServeFlags(args []string, stderr io.Writer) (*serveFlags, error) {
	f := &serveFlags{set: map[string]bool{}}
	fs := flag.NewFlagSet("wwz serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.config, "config", "", "YAML configuration file (env "+envConfig+"; empty = built-in defaults)")
	fs.StringVar(&f.listen, "listen", "", "data plane address, e.g. :8000 (env WWZ_LISTEN; server.listen)")
	fs.IntVar(&f.port, "port", 0, "data plane port, shorthand for -listen :PORT (env WWZ_PORT)")
	fs.StringVar(&f.adminListen, "admin-listen", "", "control plane address, e.g. :9000 (env WWZ_ADMIN_LISTEN; admin.listen)")
	fs.IntVar(&f.adminPort, "admin-port", 0, "control plane port, shorthand for -admin-listen :PORT (env "+envAdminPort+")")
	fs.Int64Var(&f.seed, "seed", 0, "random seed; 0 = random (env WWZ_SEED; server.seed)")
	fs.StringVar(&f.auth, "auth", "", "authentication mode: keys | none (env WWZ_AUTH; server.auth)")
	fs.StringVar(&f.scenariosDir, "scenarios-dir", "", "directory of scenario YAML files (env WWZ_SCENARIOS_DIR; scenarios.dir)")
	fs.StringVar(&f.logLevel, "log-level", "", "debug | info | warn | error (env WWZ_LOG_LEVEL; logging.level)")
	fs.StringVar(&f.logFormat, "log-format", "", "json | text (logging.format)")
	fs.DurationVar(&f.shutdownTimeout, "shutdown-timeout", 30*time.Second, "how long to wait for in-flight requests on SIGTERM/SIGINT")
	fs.BoolVar(&f.validateOnLoad, "validate-on-load", false, "run the wwz validate checks on boot and reload: an invalid scenario aborts the boot and refuses the whole reload (env "+envValidateOnLoad+"=1)")
	fs.BoolVar(&f.version, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	if f.set["listen"] && f.set["port"] {
		return nil, errors.New("-listen and -port are mutually exclusive")
	}
	if f.set["admin-listen"] && f.set["admin-port"] {
		return nil, errors.New("-admin-listen and -admin-port are mutually exclusive")
	}
	for _, p := range []struct {
		name string
		v    int
	}{{"port", f.port}, {"admin-port", f.adminPort}} {
		if f.set[p.name] && (p.v < 0 || p.v > 65535) {
			return nil, fmt.Errorf("-%s %d: out of range 0-65535", p.name, p.v)
		}
	}
	if f.shutdownTimeout < 0 {
		return nil, errors.New("-shutdown-timeout must not be negative")
	}
	if !f.set["config"] {
		f.config = os.Getenv(envConfig)
	}
	if !f.set["validate-on-load"] {
		v := strings.TrimSpace(os.Getenv(envValidateOnLoad))
		f.validateOnLoad = v == "1" || strings.EqualFold(v, "true")
	}
	return f, nil
}

// envPort validates a port number read from the environment.
func envPort(name string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(os.Getenv(name)), ":")
	if v == "" {
		return "", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("%s=%q: not a port number", name, os.Getenv(name))
	}
	return v, nil
}

// override applies what the configuration file must not win against: the
// environment variables config.ApplyEnv does not know and the flags set on
// the command line. It runs on the initial load and on every reload.
func (f *serveFlags) override(adminPortEnv string) func(*config.Config) {
	return func(c *config.Config) {
		if adminPortEnv != "" {
			c.Admin.Listen = ":" + adminPortEnv
		}
		if f.set["listen"] {
			c.Server.Listen = f.listen
		}
		if f.set["port"] {
			c.Server.Listen = ":" + strconv.Itoa(f.port)
		}
		if f.set["admin-listen"] {
			c.Admin.Listen = f.adminListen
		}
		if f.set["admin-port"] {
			c.Admin.Listen = ":" + strconv.Itoa(f.adminPort)
		}
		if f.set["seed"] {
			c.Server.Seed = f.seed
		}
		if f.set["auth"] {
			c.Server.Auth = f.auth
		}
		if f.set["scenarios-dir"] {
			c.Scenarios.Dir = f.scenariosDir
		}
		if f.set["log-level"] {
			c.Logging.Level = f.logLevel
		}
		if f.set["log-format"] {
			c.Logging.Format = f.logFormat
		}
		// Overrides can leave fields the defaults depend on (e.g. -auth keys
		// over a YAML without keys); filling them again is idempotent.
		config.ApplyDefaults(c)
	}
}

// restartCheck refuses reloads that change what only a restart can apply:
// the listen addresses and the store backend. Everything else the server
// can serve (server.CheckConfig) is accepted.
func restartCheck(initial *config.Config) func(*config.Config) error {
	return func(c *config.Config) error {
		var changed []string
		if c.Server.Listen != initial.Server.Listen {
			changed = append(changed, "server.listen")
		}
		if c.Admin.Listen != initial.Admin.Listen {
			changed = append(changed, "admin.listen")
		}
		if c.Store.Kind != initial.Store.Kind {
			changed = append(changed, "store.kind")
		}
		if len(changed) > 0 {
			return fmt.Errorf("config: %s cannot change without a restart", strings.Join(changed, ", "))
		}
		return server.CheckConfig(c)
	}
}

// reloadCheck is the Check of the control plane: restartCheck and, with
// -validate-on-load, the scenario checks of wwz validate, so an invalid
// scenario refuses the whole reload (I23).
func (f *serveFlags) reloadCheck(initial *config.Config) func(*config.Config) error {
	check := restartCheck(initial)
	if !f.validateOnLoad {
		return check
	}
	return func(c *config.Config) error {
		if err := check(c); err != nil {
			return err
		}
		return checkScenarios(c)
	}
}

// newLogger builds the process logger from logging.level/format.
func newLogger(c *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Logging.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(c.Logging.Format, "text") {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// valkeyDB is the logical database of store.kind valkey (tests use 15).
var valkeyDB = 0

// newStore opens the limits store selected by store.kind. The Valkey store
// is wrapped in the store.on_down circuit, so an unreachable Valkey never
// stops the boot: circuit reports every open/close transition.
func newStore(ctx context.Context, c *config.Config, instance string, log *slog.Logger, circuit func(open bool)) (limits.Store, error) {
	switch c.Store.Kind {
	case "", "memory":
		return limits.NewMemory(), nil
	case "valkey":
		vo := limits.ValkeyOptions{DB: valkeyDB, Instance: instance, Version: version, Addr: c.Server.Listen, Logger: log}
		return limits.OpenValkey(ctx, c.Store, vo, limits.FallbackOptions{Logger: log, OnChange: circuit}), nil
	default:
		return nil, fmt.Errorf("store.kind %q: want memory or valkey", c.Store.Kind)
	}
}

// watchPolicies applies the policies of the store before the planes start
// serving and then on every change (ADR 0025). Both store kinds keep
// policies (limits.Memory only for this instance).
func watchPolicies(ctx context.Context, store limits.Store, pol *limits.PolicyApplier, log *slog.Logger) {
	ps := store.(limits.PolicySource) //nolint:forcetypeassert // newStore returns *Memory or *Fallback
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	p, err := ps.Policies(pctx)
	cancel()
	if err != nil && p.Version == 0 {
		log.Warn("policies unavailable at boot; serving the YAML until the store answers", "err", err)
	}
	pol.Offer(p, err)
	ps.WatchPolicies(pol.Offer)
}

// Seams for tests: dependencies whose errors Validate and CheckConfig already
// rule out before the planes start.
var (
	builtinFiles = scenarios.Files
	openStore    = newStore
	newAdmin     = admin.New
	newServer    = server.New
	listenAdmin  = net.Listen
)

// builtinScenarios parses the scenarios embedded in the binary.
func builtinScenarios() ([]*scenario.Scenario, error) {
	var out []*scenario.Scenario
	for _, f := range builtinFiles() {
		s, err := scenario.ParseYAML(f.Data)
		if err != nil {
			return nil, fmt.Errorf("built-in scenario %s: %w", f.Name, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// addScenario registers s in reg, logging at info when it replaces one with
// the same name.
func addScenario(reg *scenario.Registry, s *scenario.Scenario, log *slog.Logger) {
	if reg.Get(s.Name) != nil {
		log.Info("scenario overridden", "name", s.Name)
	}
	reg.Add(s)
}

// loadScenarios adds the built-in scenarios (unless scenarios.builtin is
// false), scenarios.dir and scenarios.files to reg, in that order. Invalid
// files are logged and skipped.
func loadScenarios(reg *scenario.Registry, c *config.Config, log *slog.Logger) {
	if c.Scenarios.UseBuiltin() {
		built, err := builtinScenarios()
		if err != nil {
			log.Error("built-in scenarios", "err", err)
		}
		for _, s := range built {
			addScenario(reg, s, log)
		}
	} else {
		// A reload that turned the built-ins off drops them, except the
		// ones a later source replaces below.
		built, _ := builtinScenarios()
		for _, s := range built {
			reg.Remove(s.Name)
		}
	}
	if c.Scenarios.Dir != "" {
		before := map[string]bool{}
		for _, n := range reg.Names() {
			before[n] = true
		}
		loaded, errs := reg.LoadDir(c.Scenarios.Dir)
		for _, err := range errs {
			log.Warn("scenario skipped", "dir", c.Scenarios.Dir, "err", err)
		}
		for _, n := range loaded {
			if before[n] {
				log.Info("scenario overridden", "name", n)
			}
		}
		if len(loaded) > 0 {
			log.Info("scenarios loaded", "dir", c.Scenarios.Dir, "count", len(loaded))
		}
	}
	for _, path := range c.Scenarios.Files {
		data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the operator configuration
		if err == nil {
			var s *scenario.Scenario
			if s, err = scenario.ParseYAML(data); err == nil {
				addScenario(reg, s, log)
				continue
			}
		}
		log.Warn("scenario skipped", "file", path, "err", err)
	}
}

// addrs are the bound addresses reported once both planes listen.
type addrs struct {
	Data  string
	Admin string
}

// runServe starts both planes and blocks until ctx ends (graceful shutdown)
// or a listener fails. ready, when not nil, receives the bound addresses.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(addrs)) int {
	f, err := parseServeFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz serve:", err)
		return exitUsage
	}
	if f.version {
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	}
	adminPortEnv, err := envPort(envAdminPort)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz serve:", err)
		return exitUsage
	}
	override := f.override(adminPortEnv)

	cfg, err := config.Load(f.config)
	if err == nil {
		override(cfg)
		err = cfg.Validate()
	}
	if err == nil {
		err = server.CheckConfig(cfg)
	}
	if err == nil && f.validateOnLoad {
		err = checkScenarios(cfg)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz serve:", err)
		return exitError
	}
	log := newLogger(cfg, stderr)
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}

	pod := os.Getenv("POD_NAME")
	if pod == "" {
		pod = os.Getenv("POD_IP")
	}
	mreg := metrics.New(cfg.Metrics.MaxSeries)
	circuit := mreg.NewGauge("wwz_store_circuit_open", "1 while the Valkey store is unreachable and store.on_down applies")
	store, err := openStore(ctx, cfg, pod, log, func(open bool) {
		v := int64(0)
		if open {
			v = 1
		}
		circuit.Set(v)
	})
	if err != nil {
		log.Error("store", "err", err)
		return exitError
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Warn("store close", "err", err)
		}
	}()
	reg := scenario.NewRegistry()
	loadScenarios(reg, cfg, log)
	m := engine.NewMetrics(mreg, cfg.Metrics.Buckets)
	if cfg.Metrics.VLLM {
		m.MirrorVLLM() // metrics.vllm_mirror is read at boot, like max_series
	}
	eng := engine.New(cfg, store, reg, m, log)
	if pod != "" {
		eng.SetPodName(pod)
	}

	// Cluster policies (ADR 0025) go on top of every configuration, through
	// the same validation and atomic swap of POST /admin/config.
	polVersion := mreg.NewGauge("wwz_policies_version", "Version of cfg:policies applied (0 = none)")
	polErrors := mreg.NewCounter("wwz_policies_errors_total", "Versions of cfg:policies refused by validation")
	pol := &limits.PolicyApplier{Log: log, OnVersion: func(v int64) { polVersion.Set(v) }, OnError: func() { polErrors.Inc() }}
	eng.EnablePromotion(pol) // account tiers of the policies + promote_when (ADR 0051)

	var load func() (*config.Config, error)
	if f.config != "" {
		path := f.config
		load = func() (*config.Config, error) { return config.Load(path) }
	}
	adm, err := newAdmin(admin.Options{
		Engine:     eng,
		ConfigPath: f.config,
		Load:       load,
		Override:   func(c *config.Config) { override(c); pol.Override(c) },
		Check:      f.reloadCheck(cfg),
		Logger:     log,
	})
	if err != nil {
		log.Error("control plane", "err", err)
		return exitError
	}
	adm.OnReload(func(c *config.Config) { loadScenarios(reg, c, log) })
	adm.OnReload(pol.Committed)
	pol.Current, pol.Apply = adm.Config, adm.Apply
	watchPolicies(ctx, store, pol, log)

	srv, err := newServer(eng, server.Options{Logger: log})
	if err != nil {
		log.Error("data plane", "err", err)
		return exitError
	}
	mreg.NewGaugeFunc("wwz_connections_open", "TCP connections open on the data plane", srv.OpenConnections)
	dataLns, err := srv.Listen("")
	if err != nil {
		log.Error("data plane listen", "addr", cfg.Server.Listen, "err", err)
		return exitError
	}
	adminAddr, restricted := adm.ListenAddr()
	adminLn, err := listenAdmin("tcp", adminAddr)
	if err != nil {
		for _, ln := range dataLns {
			_ = ln.Close()
		}
		log.Error("control plane listen", "addr", adminAddr, "err", err)
		return exitError
	}
	if restricted {
		log.Warn("no admin token (WWZ_ADMIN_TOKEN or admin.token): control plane bound to loopback only", "addr", adminLn.Addr().String())
	}

	hs := &http.Server{
		Handler:           adm.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, len(dataLns)+1)
	for _, ln := range dataLns {
		go func(ln net.Listener) { errc <- srv.Serve(ln) }(ln)
	}
	go func() { errc <- hs.Serve(adminLn) }()

	sigCtx, stopSig := context.WithCancel(ctx)
	defer stopSig()
	adm.WatchSIGHUP(sigCtx)

	a := addrs{Data: dataLns[0].Addr().String(), Admin: adminLn.Addr().String()}
	log.Info("wwz started", "version", version, "data", a.Data, "admin", a.Admin, "listeners", len(dataLns), "store", cfg.Store.Kind, "config", f.config)
	if ready != nil {
		ready(a)
	}

	code := exitOK
	select {
	case <-ctx.Done():
		log.Info("shutting down", "timeout", f.shutdownTimeout)
	case err := <-errc:
		log.Error("listener failed; shutting down", "err", err)
		code = exitError
	}
	stopSig()
	adm.SetReady(false)

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("data plane shutdown", "err", err)
	}
	if err := hs.Shutdown(sctx); err != nil {
		log.Warn("control plane shutdown", "err", err)
	}
	waitPromotions(sctx, eng.Promoter().Wait, log)
	log.Info("wwz stopped")
	return code
}

// waitPromotions waits for the background promotions of the Promoter (task
// 191) to end, up to the shutdown deadline of ctx; the store closes after.
func waitPromotions(ctx context.Context, wait func(), log *slog.Logger) {
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("promotions still running at shutdown deadline")
	}
}
