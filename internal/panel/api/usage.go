package api

import (
	"context"
	"encoding/csv"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Usage groupings of the contract.
var usageGroups = []string{store.GroupDriver, store.GroupModel, store.GroupKey, store.GroupAccount, store.GroupIP}

// usageWindows are the windows of UsageWindow.
var usageWindows = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

// maxScan bounds the events read in Go (scenario steps without
// Prometheus), newest first.
var maxScan = 200_000

// UsageRow is one row of GET /api/v1/usage (nil = no data in the source).
type UsageRow struct {
	Value           string             `json:"value"`
	Source          string             `json:"source"`
	Requests        float64            `json:"requests"`
	PromptTokens    *float64           `json:"prompt_tokens"`
	OutputTokens    *float64           `json:"output_tokens"`
	ReasoningTokens *float64           `json:"reasoning_tokens"`
	CachedTokens    *float64           `json:"cached_tokens"`
	CacheHitRate    *float64           `json:"cache_hit_rate"`
	CostUSD         *float64           `json:"cost_usd"`
	ToolCalls       map[string]float64 `json:"tool_calls"`
	// InputBreakdown and InputBreakdownRequests always come from events
	// (I16, ADR 0032), also when the rest of the row is from Prometheus.
	InputBreakdown         UsageInputBreakdown `json:"input_breakdown"`
	InputBreakdownRequests float64             `json:"input_breakdown_requests"`
}

// UsageInputBreakdown is the input composition of a group (nil = no event
// of the group carried fields.input_breakdown). Same layout as
// store.InputBreakdown, so one converts into the other.
type UsageInputBreakdown struct {
	System      *float64 `json:"system"`
	ToolSchemas *float64 `json:"tool_schemas"`
	User        *float64 `json:"user"`
	Assistant   *float64 `json:"assistant"`
	ToolCalls   *float64 `json:"tool_calls"`
	ToolResults *float64 `json:"tool_results"`
	Images      *float64 `json:"images"`
	Cached      *float64 `json:"cached"`
	Total       *float64 `json:"total"`
}

// Values returns the values in the order of store.InputBreakdownKeys.
func (b UsageInputBreakdown) Values() []*float64 {
	return []*float64{b.System, b.ToolSchemas, b.User, b.Assistant, b.ToolCalls, b.ToolResults, b.Images, b.Cached, b.Total}
}

// Usage is the body of GET /api/v1/usage.
type Usage struct {
	Group  string     `json:"group"`
	Window string     `json:"window"`
	Rows   []UsageRow `json:"rows"`
}

// UsageQuery selects Usage and UsageSeries.
type UsageQuery struct {
	Group  string // driver | model | key | account | ip
	Window string // 1m | 5m | 1h | 24h | 7d ("" = 1h)
	Driver string
	Model  string
	Limit  int    // rows (<= 0 = 100)
	Value  string // UsageSeries: value of the group
}

func (q *UsageQuery) check() (time.Duration, error) {
	if !slices.Contains(usageGroups, q.Group) {
		return 0, invalid("group inválido", "group: use "+strings.Join(usageGroups, ", "))
	}
	if q.Window == "" {
		q.Window = "1h"
	}
	d, ok := usageWindows[q.Window]
	if !ok {
		return 0, invalid("window inválida", "window: use 1m, 5m, 1h, 24h, 7d")
	}
	return d, nil
}

func add(p **float64, v float64) {
	if *p == nil {
		*p = new(float64)
	}
	**p += v
}

func hitRate(cached, prompt *float64) *float64 {
	if cached == nil || prompt == nil || *prompt <= 0 {
		return nil
	}
	v := min(1, *cached / *prompt)
	return &v
}

// Usage aggregates the fake inference usage (spec 016): driver and model
// from Prometheus when available (exact), everything else from the events
// table weighted by 1/sample.
func (a *API) Usage(ctx context.Context, q UsageQuery) (Usage, error) {
	d, err := q.check()
	if err != nil {
		return Usage{}, err
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	since := a.now().Add(-d)
	evRows, err := a.st.Events.Usage(ctx, store.UsageQuery{GroupBy: q.Group, Since: since, Driver: q.Driver, Model: q.Model})
	byValue := map[string]*UsageRow{}
	rows := make([]UsageRow, 0, len(evRows))
	needTools := false
	for _, r := range evRows {
		row := UsageRow{
			Value: r.Group, Source: SourceEvents, Requests: r.Requests + r.Errors, PromptTokens: r.PromptTokens,
			OutputTokens: r.OutputTokens, ReasoningTokens: r.ReasoningTokens, CachedTokens: r.CachedTokens, CostUSD: r.CostUSD,
			InputBreakdown: UsageInputBreakdown(r.InputBreakdown), InputBreakdownRequests: r.InputBreakdownRequests,
		}
		if r.ToolCalls != nil {
			row.ToolCalls = map[string]float64{}
			needTools = true
		}
		rows = append(rows, row)
	}
	for i := range rows {
		byValue[rows[i].Value] = &rows[i]
	}
	if err == nil && needTools {
		var tools map[string]map[string]float64
		tools, err = a.st.Events.ToolCallsByGroup(ctx, store.UsageQuery{GroupBy: q.Group, Since: since, Driver: q.Driver, Model: q.Model})
		for v, names := range tools {
			if r := byValue[v]; r != nil && r.ToolCalls != nil {
				maps.Copy(r.ToolCalls, names)
			}
		}
	}
	if err != nil {
		return Usage{}, dbErr(err)
	}
	if (q.Group == store.GroupDriver || q.Group == store.GroupModel) && a.prom != nil {
		if prom, err := a.promUsage(ctx, q, byValue); err == nil {
			rows = prom
		} else {
			a.log.Warn("painel: uso pelo Prometheus; usando events", "err", err)
		}
	}
	for i := range rows {
		rows[i].CacheHitRate = hitRate(rows[i].CachedTokens, rows[i].PromptTokens)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Requests != rows[j].Requests {
			return rows[i].Requests > rows[j].Requests
		}
		return rows[i].Value < rows[j].Value
	})
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return Usage{Group: q.Group, Window: q.Window, Rows: rows}, nil
}

// promMatchers renders the driver/model matchers plus extra.
func promMatchers(driver, model string, extra ...string) string {
	var m []string
	if driver != "" {
		m = append(m, "driver="+strconv.Quote(driver))
	}
	if model != "" {
		m = append(m, "model="+strconv.Quote(model))
	}
	m = append(m, extra...)
	if len(m) == 0 {
		return ""
	}
	return "{" + strings.Join(m, ", ") + "}"
}

// promUsage reads requests and tokens by driver/model from Prometheus and
// takes cost and tool calls from the events rows.
func (a *API) promUsage(ctx context.Context, q UsageQuery, ev map[string]*UsageRow) ([]UsageRow, error) {
	m := promMatchers(q.Driver, q.Model)
	reqs, err := a.prom.Query(ctx, "sum by ("+q.Group+") (increase(wwz_requests_total"+m+"["+q.Window+"]))", time.Time{})
	if err != nil {
		return nil, err
	}
	toks, err := a.prom.Query(ctx, "sum by ("+q.Group+", kind) (increase(wwz_tokens_total"+m+"["+q.Window+"]))", time.Time{})
	if err != nil {
		return nil, err
	}
	rows := map[string]*UsageRow{}
	get := func(v string) *UsageRow {
		r := rows[v]
		if r == nil {
			r = &UsageRow{Value: v, Source: SourcePrometheus}
			if e := ev[v]; e != nil {
				r.CostUSD, r.ToolCalls = e.CostUSD, e.ToolCalls
				r.InputBreakdown, r.InputBreakdownRequests = e.InputBreakdown, e.InputBreakdownRequests
			}
			rows[v] = r
		}
		return r
	}
	for _, s := range reqs {
		get(s.Labels[q.Group]).Requests += s.Value
	}
	for _, s := range toks {
		r := get(s.Labels[q.Group])
		switch s.Labels["kind"] {
		case "prompt":
			add(&r.PromptTokens, s.Value)
		case "completion":
			add(&r.OutputTokens, s.Value)
		case "reasoning":
			add(&r.ReasoningTokens, s.Value)
			add(&r.OutputTokens, s.Value)
		case "cached":
			add(&r.CachedTokens, s.Value)
		}
	}
	out := make([]UsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	return out, nil
}

// ---- series ----

// UsageSeries is the body of GET /api/v1/usage/series.
type UsageSeries struct {
	Group  string             `json:"group"`
	Value  string             `json:"value"`
	Window string             `json:"window"`
	StepS  float64            `json:"step_s"`
	Points map[string][]Point `json:"points"`
}

// seriesKeys are the keys of UsageSeries.Points.
var seriesKeys = []string{"requests", "prompt_tokens", "output_tokens", "cost_usd"}

// UsageSeries returns 60 points of requests, tokens and cost of one group
// value over the window: events (weighted) for every group, with requests
// and tokens from Prometheus for driver/model when available.
func (a *API) UsageSeries(ctx context.Context, q UsageQuery) (UsageSeries, error) {
	d, err := q.check()
	if err != nil {
		return UsageSeries{}, err
	}
	if q.Value == "" {
		return UsageSeries{}, invalid("value é obrigatório")
	}
	step := d / 60
	end := a.now()
	start := end.Add(-d)
	pts, err := a.st.Events.UsageBuckets(ctx, store.UsageQuery{GroupBy: q.Group, Value: q.Value, Since: start}, step, 60)
	if err != nil {
		return UsageSeries{}, dbErr(err)
	}
	buckets := make([]map[string]*float64, 60)
	for i := range buckets {
		buckets[i] = map[string]*float64{"requests": new(float64)}
	}
	for _, p := range pts {
		i := int((p.At.Sub(start) + step/2) / step) // start has ns, buckets have ms
		*buckets[i]["requests"] = p.Requests + p.Errors
		buckets[i]["prompt_tokens"], buckets[i]["output_tokens"], buckets[i]["cost_usd"] = p.PromptTokens, p.OutputTokens, p.CostUSD
	}
	out := UsageSeries{Group: q.Group, Value: q.Value, Window: q.Window, StepS: step.Seconds(), Points: map[string][]Point{}}
	for _, k := range seriesKeys {
		pts := make([]Point, 60)
		for i, b := range buckets {
			pts[i] = NewPoint(start.Add(time.Duration(i)*step), b[k])
		}
		out.Points[k] = pts
	}
	if (q.Group == store.GroupDriver || q.Group == store.GroupModel) && a.prom != nil {
		if err := a.promSeries(ctx, q, start, end, step, out.Points); err != nil {
			a.log.Warn("painel: série pelo Prometheus; usando events", "err", err)
		}
	}
	return out, nil
}

// promSeries replaces requests and tokens by the Prometheus range queries.
func (a *API) promSeries(ctx context.Context, q UsageQuery, start, end time.Time, step time.Duration, pts map[string][]Point) error {
	driver, model := q.Value, ""
	if q.Group == store.GroupModel {
		driver, model = "", q.Value
	}
	m := func(extra ...string) string { return promMatchers(driver, model, extra...) }
	rng := "[" + strconv.FormatFloat(step.Seconds(), 'f', -1, 64) + "s]"
	queries := map[string]string{
		"requests":      "sum(increase(wwz_requests_total" + m() + rng + "))",
		"prompt_tokens": "sum(increase(wwz_tokens_total" + m(`kind="prompt"`) + rng + "))",
		"output_tokens": "sum(increase(wwz_tokens_total" + m(`kind=~"completion|reasoning"`) + rng + "))",
	}
	got := map[string][]Point{}
	for _, k := range seriesKeys[:3] {
		series, err := a.prom.QueryRange(ctx, queries[k], start, end, step)
		if err != nil {
			return err
		}
		got[k] = sumSeries(series, 1)
	}
	for k, v := range got {
		pts[k] = v
	}
	return nil
}

// ---- handlers ----

func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, invalid(name+" inválido", name+": inteiro entre "+strconv.Itoa(lo)+" e "+strconv.Itoa(hi))
	}
	return n, nil
}

