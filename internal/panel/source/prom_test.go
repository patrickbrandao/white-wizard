package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromQL(t *testing.T) {
	f := Filter{Window: "5m", Driver: "openai", Model: `g"5`}
	m := `{driver="openai", model="g\"5"}`
	want := map[Panel]string{
		PanelReqRate:        `sum(rate(wwz_requests_total` + m + `[5m]))`,
		PanelTokenRate:      `sum by (kind) (rate(wwz_tokens_total` + m + `[5m]))`,
		PanelStreamsOpen:    `sum(wwz_requests_in_flight` + m + `)`,
		PanelTTFTP50:        `histogram_quantile(0.5, sum by (le) (rate(wwz_ttft_seconds_bucket` + m + `[5m])))`,
		PanelTTFTP99:        `histogram_quantile(0.99, sum by (le) (rate(wwz_ttft_seconds_bucket` + m + `[5m])))`,
		PanelE2EP50:         `histogram_quantile(0.5, sum by (le) (rate(wwz_e2e_seconds_bucket` + m + `[5m])))`,
		PanelE2EP99:         `histogram_quantile(0.99, sum by (le) (rate(wwz_e2e_seconds_bucket` + m + `[5m])))`,
		PanelErrorsByType:   `sum by (kind) (rate(wwz_errors_injected_total` + m + `[5m]))`,
		PanelErrorsByStatus: `sum by (status) (rate(wwz_requests_total{driver="openai", model="g\"5", status=~"4..|5.."}[5m]))`,
		PanelRateLimited:    `sum by (limit) (rate(wwz_rate_limited_total` + m + `[5m]))`,
	}
	for _, p := range Panels {
		got, err := PromQL(p, f)
		if err != nil || got != want[p] {
			t.Errorf("%s:\n got %s\nwant %s", p, got, want[p])
		}
	}
	if got, _ := PromQL(PanelReqRate, Filter{}); got != `sum(rate(wwz_requests_total[1m]))` {
		t.Errorf("no filter: %s", got)
	}
	if got, _ := PromQL(PanelErrorsByStatus, Filter{Window: "1h"}); got != `sum by (status) (rate(wwz_requests_total{status=~"4..|5.."}[1h]))` {
		t.Errorf("status only: %s", got)
	}
	if _, err := PromQL(PanelReqRate, Filter{Window: "2m"}); err == nil {
		t.Error("bad window: want error")
	}
	if _, err := PromQL("nope", Filter{}); err == nil {
		t.Error("unknown panel: want error")
	}
}

// fakeProm answers /api/v1/query from results (query → JSON of data) and
// /api/v1/query_range with a fixed matrix.
type fakeProm struct {
	mu      sync.Mutex
	results map[string]string
	fail    bool
	last    map[string]string
}

func (f *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.last = map[string]string{"path": r.URL.Path}
	for k := range q {
		f.last[k] = q.Get(k)
	}
	if f.fail {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"unavailable","error":"down"}`))
		return
	}
	switch r.URL.Path {
	case "/api/v1/query":
		data, ok := f.results[q.Get("query")]
		if !ok {
			data = `{"resultType":"vector","result":[]}`
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
	case "/api/v1/query_range":
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"kind":"prompt"},"values":[[1700000000.5,"1"],[1700000001,"2.5"]]}]}}`))
	}
}

func vec(pairs ...string) string {
	var items []string
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, fmt.Sprintf(`{"metric":%s,"value":[1700000000,"%s"]}`, pairs[i], pairs[i+1]))
	}
	return `{"resultType":"vector","result":[` + strings.Join(items, ",") + `]}`
}

