package drift

import (
	"context"
	_ "embed" // wwz.yaml
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini" // embedded wwz drivers
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// WWZConfig is the embedded wwz configuration: the 6 drivers mounted with
// the probe model ids (Provider.WWZPrefix and Provider.DefaultModel).
//
//go:embed wwz.yaml
var WWZConfig []byte

// InProcessURL is the report value of wwz.url for the embedded server.
const InProcessURL = "in-process"

// EmbeddedWWZ is a wwz data plane running in this process.
type EmbeddedWWZ struct {
	// URL is the base of the data plane (http://127.0.0.1:<port>).
	URL string
	srv *server.Server
}

// StartWWZ serves cfg (nil = WWZConfig) on 127.0.0.1:0 until Close.
func StartWWZ(cfg []byte) (*EmbeddedWWZ, error) { return startWith(cfg, nil) }

// startWWZ starts the embedded wwz of a check, serving the --model
// overrides too; replaceable in tests.
var startWWZ = func(models map[string]string) (*EmbeddedWWZ, error) { return startWith(nil, models) }

// startWith is StartWWZ serving also models (provider → id).
func startWith(cfg []byte, models map[string]string) (*EmbeddedWWZ, error) {
	if cfg == nil {
		cfg = WWZConfig
	}
	c, err := config.Parse(cfg)
	if err != nil {
		return nil, err
	}
	addModels(c, models)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets)
	eng := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), m, log)
	srv, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		return nil, err
	}
	ln, err := listenTCP("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() { _ = srv.Serve(ln) }()
	return &EmbeddedWWZ{URL: "http://" + ln.Addr().String(), srv: srv}, nil
}

// addModels puts each model id in the catalog of its provider's driver, so
// that --model changes the model on both sides: an id already configured
// joins the catalog; a new id copies the model in that catalog (same
// protocols, latency, errors and generation) and is listed by that driver
// only. A provider without a model in c is left alone.
func addModels(c *config.Config, models map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(models)) {
		id, p := models[name], providers[name].DriverName()
		if m := c.FindModel(id); m != nil {
			m.AddToCatalog(p)
			continue
		}
		i := slices.IndexFunc(c.Models, func(m config.ModelConfig) bool { return m.ModelSupports(p) })
		if i < 0 {
			continue
		}
		m := c.Models[i]
		m.ID, m.DisplayName, m.Aliases = id, id, nil
		m.Drivers = []string{p}
		c.Models = append(c.Models, m)
	}
}

// listenTCP is net.Listen, replaceable in tests.
var listenTCP = net.Listen

// Close stops the server, waiting up to 5 s for running requests.
func (w *EmbeddedWWZ) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.srv.Shutdown(ctx)
}
