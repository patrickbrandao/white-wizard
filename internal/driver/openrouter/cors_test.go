package openrouter_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// TestAllowOriginOnEveryRoute (ADR 0061): access-control-allow-origin "*"
// goes on every answer of the driver: account routes, chat, messages,
// responses (200, stream and errors) and models.
func TestAllowOriginOnEveryRoute(t *testing.T) {
	cfg, err := config.Parse([]byte(keyFixtureCfg + `
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: openai/gpt-4o-mini, protocols: [openai], latency: fast}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(100), nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + keyFixtureKey
	chat := `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	msgs := `{"model":"openai/gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	stream := `{"model":"openai/gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, method, path, auth, body string
		status                         int
		want                           string
	}{
		{"key", "GET", "/api/v1/key", auth, "", 200, "*"},
		{"auth/key", "GET", "/api/v1/auth/key", auth, "", 200, "*"},
		{"credits", "GET", "/api/v1/credits", auth, "", 200, "*"},
		{"keys list", "GET", "/api/v1/keys", auth, "", 401, "*"},
		{"keys create", "POST", "/api/v1/keys", auth, `{"name":"x"}`, 401, "*"},
		{"key without auth", "GET", "/api/v1/key", "", "", 401, "*"},
		{"credits bad key", "GET", "/api/v1/credits", "Bearer abc", "", 401, "*"},
		{"chat", "POST", "/api/v1/chat/completions", auth, chat, 200, "*"},
		{"chat stream", "POST", "/api/v1/chat/completions", auth, stream, 200, "*"},
		{"chat without auth", "POST", "/api/v1/chat/completions", "", chat, 401, "*"},
		{"chat unknown model", "POST", "/api/v1/chat/completions", auth, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, 400, "*"},
		{"messages", "POST", "/api/v1/messages", auth, msgs, 200, "*"},
		{"messages without auth", "POST", "/api/v1/messages", "", msgs, 401, "*"},
		{"models", "GET", "/api/v1/models", auth, "", 200, "*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, param, ok := keyRouteOf(drv, c.method, c.path)
			if !ok {
				t.Fatalf("no route for %s %s", c.method, c.path)
			}
			hdr := driver.MapHeaders{"content-type": "application/json"}
			if c.auth != "" {
				hdr["authorization"] = c.auth
			}
			w := core.NewMemWriter()
			e.Handle(context.Background(), engine.Input{
				Driver: drv, Kind: kind, Method: c.method, Path: c.path, PathParam: param,
				Headers: hdr, Body: []byte(c.body), ClientIP: "127.0.0.1",
			}, w)
			if w.StatusCode() != c.status {
				t.Fatalf("status %d, want %d: %s", w.StatusCode(), c.status, w.Body())
			}
			if got := headerOf(w, "access-control-allow-origin"); got != c.want {
				t.Errorf("access-control-allow-origin %q, want %q", got, c.want)
			}
		})
	}
}
