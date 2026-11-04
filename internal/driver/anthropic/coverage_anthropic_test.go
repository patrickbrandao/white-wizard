package anthropic_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
)

func covDriver(t *testing.T, opts map[string]string) driver.Driver {
	t.Helper()
	d, err := driver.New("anthropic", opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestGetModelSubpath: /v1/models/{id}/... is an unknown route (task 316,
// fixture routes/model_subpath): 404 "Not found" whose request_id is the
// request-id header, no account headers.
func TestGetModelSubpath(t *testing.T) {
	d := covDriver(t, nil)
	w := core.NewMemWriter()
	d.GetModel(w, &core.Request{}, nil, "claude-haiku-4-5/x")
	id := w.Headers()["request-id"]
	want := `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":"` + id + `"}`
	if w.StatusCode() != 404 || id == "" || string(w.Body()) != want || w.Headers()["x-should-retry"] != "false" {
		t.Errorf("%d %s %v", w.StatusCode(), w.Body(), w.Headers())
	}
	if _, ok := w.Headers()["anthropic-organization-id"]; ok {
		t.Errorf("account header on an unknown route: %v", w.Headers())
	}
}

func TestRequireVersionOption(t *testing.T) {
	d := covDriver(t, nil)
	if d.Protocol() != "anthropic" {
		t.Errorf("protocol = %s", d.Protocol())
	}
	c := d.Authenticate(driver.MapHeaders{"x-api-key": "k"})
	if len(c.Missing) != 1 || c.Missing[0] != "anthropic-version" {
		t.Errorf("missing = %v", c.Missing)
	}
}

func TestParseRequestUnknownKind(t *testing.T) {
	d := covDriver(t, nil)
	_, e := d.ParseRequest(driver.KindResource, nil, driver.MapHeaders{})
	if e == nil || e.Kind != core.ErrNotFound {
		t.Errorf("err = %v", e)
	}
}

func TestModelsExtraAndErrors(t *testing.T) {
	d := covDriver(t, nil)
	m := &config.ModelConfig{
		ID: "claude-haiku-4-5", DeprecatedAt: "not-a-date", RetiresAt: "2030-01-01",
		Extra: map[string]any{
			"line":         "custom",
			"capabilities": map[string]any{"batch": map[string]any{"supported": false}, "new_cap": map[string]any{"deep": map[string]any{"x": 1}}},
			"zzz":          map[string]any{"a": 1},
		},
	}
	w := core.NewMemWriter()
	d.GetModel(w, &core.Request{}, &driver.ModelInfo{Cfg: m}, m.ID)
	var body map[string]any
	if err := json.Unmarshal(w.Body(), &body); err != nil {
		t.Fatalf("%v: %s", err, w.Body())
	}
	if body["line"] != "custom" || body["deprecated_at"] != "not-a-date" || body["retires_at"] != "2030-01-01T00:00:00Z" {
		t.Errorf("body = %v", body)
	}
	caps := body["capabilities"].(map[string]any)
	if caps["batch"].(map[string]any)["supported"] != false || caps["new_cap"] == nil {
		t.Errorf("caps = %v", caps)
	}

	// Unknown model with and without a request.
	w = core.NewMemWriter()
	d.GetModel(w, nil, nil, "nope")
	if w.StatusCode() != 404 || !strings.Contains(string(w.Body()), "model: nope") {
		t.Errorf("404 = %d %s", w.StatusCode(), w.Body())
	}
	w = core.NewMemWriter()
	d.GetModel(w, &core.Request{Headers: driver.MapHeaders{"x-api-key": "k"}, ClientIP: "1.2.3.4"}, nil, "nope")
	if w.StatusCode() != 404 || w.Headers()["request-id"] == "" {
		t.Errorf("404 = %d %v", w.StatusCode(), w.Headers())
	}

	// A value JSON cannot encode becomes a 500 error body.
	bad := &config.ModelConfig{ID: "claude-x", Extra: map[string]any{"bad": make(chan int)}}
	w = core.NewMemWriter()
	d.GetModel(w, &core.Request{}, &driver.ModelInfo{Cfg: bad}, bad.ID)
	if w.StatusCode() != 500 {
		t.Errorf("status = %d, body %s", w.StatusCode(), w.Body())
	}
	w = core.NewMemWriter()
	d.ListModels(w, &core.Request{}, []driver.ModelInfo{{Cfg: bad}})
	if w.StatusCode() != 500 {
		t.Errorf("list status = %d", w.StatusCode())
	}
}

func TestListModelsEmpty(t *testing.T) {
	d := covDriver(t, nil)
	w := core.NewMemWriter()
	d.ListModels(w, &core.Request{}, nil)
	var body map[string]any
	if err := json.Unmarshal(w.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body["first_id"] != nil || body["last_id"] != nil || body["has_more"] != false {
		t.Errorf("body = %v", body)
	}
}

func TestWriteRouteMiss(t *testing.T) {
	d := covDriver(t, nil)
	rm, ok := driver.As[driver.RouteMisser](d)
	if !ok {
		t.Fatal("anthropic does not implement RouteMisser")
	}
	keyed := &core.Request{Headers: driver.MapHeaders{}}
	driver.SetAccount(driver.MapHeaders(keyed.Headers), driver.Account{Key: "bad"})
	auth := &core.Error{Kind: core.ErrInvalidAPIKey}

	cases := []struct {
		name   string
		req    *core.Request
		miss   driver.RouteMiss
		auth   *core.Error
		status int
		check  func(t *testing.T, w *core.MemWriter)
	}{
		{"outside v1", &core.Request{}, driver.RouteMiss{Method: "GET", Path: "/health"}, nil, 404, func(t *testing.T, w *core.MemWriter) {
			if len(w.Body()) != 0 || w.Headers()["Content-Length"] != "0" {
				t.Errorf("body %q %v", w.Body(), w.Headers())
			}
		}},
		{"edge 405", &core.Request{}, driver.RouteMiss{Method: "GET", Path: "/v1/messages", Allow: []string{"POST"}, Kind: driver.KindMessages}, auth, 405, func(t *testing.T, w *core.MemWriter) {
			if !strings.Contains(string(w.Body()), "Method Not Allowed") || w.Headers()["Referrer-Policy"] != "same-origin" {
				t.Errorf("body %s %v", w.Body(), w.Headers())
			}
		}},
		{"invalid key", keyed, driver.RouteMiss{Method: "GET", Path: "/v1/nope"}, auth, 401, nil},
		{"missing key does not stop the 404", &core.Request{}, driver.RouteMiss{Method: "GET", Path: "/v1/nope"}, auth, 404, func(t *testing.T, w *core.MemWriter) {
			if !strings.Contains(string(w.Body()), `"request_id":null`) || w.Headers()["x-should-retry"] != "false" {
				t.Errorf("body %s %v", w.Body(), w.Headers())
			}
		}},
		{"wrong method", &core.Request{}, driver.RouteMiss{Method: "DELETE", Path: "/v1/models", Allow: []string{"GET"}, Kind: driver.KindModels}, nil, 405, func(t *testing.T, w *core.MemWriter) {
			if w.Headers()["Allow"] != "GET,HEAD" || w.Headers()["request-id"] == "" {
				t.Errorf("headers %v", w.Headers())
			}
		}},
		{"wrong method on POST route", &core.Request{}, driver.RouteMiss{Method: "PUT", Path: "/v1/messages", Allow: []string{"POST"}, Kind: driver.KindMessages}, nil, 405, func(t *testing.T, w *core.MemWriter) {
			if w.Headers()["Allow"] != "POST" {
				t.Errorf("headers %v", w.Headers())
			}
		}},
	}
	for _, c := range cases {
		w := core.NewMemWriter()
		rm.WriteRouteMiss(w, c.req, c.miss, c.auth)
		if w.StatusCode() != c.status {
			t.Errorf("%s: status %d, body %s", c.name, w.StatusCode(), w.Body())
		}
		if c.check != nil {
			c.check(t, w)
		}
	}
}
