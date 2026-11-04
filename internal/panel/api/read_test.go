package api

import (
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// fakeAdmin answers GET paths with fixed JSON bodies (404 otherwise).
func fakeAdmin(t *testing.T, bodies map[string]string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","message":"sem rota"}`)
			return
		}
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// deadValkey is a Valkey source pointing to a closed port.
func deadValkey(t *testing.T) *source.Valkey {
	t.Helper()
	v, err := source.NewValkey(panelcfg.Valkey{Addr: []string{strings.TrimPrefix(deadURL(t), "http://")}}, source.ValkeyOptions{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v
}

func overviewProm() http.Handler {
	return promHandler(
		promRule{match: `status=~`, body: vector(sample(`"status":"429"`, "2"))},
		promRule{match: "wwz_requests_total", body: vector(sample(``, "12.5"))},
		promRule{match: "wwz_tokens_total", body: vector(sample(`"kind":"prompt"`, "100"), sample(`"kind":"completion"`, "50"))},
		promRule{match: "wwz_requests_in_flight", body: vector(sample(``, "3"))},
		promRule{match: "wwz_ttft_seconds_bucket", body: vector(sample(``, "0.25"))},
		promRule{match: "wwz_e2e_seconds_bucket", body: vector(sample(``, "1.5"))},
		promRule{match: "wwz_errors_injected_total", body: vector(sample(`"kind":"overloaded"`, "0.5"))},
		promRule{match: "wwz_rate_limited_total", body: vector(sample(`"limit":"rpm"`, "1"))},
		promRule{rng: true, match: "wwz_tokens_total", body: matrix(
			`{"metric":{"kind":"prompt"},"values":[[1700000000,"1"],[1700000060,"2"]]}`,
			`{"metric":{"kind":"completion"},"values":[[1700000000,"3"]]}`)},
		promRule{rng: true, match: "wwz_requests_total", body: matrix(`{"metric":{},"values":[[1700000000,"4"]]}`)},
	)
}

func TestOverview(t *testing.T) {
	// Without Prometheus: /admin/stats of the in-process wwz.
	e := newEnv(t)
	var ov Overview
	r := e.do("GET", "/api/v1/overview", "", operator)
	r.decode(t, &ov)
	if r.code != 200 || ov.Source != SourceAdminStats || ov.Window != "5m" || ov.StreamsOpen == nil ||
		len(ov.Instances) != 1 || ov.Instances[0].Store != "memory" || ov.Instances[0].Started == nil || ov.Series != nil {
		t.Fatalf("overview admin: %d %s", r.code, r.body)
	}
	if !strings.Contains(string(r.body), `"tokens_per_s":{}`) || !strings.Contains(string(r.body), `"req_per_s":null`) {
		t.Fatalf("mapas vazios e nulos: %s", r.body)
	}
	// series=true without Prometheus: empty series.
	r = e.do("GET", "/api/v1/overview?"+q("series", "true", "window", "1h"), "", operator)
	if r.code != 200 || !strings.Contains(string(r.body), `"series":{}`) && strings.Contains(string(r.body), `"series"`) {
		t.Fatalf("series sem prometheus: %s", r.body)
	}

	// With Prometheus.
	e = newEnv(t, func(o *envOpts) { o.prom = overviewProm() })
	r = e.do("GET", "/api/v1/overview?"+q("window", "1m", "driver", "openai", "series", "1"), "", operator)
	ov = Overview{}
	r.decode(t, &ov)
	if r.code != 200 || ov.Source != SourcePrometheus || *ov.ReqPerS != 12.5 || *ov.StreamsOpen != 3 ||
		*ov.TTFT.P50MS != 250 || *ov.E2E.P99MS != 1500 || ov.TokensPerS["prompt"] != 100 ||
		ov.ErrorsPerS["overloaded"] != 0.5 || ov.StatusPerS["429"] != 2 || ov.RateLimitedPerS["rpm"] != 1 {
		t.Fatalf("overview prometheus: %d %s", r.code, r.body)
	}
	tok := ov.Series["tokens_per_s"]
	if len(tok) != 2 || *tok[0][1] != 4 || *tok[1][1] != 2 || *tok[0][0] != 1700000000 || len(ov.Series["req_per_s"]) != 1 ||
		len(ov.Series["ttft_p99_ms"]) != 0 {
		t.Fatalf("séries: %s", r.body)
	}

	// Range query failing: no series.
	e = newEnv(t, func(o *envOpts) {
		o.prom = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "query_range") {
				w.WriteHeader(500)
				return
			}
			overviewProm().ServeHTTP(w, r)
		})
	})
	r = e.do("GET", "/api/v1/overview?series=true", "", operator)
	if r.code != 200 || !strings.Contains(string(r.body), `"series":{}`) && strings.Contains(string(r.body), `"series":`) {
		t.Fatalf("séries com range falhando: %s", r.body)
	}

	// Parameters.
	for _, bad := range []string{"window=2m", "series=talvez"} {
		if r := e.do("GET", "/api/v1/overview?"+bad, "", operator); r.code != 400 || r.problem(t).Error != "invalid" {
			t.Errorf("%s: %d %s", bad, r.code, r.body)
		}
	}

	// Admin down and no Prometheus: 502.
	e = newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := e.do("GET", "/api/v1/overview", "", operator); r.code != 502 || r.problem(t).Error != "upstream_unavailable" {
		t.Fatalf("overview sem fontes: %d %s", r.code, r.body)
	}

	// Valkey down: overview still answers with no instances.
	e = newEnv(t, func(o *envOpts) { o.valkey = deadValkey(t) })
	ov = Overview{}
	r = e.do("GET", "/api/v1/overview", "", operator)
	r.decode(t, &ov)
	if r.code != 200 || ov.Instances == nil || len(ov.Instances) != 0 {
		t.Fatalf("overview com valkey fora: %d %s", r.code, r.body)
	}
}

