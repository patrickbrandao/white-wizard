package engine_test

import (
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// Task 267: the gateways translate a Responses request into a Chat one for
// the engine (task 240), but metrics, events and records name the route the
// request came through: endpoint "responses", stream or not, success or
// error.
func TestGatewayResponsesEndpointLabel(t *testing.T) {
	const cfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gw-model, latency: fast}
keys:
  - {key: sk-or-v1-gw, label: gw}
`
	for _, c := range []struct{ drv, path string }{
		{"openrouter", "/api/v1/responses"},
		{"opencode", "/zen/go/v1/responses"},
	} {
		t.Run(c.drv, func(t *testing.T) {
			e := newEngine(t, cfg)
			hdr := map[string]string{"Authorization": "Bearer sk-or-v1-gw", "x-wwz-path": c.path}
			model := "gw-model"
			if c.drv == "openrouter" {
				model = "white-wizard/gw-model"
			}
			for _, stream := range []string{"false", "true"} {
				body := `{"model":"` + model + `","input":"hi","max_output_tokens":16,"stream":` + stream + `}`
				if w := call(t, e, c.drv, c.path, hdr, body); w.StatusCode() != 200 {
					t.Fatalf("stream=%s: %d %s", stream, w.StatusCode(), w.Body())
				}
				if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Endpoint != "responses" {
					t.Errorf("stream=%s: recorded %+v, want endpoint responses", stream, rec)
				}
			}
			if n := e.M.Requests.Value(c.drv, "gw-model", "responses", "200"); n != 2 {
				t.Errorf("wwz_requests_total{endpoint=responses} = %d, want 2", n)
			}
			if n := e.M.Requests.Value(c.drv, "gw-model", "chat", "200"); n != 0 {
				t.Errorf("wwz_requests_total{endpoint=chat} = %d, want 0", n)
			}
			// An error after the parse (the engine refuses the key) keeps
			// the route's name too.
			bad := map[string]string{"Authorization": "Bearer sk-or-v1-nope", "x-wwz-path": c.path}
			body := `{"model":"` + model + `","input":"hi"}`
			if w := call(t, e, c.drv, c.path, bad, body); w.StatusCode() < 400 {
				t.Fatalf("invalid key: %d %s", w.StatusCode(), w.Body())
			}
			if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Endpoint != "responses" || rec[0].Status < 400 {
				t.Errorf("error recorded %+v, want endpoint responses", rec)
			}
		})
	}
}
