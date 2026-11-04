package driver_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// TestChaosNextRawBodyEndToEnd: POST /admin/chaos/next with "body" (via
// Injector.ScheduleNext, the same call the admin handler makes) makes the
// next response of that test carry exactly that body, in every driver, with
// and without streaming; the following request is served normally.
func TestChaosNextRawBodyEndToEnd(t *testing.T) {
	cfg, err := config.Parse([]byte("server: {seed: 42}\nprofiles:\n  latency:\n    fast: {ttft: 0ms, itl: 0ms}\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(100), nil), slog.New(slog.NewTextHandler(io.Discard, nil)))

	chat := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]%s}`
	msgs := `{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]%s}`
	cases := []struct {
		drv, path, body string
		headers         map[string]string
	}{
		{"openai", "/v1/chat/completions", chat, map[string]string{"authorization": "Bearer wwz-key-01"}},
		{"anthropic", "/v1/messages", msgs, map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}},
		{"openrouter", "/api/v1/chat/completions", chat, map[string]string{"authorization": "Bearer wwz-key-01"}},
		{"opencode", "/zen/go/v1/chat/completions", chat, map[string]string{"authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}},
	}
	raws := []struct {
		body, ct string
		status   int
	}{
		{"{\"weird\": [1, 2,  3]}\n", core.RawBodyJSON, 503},
		{"<html>upstream exploded</html>", core.RawBodyText, 502},
	}
	for _, tc := range cases {
		drv, err := driver.New(tc.drv, nil)
		if err != nil {
			t.Fatal(err)
		}
		kind := routeKind(t, drv, tc.path)
		for i, stream := range []bool{false, true} {
			raw := raws[i]
			name := tc.drv + map[bool]string{false: "/non-stream", true: "/stream"}[stream]
			t.Run(name, func(t *testing.T) {
				testID := "t-" + strings.ReplaceAll(name, "/", "-")
				if err := e.Injector.ScheduleNext(chaos.Next{TestID: testID, Status: raw.status, Body: raw.body}); err != nil {
					t.Fatal(err)
				}
				extra := ""
				if stream {
					extra = `,"stream":true`
				}
				body := strings.Replace(tc.body, "%s", extra, 1)
				h := driver.MapHeaders{"x-test-id": testID}
				for k, v := range tc.headers {
					h[k] = v
				}
				in := engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: tc.path, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}

				w := core.NewMemWriter()
				e.Handle(context.Background(), in, w)
				if got := string(w.Body()); got != raw.body {
					t.Fatalf("body %q, want %q", got, raw.body)
				}
				if w.StatusCode() != raw.status {
					t.Fatalf("status %d, want %d", w.StatusCode(), raw.status)
				}
				if ct := w.Headers()["Content-Type"]; ct != raw.ct {
					t.Fatalf("content-type %q, want %q", ct, raw.ct)
				}

				w2 := core.NewMemWriter()
				e.Handle(context.Background(), in, w2)
				if w2.StatusCode() != 200 {
					t.Fatalf("one-shot not consumed: status %d body %s", w2.StatusCode(), w2.Body())
				}
			})
		}
	}
}

func routeKind(t *testing.T, d driver.Driver, path string) driver.Kind {
	t.Helper()
	for _, rt := range d.Routes() {
		if rt.Path == path && rt.Method == "POST" {
			return rt.Kind
		}
	}
	t.Fatalf("%s: no route for %s", d.Name(), path)
	return ""
}
