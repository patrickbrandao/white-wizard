package engine_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// TestAnthropicModelSubpathWithoutKey (task 316, conferred on 2026-10-31 in
// task 342): Anthropic answers GET /v1/models/{id}/... without a key with
// its 404 (request_id filled), with or without anthropic-version; an
// invalid key is still 401 there, and the model itself without a key too.
func TestAnthropicModelSubpathWithoutKey(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42}
models:
  - {id: claude-haiku-4-5}
keys:
  - {key: k-sub, label: sub}
`)
	reqID := regexp.MustCompile(`^\{"type":"error","error":\{"type":"not_found_error","message":"Not found"\},"request_id":"req_[A-Za-z0-9]+"\}$`)
	for _, c := range []struct {
		name, path, param string
		h                 driver.MapHeaders
		status            int
	}{
		{"nokey", "/v1/models/claude-haiku-4-5/x", "claude-haiku-4-5/x", driver.MapHeaders{"anthropic-version": "2023-06-01"}, 404},
		{"nokey_noversion", "/v1/models/claude-haiku-4-5/x/y", "claude-haiku-4-5/x/y", driver.MapHeaders{}, 404},
		{"badkey", "/v1/models/claude-haiku-4-5/x", "claude-haiku-4-5/x", driver.MapHeaders{"anthropic-version": "2023-06-01", "x-api-key": "sk-ant-api03-wwz-invalid"}, 401},
		{"badbearer", "/v1/models/claude-haiku-4-5/x", "claude-haiku-4-5/x", driver.MapHeaders{"authorization": "Bearer bad"}, 401},
		{"model_nokey", "/v1/models/claude-haiku-4-5", "claude-haiku-4-5", driver.MapHeaders{"anthropic-version": "2023-06-01"}, 401},
		{"validkey", "/v1/models/claude-haiku-4-5/x", "claude-haiku-4-5/x", driver.MapHeaders{"anthropic-version": "2023-06-01", "x-api-key": "k-sub"}, 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			drv, err := driver.New("anthropic", nil)
			if err != nil {
				t.Fatal(err)
			}
			w := core.NewMemWriter()
			e.Handle(context.Background(), engine.Input{Driver: drv, Kind: driver.KindModel, Method: "GET", Path: c.path, PathParam: c.param, Headers: c.h, ClientIP: "127.0.0.1"}, w)
			if w.StatusCode() != c.status {
				t.Fatalf("status %d, real %d: %s", w.StatusCode(), c.status, w.Body())
			}
			if c.status == 404 {
				if !reqID.Match(w.Body()) || w.Headers()["request-id"] == "" {
					t.Errorf("body %s headers %v", w.Body(), w.Headers())
				}
			}
		})
	}
}