func formatParam(q url.Values) (string, error) {
	switch f := q.Get("format"); f {
	case "", store.FormatJSON:
		return store.FormatJSON, nil
	case store.FormatCSV:
		return f, nil
	}
	return "", invalid("format inválido", "format: use json ou csv")
}

func (a *API) getUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 100, 1, 10000)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	format, err := formatParam(q)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	u, err := a.Usage(r.Context(), UsageQuery{
		Group: q.Get("group"), Window: q.Get("window"), Driver: q.Get("driver"), Model: q.Get("model"), Limit: limit,
	})
	if err != nil {
		a.fail(w, err, "")
		return
	}
	if format == store.FormatCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="usage-`+u.Group+`.csv"`)
		_ = WriteUsageCSV(w, u)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// UsageCSVHeader is the first line of the usage CSV.
var UsageCSVHeader = []string{
	"value", "source", "requests", "prompt_tokens", "output_tokens", "reasoning_tokens", "cached_tokens",
	"cache_hit_rate", "cost_usd", "tool_calls",
	"input_system", "input_tool_schemas", "input_user", "input_assistant", "input_tool_calls", "input_tool_results",
	"input_images", "input_cached", "input_total", "input_breakdown_requests",
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func optNum(v *float64) string {
	if v == nil {
		return ""
	}
	return num(*v)
}

// WriteUsageCSV writes u as CSV (null = empty field; tool_calls as
// name=n;name=n sorted by name, 0 when known and none; input_breakdown flattened in the nine
// input_<key> columns, then input_breakdown_requests).
func WriteUsageCSV(w io.Writer, u Usage) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(UsageCSVHeader)
	for _, r := range u.Rows {
		tools := ""
		if r.ToolCalls != nil {
			names := make([]string, 0, len(r.ToolCalls))
			for n := range r.ToolCalls {
				names = append(names, n)
			}
			sort.Strings(names)
			parts := make([]string, len(names))
			for i, n := range names {
				parts[i] = n + "=" + num(r.ToolCalls[n])
			}
			tools = strings.Join(parts, ";")
			if tools == "" {
				tools = "0" // known and zero, unlike null
			}
		}
		rec := []string{
			r.Value, r.Source, num(r.Requests), optNum(r.PromptTokens), optNum(r.OutputTokens), optNum(r.ReasoningTokens),
			optNum(r.CachedTokens), optNum(r.CacheHitRate), optNum(r.CostUSD), tools,
		}
		for _, v := range r.InputBreakdown.Values() {
			rec = append(rec, optNum(v))
		}
		_ = cw.Write(append(rec, num(r.InputBreakdownRequests)))
	}
	cw.Flush()
	return cw.Error()
}

func (a *API) getUsageSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, err := a.UsageSeries(r.Context(), UsageQuery{Group: q.Get("group"), Window: q.Get("window"), Value: q.Get("value")})
	if err != nil {
		a.fail(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, s)
}
