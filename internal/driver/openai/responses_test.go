package openai_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestResponsesRoute: POST /v1/responses is served (ADR 0054), with
// "resp_" + 50 hex ids, the Responses body and the event stream.
func TestResponsesRoute(t *testing.T) {
	d := newDriver(t)
	found := false
	for _, rt := range d.Routes() {
		found = found || (rt.Method == "POST" && rt.Path == "/v1/responses" && rt.Kind == driver.KindResponses)
	}
	if !found {
		t.Fatal("no /v1/responses route")
	}
	m := d.(driver.IDMinter)
	id := m.ResponseID(driver.KindResponses, generate.NewRNG(1, "t"), time.Unix(1793454317, 0))
	if !regexp.MustCompile(`^resp_[0-9a-f]{50}$`).MatchString(id) {
		t.Errorf("id %q", id)
	}
	req, perr := d.ParseRequest(driver.KindResponses, []byte(`{"model":"gpt-4o-mini","input":"hi"}`), driver.MapHeaders{})
	if perr != nil || req.Endpoint != core.EndpointResponses {
		t.Fatalf("parse: %v", perr)
	}
	resp := &core.Response{ID: id, Model: "gpt-4o-mini-2024-07-18", Created: time.Unix(1793454317, 0), Content: "Hi", FinishReason: core.FinishStop}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, req, resp, nil); err != nil || !strings.HasPrefix(string(w.Body()), "{\n  \"id\": \"resp_") {
		t.Errorf("body: %v %s", err, w.Body())
	}
	w = core.NewMemWriter()
	s := d.StreamWriter(w, req, nil)
	if err := s.Start(resp); err != nil || !strings.HasPrefix(string(w.Body()), "event: response.created\n") {
		t.Errorf("stream: %v %s", err, w.Body())
	}
}