func newFakeProm(t *testing.T) (*fakeProm, *Prom) {
	f := &fakeProm{results: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewProm(srv.URL+"/", nil)
}

func TestPromQuery(t *testing.T) {
	f, p := newFakeProm(t)
	ctx := context.Background()
	f.results["up"] = vec(`{"job":"wwz"}`, "1", `{"job":"x"}`, "0")
	f.results["1"] = `{"resultType":"scalar","result":[1700000000,"1"]}`
	f.results["str"] = `{"resultType":"string","result":[1,"a"]}`
	f.results["badvec"] = `{"resultType":"vector","result":{}}`
	f.results["badval"] = vec(`{}`, "x")
	f.results["badts"] = `{"resultType":"vector","result":[{"metric":{},"value":["t","1"]}]}`
	f.results["badnum"] = `{"resultType":"vector","result":[{"metric":{},"value":[1,1]}]}`
	f.results["badscalar"] = `{"resultType":"scalar","result":{}}`
	f.results["badscalarval"] = `{"resultType":"scalar","result":[1,"x"]}`
	f.results["notjson"] = `x`

	s, err := p.Query(ctx, "up", time.UnixMilli(1_700_000_000_250))
	if err != nil || len(s) != 2 || s[0].Labels["job"] != "wwz" || s[0].Value != 1 || f.last["time"] != "1700000000.250" {
		t.Fatalf("vector = %+v %v %v", s, err, f.last)
	}
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if _, ok := f.last["time"]; ok {
		t.Error("zero time must not send time=")
	}
	for _, q := range []string{"str", "badvec", "badval", "badts", "badnum", "badscalar", "badscalarval", "notjson"} {
		if _, err := p.Query(ctx, q, time.Time{}); err == nil {
			t.Errorf("%s: want error", q)
		}
	}
	series, err := p.QueryRange(ctx, "q", time.Unix(100, 0), time.Unix(160, 0), time.Second)
	if err != nil || len(series) != 1 || len(series[0].Points) != 2 || series[0].Points[0].T.UnixMilli() != 1_700_000_000_500 || series[0].Points[1].V != 2.5 {
		t.Fatalf("range = %+v %v", series, err)
	}
	if f.last["start"] != "100.000" || f.last["end"] != "160.000" || f.last["step"] != "1" || f.last["path"] != "/api/v1/query_range" {
		t.Fatalf("range params = %v", f.last)
	}
	f.fail = true
	if _, err := p.Query(ctx, "up", time.Time{}); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("error status = %v", err)
	}
	if _, err := p.QueryRange(ctx, "q", time.Now(), time.Now(), time.Second); err == nil {
		t.Fatal("range error status: want error")
	}
	if NewProm("", nil) != nil {
		t.Fatal("NewProm(\"\") must be nil")
	}
	var off *Prom
	if _, err := off.Query(ctx, "1", time.Time{}); !errors.Is(err, ErrPromOff) {
		t.Fatalf("nil Prom = %v", err)
	}
	if _, err := NewProm("http://127.0.0.1:1", nil).Query(ctx, "1", time.Time{}); err == nil {
		t.Fatal("connection refused: want error")
	}
	if _, err := NewProm("http://a b", nil).Query(ctx, "1", time.Time{}); err == nil {
		t.Fatal("bad URL: want error")
	}
}

func TestPromQueryRangeBad(t *testing.T) {
	for name, body := range map[string]string{
		"type":   `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		"json":   `{"status":"success","data":{"resultType":"matrix","result":{}}}`,
		"values": `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"x"]]}]}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		if _, err := NewProm(srv.URL, nil).QueryRange(context.Background(), "q", time.Now(), time.Now(), time.Second); err == nil {
			t.Errorf("%s: want error", name)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("{"))
	}))
	defer srv.Close()
	if _, err := NewProm(srv.URL, nil).Query(context.Background(), "q", time.Time{}); err == nil {
		t.Error("cut body: want error")
	}
}

func TestMetricsOverviewProm(t *testing.T) {
	f, p := newFakeProm(t)
	flt := Filter{Window: "1h", Model: "m"}
	q := func(panel Panel) string { s, _ := PromQL(panel, flt); return s }
	f.results[q(PanelReqRate)] = vec(`{}`, "12.5")
	f.results[q(PanelTokenRate)] = vec(`{"kind":"prompt"}`, "100", `{"kind":"completion"}`, "NaN")
	f.results[q(PanelStreamsOpen)] = vec(`{}`, "3")
	f.results[q(PanelTTFTP50)] = vec(`{}`, "0.05")
	f.results[q(PanelTTFTP99)] = vec(`{}`, "NaN")
	f.results[q(PanelE2EP50)] = vec(`{}`, "0.5")
	f.results[q(PanelE2EP99)] = vec(`{}`, "2")
	f.results[q(PanelErrorsByType)] = vec(`{"kind":"overloaded"}`, "0.25")
	f.results[q(PanelErrorsByStatus)] = vec(`{"status":"429"}`, "1")
	m := NewMetrics(p, nil)
	ov, err := m.Overview(context.Background(), flt)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Source != "prometheus" || ov.Window != "1h" || *ov.ReqRate != 12.5 || ov.TokenRate["prompt"] != 100 || len(ov.TokenRate) != 1 ||
		*ov.StreamsOpen != 3 || *ov.TTFTP50 != 0.05 || ov.TTFTP99 != nil || *ov.E2EP50 != 0.5 || *ov.E2EP99 != 2 ||
		ov.ErrorsByType["overloaded"] != 0.25 || ov.ErrorsByStatus["429"] != 1 || ov.RateLimited != nil {
		t.Fatalf("overview = %+v", ov)
	}
	if _, err := m.Overview(context.Background(), Filter{Window: "7d"}); err == nil {
		t.Fatal("bad window: want error")
	}
	m.now = func() time.Time { return time.Unix(10_000, 0) }
	series, err := m.Range(context.Background(), PanelTokenRate, Filter{Window: "1h"})
	if err != nil || len(series) != 1 || f.last["step"] != "60" || f.last["start"] != "6400.000" {
		t.Fatalf("range = %+v %v %v", series, err, f.last)
	}
	if _, err := m.Range(context.Background(), "nope", Filter{}); err == nil {
		t.Fatal("unknown panel: want error")
	}
	if _, err := NewMetrics(nil, nil).Range(context.Background(), PanelReqRate, Filter{}); !errors.Is(err, ErrPromOff) {
		t.Fatalf("no prom range = %v", err)
	}
}