func TestInstances(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/api/v1/instances", "", operator)
	if r.code != 200 || !strings.Contains(string(r.body), `"source":"admin_stats"`) || !strings.Contains(string(r.body), `"store":"memory"`) {
		t.Fatalf("instances: %d %s", r.code, r.body)
	}
	// Stats without pod nor start: id = host of the admin URL.
	url := fakeAdmin(t, map[string]string{"/admin/stats": `{"store":"valkey","totals":{"in_flight":2}}`})
	e = newEnv(t, func(o *envOpts) { o.adminURL = url })
	var body struct {
		Instances []Instance `json:"instances"`
	}
	r = e.do("GET", "/api/v1/instances", "", operator)
	r.decode(t, &body)
	if len(body.Instances) != 1 || body.Instances[0].ID != strings.TrimPrefix(url, "http://") || body.Instances[0].Store != "ok" ||
		*body.Instances[0].InFlight != 2 || body.Instances[0].Started != nil {
		t.Fatalf("instância sem pod: %s", r.body)
	}
	e = newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := e.do("GET", "/api/v1/instances", "", operator); r.code != 502 || !strings.Contains(string(r.body), "admin: ") {
		t.Fatalf("instances admin fora: %d %s", r.code, r.body)
	}
	e = newEnv(t, func(o *envOpts) { o.valkey = deadValkey(t) })
	if r := e.do("GET", "/api/v1/instances", "", operator); r.code != 502 || !strings.Contains(string(r.body), "valkey: ") {
		t.Fatalf("instances valkey fora: %d %s", r.code, r.body)
	}
	for in, want := range map[string]string{"ok": "ok", "open": "open", "local": "local", "memory": "memory", "valkey": "ok", "x": ""} {
		if got := storeState(in); got != want {
			t.Errorf("storeState(%q) = %q", in, got)
		}
	}
}

