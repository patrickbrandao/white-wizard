package server

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestResponsesModelOutsideRouteCatalog: on OpenCode's Responses route the
// real refuses an unknown model before the portable route's checks
// (order_store_before_unknown vs order_unknown_model_vs_unknown_field); a
// model that only exists in the Go catalog (opencode-go) called on a
// pay-as-you-go base (/inference, catalog opencode) is unknown there, so
// it gets the same answer at the same step as an unknown id (task 430).
// On the Go base the same body passes the model and reaches the portable
// checks.
func TestResponsesModelOutsideRouteCatalog(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: go-only, latency: fast, pricing: {input_per_m: 1, output_per_m: 1}, catalogs: {opencode-go: {}}}
drivers: [{name: opencode}]
`, nil)
	auth := map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}
	const unavailable = `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`
	const portable = `{"type":"error","error":{"type":"invalid_request_error","message":"zzz: requires a native Responses target; portable routing requires the full conversation in input"}}`
	for _, tc := range []struct {
		path, model string
		status      int
		body        string
	}{
		{"/inference/openai/v1/responses", "go-only", 400, unavailable},
		{"/zen/openai/v1/responses", "go-only", 400, unavailable},
		{"/inference/openai/v1/responses", "wwz-nao-existe", 400, unavailable},
		{"/inference/go/openai/v1/responses", "go-only", 400, portable},
		{"/zen/go/openai/v1/responses", "go-only", 400, portable},
	} {
		body := `{"model":"` + tc.model + `","input":"hi","zzz":1,"store":true}`
		resp := h.do("POST", tc.path, auth, body)
		if resp.StatusCode() != tc.status || string(resp.Body()) != tc.body {
			t.Errorf("%s %s: %d %s\nwant %d %s", tc.path, tc.model, resp.StatusCode(), resp.Body(), tc.status, tc.body)
		}
	}
}

// TestValidationModelsFor: without a route catalog in the headers the
// validator sees the validation itself (drivers without CatalogRouter are
// unchanged); with one, a model outside it is unknown and not retired,
// and a model inside it answers as before.
func TestValidationModelsFor(t *testing.T) {
	cfg := &config.Config{Models: []config.ModelConfig{
		{ID: "go-only", Catalogs: map[string]config.CatalogEntry{"opencode-go": {}}},
		{ID: "everywhere"},
	}}
	drv, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	v := &validation{cfg: cfg, protocol: drv.Protocol(), drv: drv}
	if got := v.modelsFor(nil); got != driver.Models(v) {
		t.Errorf("nil headers: %#v", got)
	}
	if got := v.modelsFor(driver.MapHeaders{}); got != driver.Models(v) {
		t.Errorf("no catalog: %#v", got)
	}
	h := driver.MapHeaders{}
	driver.SetCatalog(h, "opencode")
	m := v.modelsFor(h)
	if m.KnownModel(driver.KindResponses, "go-only") {
		t.Error("go-only known on the opencode catalog")
	}
	if known, retired := m.Model(driver.KindResponses, "go-only"); known || retired {
		t.Errorf("go-only on opencode: known %v retired %v", known, retired)
	}
	if !m.KnownModel(driver.KindResponses, "everywhere") {
		t.Error("everywhere unknown on the opencode catalog")
	}
	driver.SetCatalog(h, "opencode-go")
	if !v.modelsFor(h).KnownModel(driver.KindResponses, "go-only") {
		t.Error("go-only unknown on the opencode-go catalog")
	}
	if !v.KnownModel(driver.KindResponses, "go-only") {
		t.Error("go-only unknown without a catalog")
	}
}
