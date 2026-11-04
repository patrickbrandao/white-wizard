package openrouter_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestWriteRouteMiss (task 369, testdata/real/openrouter/routes/miss/): a
// 404 in every case, never a 405, whatever the key; the server test
// TestRealFixturesRoutes replays the fixtures byte for byte.
func TestWriteRouteMiss(t *testing.T) {
	rm, ok := driver.As[driver.RouteMisser](newDriver(t))
	if !ok {
		t.Fatal("openrouter is not a RouteMisser")
	}
	const body = `{"error":{"message":"Not Found","code":404}}`
	cases := []struct {
		name  string
		miss  driver.RouteMiss
		ct    string
		cache string
		allow bool // access-control-allow-methods/-headers, no expose-headers
	}{
		{"outside_api", driver.RouteMiss{Method: "GET", Path: "/v1/chat/completions"}, "text/html; charset=utf-8", "public, max-age=0, must-revalidate", false},
		{"unknown_path", driver.RouteMiss{Method: "POST", Path: "/api/v1/nope"}, "application/json", "private, no-store", true},
		{"api_root", driver.RouteMiss{Method: "GET", Path: "/api"}, "application/json", "private, no-store", true},
		{"chat_get", driver.RouteMiss{Method: "GET", Path: "/api/v1/chat/completions", Allow: []string{"POST"}, Kind: driver.KindChat}, "application/json", "private, no-store", false},
		{"chat_put", driver.RouteMiss{Method: "PUT", Path: "/api/v1/chat/completions", Allow: []string{"POST"}, Kind: driver.KindChat}, "application/json", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := core.NewMemWriter()
			rm.WriteRouteMiss(w, nil, c.miss, &core.Error{Kind: core.ErrInvalidAPIKey})
			h := w.Headers()
			if w.StatusCode() != 404 {
				t.Fatalf("status %d", w.StatusCode())
			}
			got := string(w.Body())
			if c.ct == "application/json" && got != body {
				t.Errorf("body %q", got)
			}
			if c.ct != "application/json" && !strings.HasPrefix(got, "<!DOCTYPE html>") {
				t.Errorf("body %.80q, want the site's page", got)
			}
			if h["Content-Type"] != c.ct || h["Cache-Control"] != c.cache || h["Access-Control-Allow-Origin"] != "*" {
				t.Errorf("headers %v", h)
			}
			_, methods := h["Access-Control-Allow-Methods"]
			_, expose := h["Access-Control-Expose-Headers"]
			if methods != c.allow || expose != (c.ct == "application/json" && !c.allow) {
				t.Errorf("CORS headers %v", h)
			}
		})
	}
}

// TestGetModelUnknownRouteCacheControl: the unknown-route 404 of
// /api/v1/models/{...} carries cache-control (fixture model_unknown_sub);
// "Model not found" keeps the headers of the models fixtures.
func TestGetModelUnknownRouteCacheControl(t *testing.T) {
	d := newDriver(t)
	w := core.NewMemWriter()
	d.GetModel(w, nil, nil, "nope")
	if w.StatusCode() != 404 || string(w.Body()) != `{"error":{"message":"Not Found","code":404}}` || w.Headers()["Cache-Control"] != "private, no-store" {
		t.Errorf("%d %s %v", w.StatusCode(), w.Body(), w.Headers())
	}
	w = core.NewMemWriter()
	d.GetModel(w, nil, nil, "openai/nope")
	if _, ok := w.Headers()["Cache-Control"]; ok || !strings.Contains(string(w.Body()), "Model not found: openai/nope") {
		t.Errorf("%s %v", w.Body(), w.Headers())
	}
}