// usageEvents stores request/error events one minute ago, plus one outside
// every window.
func usageEvents(e *env) {
	at := e.clk.now().Add(-time.Minute)
	ev := func(sid, typ, drv, model, kind, id string, sample float64, data string) store.Event {
		return store.Event{
			StreamID: sid, At: at, Type: typ, Driver: drv, Model: model, EntityKind: kind, EntityID: id, Sample: sample,
			Data: []byte(data),
		}
	}
	e.insertEvents(
		ev("1-0", "request", "openai", "m1", "key", "k1", 0.5,
			`{"key":"k1","account":"acme","ip":"1.1.1.1","fields":{"prompt_tokens":10,"output_tokens":5,"cached_tokens":4,"cost_usd":0.01,"tool_calls":{"search":2}}}`),
		ev("2-0", "error", "openai", "m1", "ip", "1.1.1.1", 1,
			`{"ip":"1.1.1.1","fields":{"prompt_tokens":3,"output_tokens":0,"tool_calls":["search",{"name":"calc"},5,{"x":1}]}}`),
		ev("3-0", "request", "anthropic", "m2", "", "", 1, `{"fields":{"key":"k2","prompt_tokens":1,"tool_calls":3}}`),
		ev("4-0", "request", "anthropic", "m2", "key", "k3", 1, `{"fields":{"prompt_tokens":2,"tool_calls":"x"}}`),
		store.Event{StreamID: "0-1", At: at.Add(-8 * 24 * time.Hour), Type: "request", Driver: "openai", Data: []byte(`{}`)},
	)
}

func usageRows(t *testing.T, r resp) map[string]UsageRow {
	t.Helper()
	var u Usage
	r.decode(t, &u)
	out := map[string]UsageRow{}
	for _, row := range u.Rows {
		out[row.Value] = row
	}
	return out
}

