package source

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Simulator error keys of ErrorsBreakdown.Simulator.
const (
	SimStoreDown       = "store_down"       // max(wwz_store_circuit_open): 1 while Valkey is unreachable
	SimPoliciesRefused = "policies_refused" // cfg:policies versions refused by validation
	SimBadRequest      = "bad_request"      // 400 answers (parse and validation; injected 400 included)
)

// ErrorsBreakdown is the data of the "Erros" screen (task 053). Counts are
// over the window (Prometheus increase) or, with the /admin/stats fallback,
// since the instance started (Source "admin"). Simulator values are nil
// without data.
type ErrorsBreakdown struct {
	Source    string
	ByType    map[string]float64
	ByPhase   map[string]float64
	ByDriver  map[string]float64
	Simulator map[string]*float64
	// PromError is why Prometheus was skipped, when the fallback ran
	// because it failed.
	PromError string
}

// Errors returns the injected errors by type (the kind label of
// wwz_errors_injected_total, spec 013), phase and driver and the
// errors of the simulator itself, from Prometheus or, when it is off or
// fails, from /admin/stats (whose totals are not split by driver and ignore
// the filter). wwz_errors_injected_total has no model label, so the model
// filter only narrows bad_request.
func (m *Metrics) Errors(ctx context.Context, f Filter) (ErrorsBreakdown, error) {
	if _, _, err := f.window(); err != nil {
		return ErrorsBreakdown{}, err
	}
	var promErr error
	if m.prom != nil {
		eb, err := m.promErrors(ctx, f)
		if err == nil {
			return eb, nil
		}
		promErr = err
	}
	st, err := m.admin.Stats(ctx)
	if err != nil {
		if promErr != nil {
			return ErrorsBreakdown{}, fmt.Errorf("%w (prometheus: %v)", err, promErr)
		}
		return ErrorsBreakdown{}, err
	}
	toF := func(in map[string]int64) map[string]float64 {
		out := make(map[string]float64, len(in))
		for k, v := range in {
			out[k] = float64(v)
		}
		return out
	}
	eb := ErrorsBreakdown{
		Source: "admin", ByType: toF(st.Totals.ErrorsByKind), ByPhase: toF(st.Totals.ErrorsByPhase), ByDriver: map[string]float64{},
		Simulator: map[string]*float64{SimStoreDown: nil, SimPoliciesRefused: nil, SimBadRequest: nil},
	}
	if n, ok := st.Totals.ByStatus["400"]; ok {
		v := float64(n)
		eb.Simulator[SimBadRequest] = &v
	}
	if promErr != nil {
		eb.PromError = promErr.Error()
	}
	return eb, nil
}

func (m *Metrics) promErrors(ctx context.Context, f Filter) (ErrorsBreakdown, error) {
	w, _, _ := f.window()
	drv := Filter{Window: f.Window, Driver: f.Driver}
	inj, err := m.prom.Query(ctx, "sum by (driver, kind, phase) (increase(wwz_errors_injected_total"+drv.matchers()+"["+w+"]))", time.Time{})
	if err != nil {
		return ErrorsBreakdown{}, err
	}
	eb := ErrorsBreakdown{
		Source: "prometheus", ByType: map[string]float64{}, ByPhase: map[string]float64{}, ByDriver: map[string]float64{},
		Simulator: map[string]*float64{},
	}
	for _, s := range inj {
		if math.IsNaN(s.Value) {
			continue
		}
		eb.ByType[s.Labels["kind"]] += s.Value
		eb.ByPhase[s.Labels["phase"]] += s.Value
		eb.ByDriver[s.Labels["driver"]] += s.Value
	}
	for _, q := range []struct{ key, query string }{
		{SimStoreDown, "max(wwz_store_circuit_open)"},
		{SimPoliciesRefused, "sum(increase(wwz_policies_errors_total[" + w + "]))"},
		{SimBadRequest, "sum(increase(wwz_requests_total" + f.matchers(`status="400"`) + "[" + w + "]))"},
	} {
		samples, err := m.prom.Query(ctx, q.query, time.Time{})
		if err != nil {
			return ErrorsBreakdown{}, err
		}
		var v *float64
		if len(samples) > 0 && !math.IsNaN(samples[0].Value) {
			x := samples[0].Value
			v = &x
		}
		eb.Simulator[q.key] = v
	}
	return eb, nil
}

// ScenarioSteps is sum by (scenario, step) (increase(wwz_scenario_steps_total[w]))
// as scenario → step → count; the step "" counts the requests of the
// scenario that matched no step (on_no_match fallback). ErrPromOff without
// Prometheus.
func (m *Metrics) ScenarioSteps(ctx context.Context, window string) (map[string]map[string]float64, error) {
	if m.prom == nil {
		return nil, ErrPromOff
	}
	w, _, err := Filter{Window: window}.window()
	if err != nil {
		return nil, err
	}
	samples, err := m.prom.Query(ctx, "sum by (scenario, step) (increase(wwz_scenario_steps_total["+w+"]))", time.Time{})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, s := range samples {
		if math.IsNaN(s.Value) {
			continue
		}
		sc := s.Labels["scenario"]
		if out[sc] == nil {
			out[sc] = map[string]float64{}
		}
		out[sc][s.Labels["step"]] += s.Value
	}
	return out, nil
}
