package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrPromOff means no Prometheus is configured (WWZ_PANEL_PROMETHEUS_URL
// empty).
var ErrPromOff = errors.New("source: painel sem Prometheus")

// Prom is the client of the Prometheus HTTP API (/api/v1/query and
// /api/v1/query_range).
type Prom struct {
	base string
	hc   *http.Client
}

// NewProm builds the client for baseURL; "" returns nil (no Prometheus).
// hc nil = a client with a 5 s timeout.
func NewProm(baseURL string, hc *http.Client) *Prom {
	if baseURL == "" {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	return &Prom{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// Sample is one series of an instant vector.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// Point is one value of a range series.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Series is one series of a range query.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

type promResp struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (p *Prom) get(ctx context.Context, path string, q url.Values) (promResp, error) {
	var out promResp
	if p == nil {
		return out, ErrPromOff
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return out, fmt.Errorf("source: prometheus: %w", err)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("source: prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminBody))
	if err != nil {
		return out, fmt.Errorf("source: prometheus: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("source: prometheus: HTTP %d: resposta inválida: %w", resp.StatusCode, err)
	}
	if out.Status != "success" {
		return out, fmt.Errorf("source: prometheus: HTTP %d: %s: %s", resp.StatusCode, out.ErrorType, out.Error)
	}
	return out, nil
}

// promValue decodes [ts, "value"].
func promValue(raw [2]json.RawMessage) (time.Time, float64, error) {
	var ts float64
	var s string
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return time.Time{}, 0, err
	}
	if err := json.Unmarshal(raw[1], &s); err != nil {
		return time.Time{}, 0, err
	}
	v, err := strconv.ParseFloat(s, 64)
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)), v, err
}

// Query runs an instant query at at (zero = now of the server). A scalar
// result becomes one sample without labels.
func (p *Prom) Query(ctx context.Context, query string, at time.Time) ([]Sample, error) {
	q := url.Values{"query": {query}}
	if !at.IsZero() {
		q.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	}
	r, err := p.get(ctx, "/api/v1/query", q)
	if err != nil {
		return nil, err
	}
	bad := func(err error) ([]Sample, error) {
		return nil, fmt.Errorf("source: prometheus: resultado inválido: %w", err)
	}
	out := []Sample{}
	switch r.Data.ResultType {
	case "vector":
		var vec []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &vec); err != nil {
			return bad(err)
		}
		for _, s := range vec {
			_, v, err := promValue(s.Value)
			if err != nil {
				return bad(err)
			}
			out = append(out, Sample{Labels: s.Metric, Value: v})
		}
	case "scalar":
		var sc [2]json.RawMessage
		if err := json.Unmarshal(r.Data.Result, &sc); err != nil {
			return bad(err)
		}
		_, v, err := promValue(sc)
		if err != nil {
			return bad(err)
		}
		out = append(out, Sample{Labels: map[string]string{}, Value: v})
	default:
		return bad(fmt.Errorf("tipo %q", r.Data.ResultType))
	}
	return out, nil
}

// QueryRange runs a range query.
func (p *Prom) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	sec := func(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }
	q := url.Values{"query": {query}, "start": {sec(start)}, "end": {sec(end)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	r, err := p.get(ctx, "/api/v1/query_range", q)
	if err != nil {
		return nil, err
	}
	bad := func(err error) ([]Series, error) {
		return nil, fmt.Errorf("source: prometheus: resultado inválido: %w", err)
	}
	if r.Data.ResultType != "matrix" {
		return bad(fmt.Errorf("tipo %q", r.Data.ResultType))
	}
	var mat []struct {
		Metric map[string]string    `json:"metric"`
		Values [][2]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(r.Data.Result, &mat); err != nil {
		return bad(err)
	}
	out := make([]Series, 0, len(mat))
	for _, m := range mat {
		s := Series{Labels: m.Metric, Points: make([]Point, 0, len(m.Values))}
		for _, raw := range m.Values {
			t, v, err := promValue(raw)
			if err != nil {
				return bad(err)
			}
			s.Points = append(s.Points, Point{T: t, V: v})
		}
		out = append(out, s)
	}
	return out, nil
}

// Ping runs the query "1" (state of /health/ready).
func (p *Prom) Ping(ctx context.Context) error {
	_, err := p.Query(ctx, "1", time.Time{})
	return err
}

// ---- PromQL of spec 016 ----

// Panel names a ready-made query of spec 016.
type Panel string

// Ready-made queries ("Consultas PromQL").
const (
	PanelReqRate        Panel = "req_rate"         // sum(rate(wwz_requests_total[$w]))
	PanelTokenRate      Panel = "token_rate"       // by kind
	PanelStreamsOpen    Panel = "streams_open"     // sum(wwz_requests_in_flight)
	PanelTTFTP50        Panel = "ttft_p50"         // seconds
	PanelTTFTP99        Panel = "ttft_p99"         // seconds
	PanelE2EP50         Panel = "e2e_p50"          // seconds
	PanelE2EP99         Panel = "e2e_p99"          // seconds
	PanelErrorsByType   Panel = "errors_by_type"   // injected, by type
	PanelErrorsByStatus Panel = "errors_by_status" // 4xx/5xx, by status
	PanelRateLimited    Panel = "rate_limited"     // by limit
)

// Panels lists every ready-made query, in screen order.
var Panels = []Panel{
	PanelReqRate, PanelTokenRate, PanelStreamsOpen, PanelTTFTP50, PanelTTFTP99,
	PanelE2EP50, PanelE2EP99, PanelErrorsByType, PanelErrorsByStatus, PanelRateLimited,
}

// Windows are the accepted windows ($w).
var Windows = map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour}

// Filter selects the window and the driver/model matchers.
type Filter struct {
	// Window is 1m, 5m or 1h ("" = 1m).
	Window string
	Driver string
	Model  string
}

func (f Filter) window() (string, time.Duration, error) {
	w := f.Window
	if w == "" {
		w = "1m"
	}
	d, ok := Windows[w]
	if !ok {
		return "", 0, fmt.Errorf("source: janela %q (use 1m, 5m ou 1h)", f.Window)
	}
	return w, d, nil
}

// matchers renders {driver="..", model="..", extra...}.
func (f Filter) matchers(extra ...string) string {
	var m []string
	if f.Driver != "" {
		m = append(m, "driver="+strconv.Quote(f.Driver))
	}
	if f.Model != "" {
		m = append(m, "model="+strconv.Quote(f.Model))
	}
	m = append(m, extra...)
	if len(m) == 0 {
		return ""
	}
	return "{" + strings.Join(m, ", ") + "}"
}

// PromQL renders the query of panel with filter f.
func PromQL(panel Panel, f Filter) (string, error) {
	w, _, err := f.window()
	if err != nil {
		return "", err
	}
	rate := func(metric string, extra ...string) string {
		return "rate(" + metric + f.matchers(extra...) + "[" + w + "])"
	}
	quantile := func(q, metric string) string {
		return "histogram_quantile(" + q + ", sum by (le) (" + rate(metric) + "))"
	}
	switch panel {
	case PanelReqRate:
		return "sum(" + rate("wwz_requests_total") + ")", nil
	case PanelTokenRate:
		return "sum by (kind) (" + rate("wwz_tokens_total") + ")", nil
	case PanelStreamsOpen:
		return "sum(wwz_requests_in_flight" + f.matchers() + ")", nil
	case PanelTTFTP50:
		return quantile("0.5", "wwz_ttft_seconds_bucket"), nil
	case PanelTTFTP99:
		return quantile("0.99", "wwz_ttft_seconds_bucket"), nil
	case PanelE2EP50:
		return quantile("0.5", "wwz_e2e_seconds_bucket"), nil
	case PanelE2EP99:
		return quantile("0.99", "wwz_e2e_seconds_bucket"), nil
	case PanelErrorsByType:
		return "sum by (kind) (" + rate("wwz_errors_injected_total") + ")", nil // engine label: kind
	case PanelErrorsByStatus:
		return "sum by (status) (" + rate("wwz_requests_total", `status=~"4..|5.."`) + ")", nil
	case PanelRateLimited:
		return "sum by (limit) (" + rate("wwz_rate_limited_total") + ")", nil
	}
	return "", fmt.Errorf("source: consulta %q desconhecida", panel)
}

// groupLabel is the label that splits each grouped panel ("" = one value).
func groupLabel(p Panel) string {
	switch p {
	case PanelTokenRate:
		return "kind"
	case PanelErrorsByType:
		return "kind"
	case PanelErrorsByStatus:
		return "status"
	case PanelRateLimited:
		return "limit"
	}
	return ""
}

// ---- overview with /admin/stats fallback ----

// Overview is the "Visão geral" data. Pointer and map fields are nil when
// the source has no data ("—" on screen, never zero).
type Overview struct {
	// Source is "prometheus" or "admin" (fallback).
	Source string `json:"source"`
	// Window actually used ("" with the admin fallback, whose rates are
	// over the interval since the previous call).
	Window         string             `json:"window,omitempty"`
	ReqRate        *float64           `json:"req_rate,omitempty"`
	TokenRate      map[string]float64 `json:"token_rate,omitempty"`
	StreamsOpen    *float64           `json:"streams_open,omitempty"`
	TTFTP50        *float64           `json:"ttft_p50_s,omitempty"`
	TTFTP99        *float64           `json:"ttft_p99_s,omitempty"`
	E2EP50         *float64           `json:"e2e_p50_s,omitempty"`
	E2EP99         *float64           `json:"e2e_p99_s,omitempty"`
	ErrorsByType   map[string]float64 `json:"errors_by_type,omitempty"`
	ErrorsByStatus map[string]float64 `json:"errors_by_status,omitempty"`
	RateLimited    map[string]float64 `json:"rate_limited,omitempty"`
	// PromError is why Prometheus was skipped, when the fallback ran
	// because it failed.
	PromError string `json:"prom_error,omitempty"`
}

// Metrics serves the aggregated metrics: Prometheus when configured and
// answering, otherwise /admin/stats (spec 016).
type Metrics struct {
	prom  *Prom
	admin *Admin
	now   func() time.Time

	mu     sync.Mutex
	prev   *Stats
	prevAt time.Time
}

// NewMetrics combines prom (nil = off) and admin.
func NewMetrics(prom *Prom, admin *Admin) *Metrics {
	return &Metrics{prom: prom, admin: admin, now: time.Now}
}

// Overview returns the overview for f, from Prometheus or, when it is off or
// fails, from /admin/stats. Only an invalid window or the failure of both
// sources is an error.
func (m *Metrics) Overview(ctx context.Context, f Filter) (Overview, error) {
	w, _, err := f.window()
	if err != nil {
		return Overview{}, err
	}
	var promErr error
	if m.prom != nil {
		ov, err := m.promOverview(ctx, f)
		if err == nil {
			ov.Window = w
			return ov, nil
		}
		promErr = err
	}
	ov, err := m.adminOverview(ctx, f)
	if err != nil {
		if promErr != nil {
			return Overview{}, fmt.Errorf("%w (prometheus: %v)", err, promErr)
		}
		return Overview{}, err
	}
	if promErr != nil {
		ov.PromError = promErr.Error()
	}
	return ov, nil
}

func (m *Metrics) promOverview(ctx context.Context, f Filter) (Overview, error) {
	ov := Overview{Source: "prometheus"}
	for _, p := range Panels {
		q, _ := PromQL(p, f)
		samples, err := m.prom.Query(ctx, q, time.Time{})
		if err != nil {
			return Overview{}, err
		}
		if lbl := groupLabel(p); lbl != "" {
			mp := map[string]float64{}
			for _, s := range samples {
				if !math.IsNaN(s.Value) {
					mp[s.Labels[lbl]] = s.Value
				}
			}
			if len(mp) == 0 {
				mp = nil
			}
			switch p {
			case PanelTokenRate:
				ov.TokenRate = mp
			case PanelErrorsByType:
				ov.ErrorsByType = mp
			case PanelErrorsByStatus:
				ov.ErrorsByStatus = mp
			default:
				ov.RateLimited = mp
			}
			continue
		}
		var v *float64
		if len(samples) > 0 && !math.IsNaN(samples[0].Value) {
			x := samples[0].Value
			v = &x
		}
		switch p {
		case PanelReqRate:
			ov.ReqRate = v
		case PanelStreamsOpen:
			ov.StreamsOpen = v
		case PanelTTFTP50:
			ov.TTFTP50 = v
		case PanelTTFTP99:
			ov.TTFTP99 = v
		case PanelE2EP50:
			ov.E2EP50 = v
		default:
			ov.E2EP99 = v
		}
	}
	return ov, nil
}

// Range returns the series of panel for f over the last window, step
// window/60 (Prometheus only).
func (m *Metrics) Range(ctx context.Context, panel Panel, f Filter) ([]Series, error) {
	if m.prom == nil {
		return nil, ErrPromOff
	}
	q, err := PromQL(panel, f)
	if err != nil {
		return nil, err
	}
	_, d, _ := f.window()
	end := m.now()
	return m.prom.QueryRange(ctx, q, end.Add(-d), end, d/60)
}

func sel(f Filter, driver, model string) bool {
	return (f.Driver == "" || f.Driver == driver) && (f.Model == "" || f.Model == model)
}

// adminOverview builds the overview from /admin/stats. Rates are the delta
// against the previous call (nil on the first call or after a counter
// reset). Latencies: with one matching driver/model row, its percentiles;
// with several, p50 weighted by requests and p99 = the largest (upper
// bound). Errors by type and status and rate limits are not split by
// driver/model in /admin/stats and ignore the filter.
func (m *Metrics) adminOverview(ctx context.Context, f Filter) (Overview, error) {
	st, err := m.admin.Stats(ctx)
	if err != nil {
		return Overview{}, err
	}
	now := m.now()
	m.mu.Lock()
	prev, prevAt := m.prev, m.prevAt
	m.prev, m.prevAt = &st, now
	m.mu.Unlock()

	ov := Overview{Source: "admin"}
	streams := float64(st.Totals.StreamsOpen)
	ov.StreamsOpen = &streams

	var rows []ModelStats
	for _, r := range st.ByModel {
		if sel(f, r.Driver, r.Model) {
			rows = append(rows, r)
		}
	}
	ov.TTFTP50, ov.TTFTP99 = latency(rows, func(r ModelStats) (*float64, *float64) { return r.TTFTP50MS, r.TTFTP99MS })
	ov.E2EP50, ov.E2EP99 = latency(rows, func(r ModelStats) (*float64, *float64) { return r.E2EP50MS, r.E2EP99MS })

	elapsed := now.Sub(prevAt).Seconds()
	if prev == nil || elapsed <= 0 || st.Started != prev.Started || !st.ResetAt.Equal(prev.ResetAt) {
		return ov, nil
	}
	var prevRows []ModelStats
	for _, r := range prev.ByModel {
		if sel(f, r.Driver, r.Model) {
			prevRows = append(prevRows, r)
		}
	}
	sumReq := func(rs []ModelStats) (n int64) {
		for _, r := range rs {
			n += r.Requests
		}
		return n
	}
	sumTok := func(rs []ModelStats) map[string]int64 {
		t := map[string]int64{}
		for _, r := range rs {
			for k, v := range r.Tokens {
				t[k] += v
			}
		}
		return t
	}
	if d := sumReq(rows) - sumReq(prevRows); d >= 0 {
		r := float64(d) / elapsed
		ov.ReqRate = &r
	}
	ov.TokenRate = rates(sumTok(rows), sumTok(prevRows), elapsed)
	ov.ErrorsByType = rates(st.Totals.ErrorsByKind, prev.Totals.ErrorsByKind, elapsed)
	errStatus := map[string]int64{}
	prevStatus := map[string]int64{}
	for k, v := range st.Totals.ByStatus {
		if strings.HasPrefix(k, "4") || strings.HasPrefix(k, "5") {
			errStatus[k] = v
			prevStatus[k] = prev.Totals.ByStatus[k]
		}
	}
	ov.ErrorsByStatus = rates(errStatus, prevStatus, elapsed)
	ov.RateLimited = rates(st.Totals.RateLimitedByLimit, prev.Totals.RateLimitedByLimit, elapsed)
	return ov, nil
}

// rates is (cur − prev)/elapsed per key; nil when empty or a counter went
// back.
func rates(cur, prev map[string]int64, elapsed float64) map[string]float64 {
	if len(cur) == 0 {
		return nil
	}
	out := make(map[string]float64, len(cur))
	for k, v := range cur {
		d := v - prev[k]
		if d < 0 {
			return nil
		}
		out[k] = float64(d) / elapsed
	}
	return out
}

// latency combines the p50/p99 (ms) of rows into seconds.
func latency(rows []ModelStats, get func(ModelStats) (*float64, *float64)) (p50, p99 *float64) {
	var wsum, w float64
	var hi *float64
	for _, r := range rows {
		a, b := get(r)
		if a != nil && r.Requests > 0 {
			wsum += *a * float64(r.Requests)
			w += float64(r.Requests)
		}
		if b != nil && (hi == nil || *b > *hi) {
			x := *b
			hi = &x
		}
	}
	if w > 0 {
		x := wsum / w / 1000
		p50 = &x
	}
	if hi != nil {
		x := *hi / 1000
		p99 = &x
	}
	return p50, p99
}