func TestUsageEvents(t *testing.T) {
	e := newEnv(t)
	usageEvents(e)
	r := e.do("GET", "/api/v1/usage?"+q("group", "key", "window", "7d"), "", operator)
	rows := usageRows(t, r)
	k1 := rows["k1"]
	if r.code != 200 || len(rows) != 4 || k1.Requests != 2 || *k1.PromptTokens != 20 || *k1.CachedTokens != 8 ||
		*k1.CacheHitRate != 0.4 || k1.ToolCalls["search"] != 4 || k1.Source != SourceEvents || k1.ReasoningTokens != nil {
		t.Fatalf("usage key: %d %s", r.code, r.body)
	}
	if rows["k2"].Requests != 1 || rows["k3"].Requests != 1 || rows[""].ToolCalls["calc"] != 1 || rows[""].ToolCalls["search"] != 1 ||
		rows["k2"].ToolCalls == nil || len(rows["k2"].ToolCalls) != 0 || rows[""].CacheHitRate != nil {
		t.Fatalf("usage key (fields e entidade): %s", r.body)
	}
	if !strings.Contains(string(r.body), `"reasoning_tokens":null`) {
		t.Fatalf("coluna sem dado deve ser null: %s", r.body)
	}
	for _, g := range []string{"account", "ip", "model"} {
		if r := e.do("GET", "/api/v1/usage?"+q("group", g), "", operator); r.code != 200 {
			t.Fatalf("usage %s: %d %s", g, r.code, r.body)
		}
	}
	rows = usageRows(t, e.do("GET", "/api/v1/usage?"+q("group", "ip", "window", "1h"), "", operator))
	if rows["1.1.1.1"].Requests != 3 {
		t.Fatalf("usage ip: %+v", rows)
	}
	// Limit and order (more requests first, then value).
	var u Usage
	r = e.do("GET", "/api/v1/usage?"+q("group", "driver", "limit", "1", "driver", "openai"), "", operator)
	r.decode(t, &u)
	if len(u.Rows) != 1 || u.Rows[0].Value != "openai" || u.Rows[0].Requests != 3 || u.Window != "1h" {
		t.Fatalf("usage limit: %s", r.body)
	}
	r = e.do("GET", "/api/v1/usage?"+q("group", "key", "limit", "1"), "", operator)
	if rows := usageRows(t, r); len(rows) != 1 || rows["k1"].Requests != 2 {
		t.Fatalf("usage limit 1: %s", r.body)
	}
	// CSV.
	r = e.do("GET", "/api/v1/usage?"+q("group", "key", "format", "csv"), "", operator)
	recs, err := csv.NewReader(strings.NewReader(string(r.body))).ReadAll()
	if err != nil || r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/csv") || len(recs) != 5 ||
		strings.Join(recs[0], ",") != strings.Join(UsageCSVHeader, ",") {
		t.Fatalf("csv: %v %d %s", err, r.code, r.body)
	}
	for _, rec := range recs[1:] {
		switch rec[0] {
		case "k1":
			if rec[2] != "2" || rec[7] != "0.4" || rec[9] != "search=4" || rec[5] != "" {
				t.Errorf("csv k1: %v", rec)
			}
		case "":
			if rec[9] != "calc=1;search=1" {
				t.Errorf("csv sem chave: %v", rec)
			}
		}
	}
	// Invalid parameters.
	for _, bad := range []string{q("group", "x"), q("group", "key", "window", "2d"), q("group", "key", "limit", "0"), q("group", "key", "format", "xml")} {
		if r := e.do("GET", "/api/v1/usage?"+bad, "", operator); r.code != 400 {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
	// SQLite closed: 500.
	_ = e.st.Close()
	if r := e.do("GET", "/api/v1/usage?group=key", "", operator); r.code != 500 || r.problem(t).Error != "internal" {
		t.Fatalf("usage sem banco: %d %s", r.code, r.body)
	}
}

func TestUsagePrometheus(t *testing.T) {
	prom := promHandler(
		promRule{match: "sum by (driver) (increase(wwz_requests_total{model=\"m1\"}[1h]))", body: vector(sample(`"driver":"openai"`, "10"))},
		promRule{match: "sum by (driver, kind) (increase(wwz_tokens_total[1h]))", body: `{"status":"error","errorType":"bad_data","error":"y"}`},
		promRule{match: "sum by (driver, kind)", body: vector(
			sample(`"driver":"openai","kind":"prompt"`, "100"), sample(`"driver":"openai","kind":"completion"`, "40"),
			sample(`"driver":"openai","kind":"reasoning"`, "10"), sample(`"driver":"openai","kind":"cached"`, "25"),
			sample(`"driver":"openai","kind":"other"`, "1"), sample(`"driver":"azure","kind":"prompt"`, "5"))},
		promRule{match: "sum by (model) (increase(wwz_requests_total[1h]))", body: `{"status":"error","errorType":"bad_data","error":"x"}`},
		promRule{match: "sum by (driver) (increase(wwz_requests_total[1h]))", body: vector()},
	)
	e := newEnv(t, func(o *envOpts) { o.prom = prom })
	usageEvents(e)
	r := e.do("GET", "/api/v1/usage?"+q("group", "driver", "model", "m1"), "", operator)
	rows := usageRows(t, r)
	o := rows["openai"]
	if r.code != 200 || o.Source != SourcePrometheus || o.Requests != 10 || *o.PromptTokens != 100 || *o.OutputTokens != 50 ||
		*o.ReasoningTokens != 10 || *o.CachedTokens != 25 || *o.CacheHitRate != 0.25 || *o.CostUSD != 0.02 ||
		o.ToolCalls["search"] != 5 || rows["azure"].CostUSD != nil || rows["azure"].Requests != 0 {
		t.Fatalf("usage prometheus: %d %s", r.code, r.body)
	}
	// Prometheus failing (first or second query): events.
	for _, g := range []string{"model", "driver"} {
		rows = usageRows(t, e.do("GET", "/api/v1/usage?"+q("group", g), "", operator))
		for _, row := range rows {
			if row.Source != SourceEvents {
				t.Fatalf("usage %s sem prometheus: %+v", g, rows)
			}
		}
	}
}

// TestUsageInputBreakdown: the input composition and its request count come
// from events (also over Prometheus rows), null without data, flattened in
// the CSV; a 0.4.0 event without tool_calls counts 0 tool calls (ADR 0032), "0" in
// the CSV and empty only when null.
func TestUsageInputBreakdown(t *testing.T) {
	prom := promHandler(
		promRule{match: "sum by (driver) (increase(wwz_requests_total[1h]))", body: vector(sample(`"driver":"openai"`, "7"), sample(`"driver":"anthropic"`, "1"))},
		promRule{match: "sum by (driver, kind)", body: vector()},
	)
	e := newEnv(t, func(o *envOpts) { o.prom = prom })
	at := e.clk.now().Add(-time.Minute)
	const ib = `"input_breakdown":{"system":40,"tool_schemas":30,"user":20,"assistant":10,"tool_calls":8,"tool_results":12,"images":0,"cached":64,"total":120}`
	e.insertEvents(
		store.Event{StreamID: "1-0", At: at, Type: "request", Driver: "openai", Sample: 0.5, Data: []byte(`{"key":"k1","fields":{"cost_usd":0,` + ib + `}}`)},
		store.Event{StreamID: "2-0", At: at, Type: "request", Driver: "anthropic", Sample: 1, Data: []byte(`{"key":"k2","fields":{"prompt_tokens":1}}`)},
	)
	r := e.do("GET", "/api/v1/usage?"+q("group", "key"), "", operator)
	rows := usageRows(t, r)
	k1, k2 := rows["k1"], rows["k2"]
	if r.code != 200 || k1.InputBreakdownRequests != 2 || *k1.InputBreakdown.System != 80 || *k1.InputBreakdown.Total != 240 ||
		*k1.InputBreakdown.Images != 0 || k1.ToolCalls == nil || len(k1.ToolCalls) != 0 {
		t.Fatalf("breakdown k1: %d %s", r.code, r.body)
	}
	if k2.InputBreakdownRequests != 0 || k2.InputBreakdown != (UsageInputBreakdown{}) || k2.ToolCalls != nil ||
		!strings.Contains(string(r.body), `"input_breakdown":{"system":null,"tool_schemas":null,"user":null,"assistant":null,"tool_calls":null,"tool_results":null,"images":null,"cached":null,"total":null},"input_breakdown_requests":0`) {
		t.Fatalf("breakdown k2: %s", r.body)
	}
	rows = usageRows(t, e.do("GET", "/api/v1/usage?"+q("group", "driver"), "", operator))
	if o := rows["openai"]; o.Source != SourcePrometheus || o.Requests != 7 || o.InputBreakdownRequests != 2 || *o.InputBreakdown.Cached != 128 {
		t.Fatalf("breakdown prometheus: %+v", rows)
	}
	r = e.do("GET", "/api/v1/usage?"+q("group", "key", "format", "csv"), "", operator)
	recs, err := csv.NewReader(strings.NewReader(string(r.body))).ReadAll()
	want := [][]string{
		UsageCSVHeader,
		{"k1", "events", "2", "", "", "", "", "", "0", "0", "80", "60", "40", "20", "16", "24", "0", "128", "240", "2"},
		{"k2", "events", "1", "1", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "0"},
	}
	if err != nil || len(recs) != 3 || strings.Join(recs[0][10:], ",") !=
		"input_system,input_tool_schemas,input_user,input_assistant,input_tool_calls,input_tool_results,input_images,input_cached,input_total,input_breakdown_requests" {
		t.Fatalf("csv: %v %s", err, r.body)
	}
	for i := range want {
		if strings.Join(recs[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("csv linha %d: %v", i, recs[i])
		}
	}
}

func TestUsageSeries(t *testing.T) {
	e := newEnv(t)
	usageEvents(e)
	var s UsageSeries
	r := e.do("GET", "/api/v1/usage/series?"+q("group", "key", "value", "k1"), "", operator)
	r.decode(t, &s)
	if r.code != 200 || s.StepS != 60 || len(s.Points) != 4 || len(s.Points["requests"]) != 60 {
		t.Fatalf("series: %d %s", r.code, r.body)
	}
	var req, prompt float64
	nulls := 0
	for i := range 60 {
		req += *s.Points["requests"][i][1]
		if p := s.Points["prompt_tokens"][i][1]; p != nil {
			prompt += *p
		} else {
			nulls++
		}
	}
	if req != 2 || prompt != 20 || nulls != 59 {
		t.Fatalf("series k1: requests %v prompt %v nulls %d", req, prompt, nulls)
	}
	r = e.do("GET", "/api/v1/usage/series?"+q("group", "model", "value", "m2", "window", "5m"), "", operator)
	r.decode(t, &s)
	req = 0
	for _, p := range s.Points["requests"] {
		req += *p[1]
	}
	if r.code != 200 || req != 2 || s.StepS != 5 {
		t.Fatalf("series m2: %v %s", req, r.body)
	}
	for _, bad := range []string{q("group", "key"), q("group", "x", "value", "a")} {
		if r := e.do("GET", "/api/v1/usage/series?"+bad, "", operator); r.code != 400 {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
	_ = e.st.Close()
	if r := e.do("GET", "/api/v1/usage/series?"+q("group", "key", "value", "k1"), "", operator); r.code != 500 {
		t.Fatalf("series sem banco: %d", r.code)
	}
}

func TestUsageSeriesPrometheus(t *testing.T) {
	prom := promHandler(
		promRule{rng: true, match: `wwz_requests_total{driver="openai"}[60s]`, body: matrix(`{"metric":{},"values":[[1700000000,"9"]]}`)},
		promRule{rng: true, match: `wwz_tokens_total{driver="openai", kind="prompt"}`, body: matrix(`{"metric":{},"values":[[1700000000,"90"]]}`)},
		promRule{rng: true, match: `model="m1"`, body: `{"status":"error","errorType":"x","error":"y"}`},
	)
	e := newEnv(t, func(o *envOpts) { o.prom = prom })
	usageEvents(e)
	var s UsageSeries
	r := e.do("GET", "/api/v1/usage/series?"+q("group", "driver", "value", "openai"), "", operator)
	r.decode(t, &s)
	if r.code != 200 || len(s.Points["requests"]) != 1 || *s.Points["requests"][0][1] != 9 || *s.Points["prompt_tokens"][0][1] != 90 ||
		len(s.Points["output_tokens"]) != 0 || len(s.Points["cost_usd"]) != 60 {
		t.Fatalf("series prometheus: %d %s", r.code, r.body)
	}
	r = e.do("GET", "/api/v1/usage/series?"+q("group", "model", "value", "m1"), "", operator)
	r.decode(t, &s)
	if r.code != 200 || len(s.Points["requests"]) != 60 || !strings.Contains(e.logs.String(), "série pelo Prometheus") {
		t.Fatalf("series prometheus falhando: %d %s", r.code, r.body)
	}
}

// TestServiceDefaults: the service layer (shared with the screens) fills
// the defaults of the contract.
func TestServiceDefaults(t *testing.T) {
	e := newEnv(t)
	usageEvents(e)
	if ov, err := e.api.Overview(ctx, OverviewQuery{}); err != nil || ov.Window != "5m" {
		t.Fatalf("overview: %v %+v", err, ov)
	}
	if u, err := e.api.Usage(ctx, UsageQuery{Group: "key"}); err != nil || u.Window != "1h" || len(u.Rows) != 4 {
		t.Fatalf("usage: %v %+v", err, u)
	}
	if l, err := e.api.Entities(ctx, EntitiesQuery{}); err != nil || l.Entities == nil {
		t.Fatalf("entities: %v %+v", err, l)
	}
	if p, err := e.api.Events(ctx, EventsQuery{}); err != nil || len(p.Events) != 5 {
		t.Fatalf("events: %v %+v", err, p)
	}
	if p, err := e.api.Audit(ctx, AuditQuery{}); err != nil || p.Entries == nil {
		t.Fatalf("audit: %v %+v", err, p)
	}
}
