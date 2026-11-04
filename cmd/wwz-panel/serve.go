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
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/api"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/backup"
	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
	"github.com/patrickbrandao/white-wizard/internal/panel/web"
)

// Timers of serve (variables so tests can shorten them).
var (
	purgeEvery      = 24 * time.Hour
	shutdownTimeout = 10 * time.Second
)

// panel holds everything serve builds.
type panel struct {
	cfg      *config.Config
	log      *slog.Logger
	st       *store.Store
	auth     *auth.Service
	admin    *source.Admin
	prom     *source.Prom
	vk       *source.Valkey // nil without Valkey
	consumer *source.Consumer
	poller   *source.RequestPoller
	api      *api.API
	web      *web.Web
}

// newLogger is slog JSON at the configured level (validated by config).
func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

// setup opens the SQLite, runs the bootstrap (ADR 0005) before syncing the
// panel.yaml users, and builds auth, the data sources and the API.
func setup(ctx context.Context, cfg *config.Config, log *slog.Logger) (*panel, error) {
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	p := &panel{cfg: cfg, log: log, st: st}
	_, err = st.Bootstrap(ctx, hasher, cfg.Admin.User, cfg.Admin.Password, log)
	if err == nil {
		p.auth, err = auth.New(ctx, auth.Options{Store: st, Config: cfg, Logger: log, Hasher: authHasher})
	}
	if err == nil {
		err = p.auth.SyncUsers(ctx, cfg.Users)
	}
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	p.admin = source.NewAdmin(cfg.WWZ.AdminURL, cfg.WWZ.AdminToken, nil)
	p.prom = source.NewProm(cfg.Prometheus.URL, nil)
	if cfg.Valkey.Enabled() {
		p.vk, _ = source.NewValkey(cfg.Valkey, source.ValkeyOptions{})
		p.consumer = source.NewConsumer(p.vk, st.Events, source.ConsumerOptions{Logger: log})
	} else {
		p.poller = source.NewRequestPoller(p.admin, st.Events, source.PollerOptions{Logger: log})
	}
	p.api, _ = api.New(api.Options{
		Store: st, Auth: p.auth, Admin: p.admin, Valkey: p.vk, Prom: p.prom,
		Consumer: p.consumer, Poller: p.poller, Logger: log,
	})
	p.web = web.New(web.Options{API: p.api, Auth: p.auth, OIDC: cfg.OIDC.Enabled(), Logger: log})
	return p, nil
}

// routes is the HTTP handler: the contract routes of the API (/api/v1,
// /auth/oidc, /health, /metrics) and, on the same mux, the HTML screens and
// /static/ (task 050), all with the security headers (CSP without inline
// script, X-Frame-Options: DENY).
func (p *panel) routes() http.Handler {
	mux := http.NewServeMux()
	p.api.Register(mux)
	p.web.Register(mux)
	return web.SecurityHeaders(mux)
}

// background starts the ev:stream consumer (or the /admin/requests poller
// without Valkey), the purge (at boot and every purgeEvery) and, with
// backup.dir, the daily snapshot at backup.at UTC; it returns when every
// goroutine has stopped after ctx ends.
func (p *panel) background(ctx context.Context) (wait func()) {
	var wg sync.WaitGroup
	if p.consumer != nil {
		wg.Go(func() { _ = p.consumer.Run(ctx) })
	}
	if p.poller != nil {
		wg.Go(func() { _ = p.poller.Run(ctx) })
	}
	if p.cfg.Backup.Dir != "" {
		wg.Go(func() { p.backupLoop(ctx) })
	}
	wg.Go(func() {
		t := time.NewTicker(purgeEvery)
		defer t.Stop()
		for {
			p.purge(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	return wg.Wait
}

// purge applies retention_days to events and drops old sessions.
func (p *panel) purge(ctx context.Context) {
	res, err := p.st.Purge(ctx, p.cfg.RetentionDays)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("painel: purga", "err", err)
		}
		return
	}
	p.log.Info("painel: purga", "events", res.Events, "sessions", res.Sessions)
}

// backupLoop takes one snapshot a day at backup.at (UTC, validated by
// config) until ctx ends.
func (p *panel) backupLoop(ctx context.Context) {
	for {
		now := backupNow()
		next, _ := backup.Next(now, p.cfg.Backup.At)
		t := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		p.backup(ctx)
	}
}

// backup takes one snapshot into backup.dir and logs the result.
func (p *panel) backup(ctx context.Context) {
	path, err := snapshot(ctx, p.st, p.cfg.Backup.Dir, p.cfg.Backup.Keep)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("painel: backup", "path", path, "err", err)
		}
		return
	}
	p.log.Info("painel: backup", "path", path)
}

func (p *panel) close() {
	if p.vk != nil {
		p.vk.Close()
	}
	_ = p.st.Close()
}

// runServe is `wwz-panel serve`. ready, when not nil, receives the listener
// once the panel accepts connections (tests).
func runServe(ctx context.Context, args []string, _, stderr io.Writer, ready func(net.Listener)) int {
	fs := flag.NewFlagSet("wwz-panel serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "panel.yaml (sobrepõe WWZ_PANEL_CONFIG)")
	listen := fs.String("listen", "", "endereço HTTP (sobrepõe WWZ_PANEL_LISTEN)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "wwz-panel serve: argumento inesperado %q\n", fs.Arg(0))
		return exitUsage
	}
	cfg, err := config.Load(config.Flags{Config: *cfgPath, Listen: *listen}, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz-panel serve:", err)
		return exitError
	}
	log := newLogger(stderr, cfg.LogLevel)
	// Exclusive lock on the database while serve runs (spec 016): restore
	// refuses while it is held. Taken before setup so a restore cannot swap
	// the file under the store being opened.
	unlock, err := backup.Lock(backup.LockPath(cfg.DB))
	if err != nil {
		log.Error("painel: banco em uso", "db", cfg.DB, "err", err)
		return exitError
	}
	defer func() { _ = unlock() }()
	p, err := setup(ctx, cfg, log)
	if err != nil {
		log.Error("painel: não subiu", "err", err)
		return exitError
	}
	defer p.close()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("painel: listen", "addr", cfg.Listen, "err", err)
		return exitError
	}
	srv := &http.Server{Handler: p.routes(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	bgCtx, stopBG := context.WithCancel(ctx)
	wait := p.background(bgCtx)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("painel: no ar", "addr", ln.Addr().String(), "valkey", cfg.Valkey.Enabled(),
		"prometheus", cfg.Prometheus.URL != "", "admin", cfg.WWZ.AdminURL)
	if ready != nil {
		ready(ln)
	}
	code := exitOK
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("painel: encerramento", "err", err)
		}
		cancel()
	case err := <-served:
		log.Error("painel: servidor HTTP parou", "err", err)
		code = exitError
	}
	stopBG()
	wait()
	log.Info("painel: encerrado")
	return code
}