func statsJSON(started string, resetAt string, req, tok, e429, kinds, limited int) string {
	reset := ""
	if resetAt != "" {
		reset = `"reset_at":"` + resetAt + `",`
	}
	return fmt.Sprintf(`{"started":%q,%s"uptime_s":1,"store":"memory",
		"totals":{"requests":%d,"in_flight":2,"streams_open":4,"errors_injected":%d,"rate_limited":%d,"blocked":0,
			"by_status":{"200":%d,"429":%d},"errors_by_kind":{"overloaded":%d},"rate_limited_by_limit":{"rpm":%d}},
		"by_model":[
			{"driver":"openai","model":"a","requests":%d,"ttft_p50_ms":10,"ttft_p99_ms":50,"e2e_p50_ms":100,"tokens":{"prompt":%d}},
			{"driver":"anthropic","model":"b","requests":%d,"ttft_p50_ms":30,"ttft_p99_ms":90,"tokens":{"prompt":%d}},
			{"driver":"openai","model":"c","requests":0}
		]}`, started, reset, 2*req, kinds, limited, 2*req-e429, e429, kinds, limited, req, tok, req, tok)
}

func TestMetricsAdminFallback(t *testing.T) {
	var mu sync.Mutex
	body := statsJSON("2026-10-30T12:00:00Z", "", 10, 100, 1, 2, 3)
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	set := func(b string, s int) { mu.Lock(); body, status = b, s; mu.Unlock() }
	a := NewAdmin(srv.URL, "", nil)
	m := NewMetrics(nil, a)
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	ctx := context.Background()

	ov, err := m.Overview(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// First call: no rates yet; latency combined over both models.
	if ov.Source != "admin" || ov.Window != "" || ov.ReqRate != nil || ov.TokenRate != nil || *ov.StreamsOpen != 4 ||
		*ov.TTFTP50 != 0.02 || *ov.TTFTP99 != 0.09 || *ov.E2EP50 != 0.1 || ov.E2EP99 != nil {
		t.Fatalf("first = %+v", ov)
	}
	clock = clock.Add(10 * time.Second)
	set(statsJSON("2026-10-30T12:00:00Z", "", 30, 300, 11, 12, 23), 200)
	ov, err = m.Overview(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if *ov.ReqRate != 4 || ov.TokenRate["prompt"] != 40 || ov.ErrorsByType["overloaded"] != 1 || ov.ErrorsByStatus["429"] != 1 ||
		len(ov.ErrorsByStatus) != 1 || ov.RateLimited["rpm"] != 2 {
		t.Fatalf("rates = %+v", ov)
	}
	// Filter by one model: its own percentiles and rate.
	clock = clock.Add(10 * time.Second)
	set(statsJSON("2026-10-30T12:00:00Z", "", 40, 400, 11, 12, 23), 200)
	ov, _ = m.Overview(ctx, Filter{Driver: "anthropic", Model: "b"})
	if *ov.ReqRate != 1 || *ov.TTFTP50 != 0.03 || *ov.TTFTP99 != 0.09 || ov.E2EP50 != nil {
		t.Fatalf("filtered = %+v", ov)
	}
	// Counter went back (reset): no rates.
	clock = clock.Add(10 * time.Second)
	set(statsJSON("2026-10-30T12:00:00Z", "2026-10-30T13:00:00Z", 1, 1, 0, 0, 0), 200)
	if ov, _ := m.Overview(ctx, Filter{}); ov.ReqRate != nil || ov.ErrorsByType != nil {
		t.Fatalf("after reset = %+v", ov)
	}
	clock = clock.Add(10 * time.Second)
	set(statsJSON("2026-10-30T12:00:00Z", "2026-10-30T13:00:00Z", 0, 0, 0, 0, 0), 200)
	if ov, _ := m.Overview(ctx, Filter{}); ov.ReqRate != nil || ov.TokenRate != nil || ov.RateLimited["rpm"] != 0 {
		t.Fatalf("counter back = %+v", ov)
	}
	// Prometheus down: falls back and says why.
	fp, p := newFakeProm(t)
	fp.fail = true
	m2 := NewMetrics(p, a)
	ov, err = m2.Overview(ctx, Filter{})
	if err != nil || ov.Source != "admin" || !strings.Contains(ov.PromError, "down") {
		t.Fatalf("prom down = %+v %v", ov, err)
	}
	// Both down: error with both causes.
	set(`{}`, 503)
	if _, err := m2.Overview(ctx, Filter{}); err == nil || !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("both down = %v", err)
	}
	if _, err := m.Overview(ctx, Filter{}); err == nil || strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("admin down without prom = %v", err)
	}
}

func TestRatesAndLatencyHelpers(t *testing.T) {
	if rates(nil, nil, 1) != nil {
		t.Error("empty rates must be nil")
	}
	if p50, p99 := latency(nil, func(ModelStats) (*float64, *float64) { return nil, nil }); p50 != nil || p99 != nil {
		t.Error("no rows must be nil")
	}
}
