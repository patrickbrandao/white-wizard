package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Source states of /health/ready.
const (
	StateOK   = "ok"
	StateDown = "down"
	StateOff  = "off"
)

// Sources is the state of each data source.
type Sources struct {
	Valkey     string `json:"valkey"`
	Admin      string `json:"admin"`
	Prometheus string `json:"prometheus"`
}

// Ready is the body of GET /health/ready.
type Ready struct {
	Status  string  `json:"status"` // ready | starting
	Sources Sources `json:"sources"`
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// CheckSources pings Valkey, the wwz admin and Prometheus concurrently.
func (a *API) CheckSources(ctx context.Context) Sources {
	ping := func(off bool, f func(context.Context) error) func() string {
		return func() string {
			if off {
				return StateOff
			}
			c, cancel := a.ctxTimeout(ctx)
			defer cancel()
			if f(c) != nil {
				return StateDown
			}
			return StateOK
		}
	}
	checks := []func() string{
		ping(a.vk == nil, func(c context.Context) error { return a.vk.Ping(c) }),
		ping(false, a.admin.Ping),
		ping(a.prom == nil, func(c context.Context) error { return a.prom.Ping(c) }),
	}
	out := make([]string, len(checks))
	var wg sync.WaitGroup
	for i, f := range checks {
		wg.Go(func() { out[i] = f() })
	}
	wg.Wait()
	return Sources{Valkey: out[0], Admin: out[1], Prometheus: out[2]}
}

// ready is 200 when the SQLite answers (sources only appear in the body)
// and 503 "starting" otherwise.
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	body := Ready{Status: "ready", Sources: a.CheckSources(r.Context())}
	status := http.StatusOK
	if _, err := a.st.UserVersion(r.Context()); err != nil {
		body.Status, status = "starting", http.StatusServiceUnavailable
	}
	writeJSON(w, status, body)
}

// metricsHandler writes the panel metrics in the Prometheus text format.
func (a *API) metricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder
	metric := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	if a.consumer != nil {
		if lag := a.consumer.Lag(); lag >= 0 {
			metric("wwz_panel_stream_lag", "gauge", "Lag of the panel consumer group on ev:stream (XINFO GROUPS).")
			fmt.Fprintf(&b, "wwz_panel_stream_lag %d\n", lag)
		}
	}
	var ingested int64
	if a.consumer != nil {
		ingested += a.consumer.Ingested()
	}
	if a.poller != nil {
		ingested += a.poller.Ingested()
	}
	metric("wwz_panel_events_ingested_total", "counter", "Events stored in the panel SQLite.")
	fmt.Fprintf(&b, "wwz_panel_events_ingested_total %d\n", ingested)
	var last float64
	if v, ok, err := a.st.Settings.Get(ctx, store.SettingLastBackupAt); err == nil && ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			last = float64(n) / 1000
		}
	}
	metric("wwz_panel_last_backup_timestamp_seconds", "gauge", "Unix time of the last SQLite snapshot (0 = never).")
	fmt.Fprintf(&b, "wwz_panel_last_backup_timestamp_seconds %s\n", strconv.FormatFloat(last, 'f', -1, 64))
	src := a.CheckSources(ctx)
	metric("wwz_panel_source_up", "gauge", "1 when the data source answers; sources turned off are omitted.")
	for _, s := range []struct{ name, state string }{
		{"valkey", src.Valkey}, {"admin", src.Admin}, {"prometheus", src.Prometheus},
	} {
		if s.state == StateOff {
			continue
		}
		up := 0
		if s.state == StateOK {
			up = 1
		}
		fmt.Fprintf(&b, "wwz_panel_source_up{source=%q} %d\n", s.name, up)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
