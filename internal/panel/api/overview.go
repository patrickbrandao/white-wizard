package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/source"
)

// Overview sources.
const (
	SourcePrometheus = "prometheus"
	SourceAdminStats = "admin_stats"
	SourceValkey     = "valkey"
	SourceAdmin      = "admin"
	SourceEvents     = "events"
)

// Quantiles are p50/p99 in milliseconds (nil = no data).
type Quantiles struct {
	P50MS *float64 `json:"p50_ms"`
	P99MS *float64 `json:"p99_ms"`
}

// Point is [t (Unix seconds), value|null].
type Point [2]*float64

// NewPoint builds a Point.
func NewPoint(t time.Time, v *float64) Point {
	s := float64(t.Unix())
	return Point{&s, v}
}

// Instance is a wwz instance (schema Instance).
type Instance struct {
	ID       string     `json:"id"`
	Version  string     `json:"version,omitempty"`
	Addr     string     `json:"addr,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Beat     *time.Time `json:"beat,omitempty"`
	RPS      *float64   `json:"rps"`
	Conns    *int64     `json:"conns"`
	InFlight *int64     `json:"inflight"`
	Store    string     `json:"store,omitempty"`
}

// Overview is the body of GET /api/v1/overview.
type Overview struct {
	Window          string             `json:"window"`
	Source          string             `json:"source"`
	ReqPerS         *float64           `json:"req_per_s"`
	TokensPerS      map[string]float64 `json:"tokens_per_s"`
	StreamsOpen     *int64             `json:"streams_open"`
	TTFT            Quantiles          `json:"ttft"`
	E2E             Quantiles          `json:"e2e"`
	ErrorsPerS      map[string]float64 `json:"errors_per_s"`
	StatusPerS      map[string]float64 `json:"status_per_s"`
	RateLimitedPerS map[string]float64 `json:"rate_limited_per_s"`
	Instances       []Instance         `json:"instances"`
	Series          map[string][]Point `json:"series,omitempty"`
}

// OverviewQuery selects the overview.
type OverviewQuery struct {
	Window string // 1m | 5m | 1h ("" = 5m)
	Driver string
	Model  string
	Series bool
}

// windowParam reads window with its default and allowed values.
func windowParam(q url.Values, def string, allowed ...string) (string, error) {
	w := q.Get("window")
	if w == "" {
		return def, nil
	}
	if !slices.Contains(allowed, w) {
		return "", invalid("window inválida", "window: use "+strings.Join(allowed, ", "))
	}
	return w, nil
}

func boolParam(q url.Values, name string) (bool, error) {
	v := q.Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, invalid(name+" inválido", name+": use true ou false")
	}
	return b, nil
}

func ms(v *float64) *float64 {
	if v == nil {
		return nil
	}
	x := *v * 1000
	return &x
}

func orEmpty(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}

// Overview builds the overview: Prometheus when configured and answering,
// /admin/stats otherwise; instances from Valkey (or the admin instance).
// A failing instance source leaves the list empty instead of failing.
func (a *API) Overview(ctx context.Context, q OverviewQuery) (Overview, error) {
	if q.Window == "" {
		q.Window = "5m"
	}
	f := source.Filter{Window: q.Window, Driver: q.Driver, Model: q.Model}
	ov, err := a.metrics.Overview(ctx, f)
	if err != nil {
		return Overview{}, err
	}
	out := Overview{
		Window: q.Window, Source: SourcePrometheus, ReqPerS: ov.ReqRate, TokensPerS: orEmpty(ov.TokenRate),
		TTFT:            Quantiles{ms(ov.TTFTP50), ms(ov.TTFTP99)},
		E2E:             Quantiles{ms(ov.E2EP50), ms(ov.E2EP99)},
		ErrorsPerS:      orEmpty(ov.ErrorsByType),
		StatusPerS:      orEmpty(ov.ErrorsByStatus),
		RateLimitedPerS: orEmpty(ov.RateLimited),
	}
	if ov.Source != SourcePrometheus {
		out.Source = SourceAdminStats
	}
	if ov.StreamsOpen != nil {
		n := int64(*ov.StreamsOpen)
		out.StreamsOpen = &n
	}
	insts, _, err := a.Instances(ctx)
	if err != nil {
		a.log.Warn("painel: instâncias da visão geral", "err", err)
		insts = []Instance{}
	}
	out.Instances = insts
	if q.Series {
		out.Series = a.overviewSeries(ctx, f)
	}
	return out, nil
}

// overviewSeries runs the range queries (Prometheus only); a failing or
// absent Prometheus yields no series.
func (a *API) overviewSeries(ctx context.Context, f source.Filter) map[string][]Point {
	out := map[string][]Point{}
	for _, s := range []struct {
		key   string
		panel source.Panel
		scale float64
	}{
		{"req_per_s", source.PanelReqRate, 1},
		{"tokens_per_s", source.PanelTokenRate, 1},
		{"ttft_p99_ms", source.PanelTTFTP99, 1000},
		{"e2e_p99_ms", source.PanelE2EP99, 1000},
		{"errors_per_s", source.PanelErrorsByType, 1},
	} {
		series, err := a.metrics.Range(ctx, s.panel, f)
		if err != nil {
			return map[string][]Point{}
		}
		out[s.key] = sumSeries(series, s.scale)
	}
	return out
}

// sumSeries adds the series point by point (grouped panels) and scales.
func sumSeries(series []source.Series, scale float64) []Point {
	sums := map[int64]float64{}
	for _, s := range series {
		for _, p := range s.Points {
			sums[p.T.Unix()] += p.V * scale
		}
	}
	keys := make([]int64, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		v := sums[k]
		out = append(out, NewPoint(time.Unix(k, 0), &v))
	}
	return out
}

// Instances lists the live instances: Valkey heartbeats or, without
// Valkey, the instance of WWZ_PANEL_WWZ_ADMIN_URL from /admin/stats.
// Returns the source (valkey | admin_stats).
func (a *API) Instances(ctx context.Context) ([]Instance, string, error) {
	if a.vk == nil {
		st, err := a.admin.Stats(ctx)
		if err != nil {
			return nil, "", err
		}
		u, _ := url.Parse(a.admin.BaseURL())
		inst := Instance{ID: st.Pod, Addr: a.admin.BaseURL(), InFlight: &st.Totals.InFlight, Store: storeState(st.Store)}
		if inst.ID == "" {
			inst.ID = u.Host
		}
		if !st.Started.IsZero() {
			t := st.Started.UTC()
			inst.Started = &t
		}
		return []Instance{inst}, SourceAdminStats, nil
	}
	list, err := a.vk.Instances(ctx)
	if err != nil {
		return nil, "", err
	}
	out := make([]Instance, len(list))
	for i, x := range list {
		out[i] = Instance{
			ID: x.ID, Version: x.Version, Addr: x.Addr, RPS: &x.RPS, Conns: &x.Conns, InFlight: &x.InFlight,
			Store: storeState(x.Store),
		}
		if !x.Started.IsZero() {
			t := x.Started.UTC()
			out[i].Started = &t
		}
		if !x.Beat.IsZero() {
			t := x.Beat.UTC()
			out[i].Beat = &t
		}
	}
	return out, SourceValkey, nil
}

// storeState maps the store state to the contract enum (ok, open, local,
// memory); "valkey" (kind of /admin/stats) is ok; unknown values are
// omitted.
func storeState(s string) string {
	switch s {
	case "ok", "open", "local", "memory":
		return s
	case "valkey":
		return "ok"
	}
	return ""
}

func (a *API) getOverview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	win, err := windowParam(q, "5m", "1m", "5m", "1h")
	if err != nil {
		a.fail(w, err, "")
		return
	}
	series, err := boolParam(q, "series")
	if err != nil {
		a.fail(w, err, "")
		return
	}
	ov, err := a.Overview(r.Context(), OverviewQuery{Window: win, Driver: q.Get("driver"), Model: q.Get("model"), Series: series})
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (a *API) getInstances(w http.ResponseWriter, r *http.Request) {
	list, src, err := a.Instances(r.Context())
	if err != nil {
		what := "valkey"
		if a.vk == nil {
			what = "admin"
		}
		a.fail(w, err, what)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": list, "source": src})
}
