package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func statsAdmin(t *testing.T, body string) *Admin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return NewAdmin(srv.URL, "", nil)
}

func deadAdmin(t *testing.T) *Admin {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return NewAdmin(srv.URL, "", &http.Client{Timeout: time.Second})
}

func TestMetricsErrorsProm(t *testing.T) {
	ctx := context.Background()
	f, p := newFakeProm(t)
	f.results[`sum by (driver, kind, phase) (increase(wwz_errors_injected_total{driver="openai"}[5m]))`] = vec(
		`{"driver":"openai","kind":"overloaded","phase":"pre"}`, "3",
		`{"driver":"openai","kind":"rate_limit","phase":"mid_stream"}`, "2",
		`{"driver":"openai","kind":"x","phase":"pre"}`, "NaN",
	)
	f.results[`max(wwz_store_circuit_open)`] = vec(`{}`, "1")
	f.results[`sum(increase(wwz_requests_total{driver="openai", model="m", status="400"}[5m]))`] = vec(`{}`, "NaN")
	m := NewMetrics(p, deadAdmin(t))
	eb, err := m.Errors(ctx, Filter{Window: "5m", Driver: "openai", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if eb.Source != "prometheus" || eb.ByType["overloaded"] != 3 || eb.ByType["rate_limit"] != 2 || eb.ByPhase["pre"] != 3 ||
		eb.ByPhase["mid_stream"] != 2 || eb.ByDriver["openai"] != 5 || len(eb.ByType) != 2 {
		t.Fatalf("errors = %+v", eb)
	}
	if v := eb.Simulator[SimStoreDown]; v == nil || *v != 1 {
		t.Fatalf("store_down = %v", v)
	}
	if eb.Simulator[SimPoliciesRefused] != nil || eb.Simulator[SimBadRequest] != nil || len(eb.Simulator) != 3 {
		t.Fatalf("simulator = %+v", eb.Simulator)
	}
	if _, err := m.Errors(ctx, Filter{Window: "2h"}); err == nil {
		t.Fatal("bad window: want error")
	}
	// A failing simulator query falls back to the admin (dead here).
	f.results[`max(wwz_store_circuit_open)`] = `{"resultType":"matrix","result":[]}`
	if _, err := m.Errors(ctx, Filter{}); err == nil || !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("both down: %v", err)
	}
	f.fail = true
	if _, err := m.Errors(ctx, Filter{}); err == nil || !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("prom down + admin down: %v", err)
	}
}

func TestMetricsErrorsAdmin(t *testing.T) {
	ctx := context.Background()
	a := statsAdmin(t, `{"started":"2026-10-30T12:00:00Z","store":"memory","totals":{"requests":9,
		"by_status":{"200":5,"400":4},"errors_by_kind":{"overloaded":2},"errors_by_phase":{"pre":2}},"by_model":[]}`)
	m := NewMetrics(nil, a)
	eb, err := m.Errors(ctx, Filter{Driver: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if eb.Source != "admin" || eb.ByType["overloaded"] != 2 || eb.ByPhase["pre"] != 2 || len(eb.ByDriver) != 0 || eb.PromError != "" {
		t.Fatalf("admin = %+v", eb)
	}
	if v := eb.Simulator[SimBadRequest]; v == nil || *v != 4 || eb.Simulator[SimStoreDown] != nil {
		t.Fatalf("simulator = %+v", eb.Simulator)
	}
	// Without 400 in by_status, bad_request has no data.
	m = NewMetrics(nil, statsAdmin(t, `{"totals":{},"by_model":[]}`))
	if eb, err := m.Errors(ctx, Filter{}); err != nil || eb.Simulator[SimBadRequest] != nil {
		t.Fatalf("no 400: %+v %v", eb, err)
	}
	// Prometheus failing: fallback with the reason.
	f, p := newFakeProm(t)
	f.fail = true
	m = NewMetrics(p, a)
	if eb, err := m.Errors(ctx, Filter{}); err != nil || eb.Source != "admin" || eb.PromError == "" {
		t.Fatalf("fallback: %+v %v", eb, err)
	}
	// Admin down without Prometheus.
	if _, err := NewMetrics(nil, deadAdmin(t)).Errors(ctx, Filter{}); err == nil {
		t.Fatal("admin down: want error")
	}
}

func TestMetricsScenarioSteps(t *testing.T) {
	ctx := context.Background()
	if _, err := NewMetrics(nil, nil).ScenarioSteps(ctx, "5m"); err != ErrPromOff {
		t.Fatalf("no prom: %v", err)
	}
	f, p := newFakeProm(t)
	m := NewMetrics(p, nil)
	if _, err := m.ScenarioSteps(ctx, "2h"); err == nil {
		t.Fatal("bad window: want error")
	}
	f.results[`sum by (scenario, step) (increase(wwz_scenario_steps_total[1h]))`] = vec(
		`{"scenario":"s1","step":"a"}`, "4", `{"scenario":"s1"}`, "1", `{"scenario":"s2","step":"b"}`, "NaN", `{"scenario":"s1","step":"a"}`, "1",
	)
	got, err := m.ScenarioSteps(ctx, "1h")
	if err != nil || got["s1"]["a"] != 5 || got["s1"][""] != 1 || len(got) != 1 {
		t.Fatalf("steps = %v %v", got, err)
	}
	f.fail = true
	if _, err := m.ScenarioSteps(ctx, "1h"); err == nil {
		t.Fatal("prom down: want error")
	}
}
