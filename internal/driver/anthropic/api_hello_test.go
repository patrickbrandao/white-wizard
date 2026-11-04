package anthropic_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// helloFixture is testdata/real/anthropic/routes/api_hello_{get,head}.json
// (curl without a credential, 2026-11-03).
type helloFixture struct {
	Request struct {
		Method string `json:"method"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

// TestAPIHello: the Claude Code probe /api/hello answers 200
// application/json {"message": "hello"} (20 bytes) to every method, with
// any key or none and through the data plane as the real does: HEAD with
// Content-Length 20 and no body, no request-id. It is a public resource
// route of the driver, served by Extra (task 449). Other paths outside /v1
// stay a 404 without a body.
func TestAPIHello(t *testing.T) {
	addr := startWWZ(t, ccConfig)
	for _, name := range []string{"api_hello_get", "api_hello_head"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "anthropic", "routes", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fx helloFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		m := fx.Request.Method
		r, body := rawDo(t, addr, m, []byte(m+" /api/hello HTTP/1.1\r\nHost: wwz\r\nUser-Agent: Bun/1.4.4\r\nConnection: close\r\n\r\n"))
		if r.StatusCode != fx.Response.Status || body != fx.Response.Body {
			t.Errorf("%s: %d %q, real %d %q", m, r.StatusCode, body, fx.Response.Status, fx.Response.Body)
		}
		if got := r.Header.Get("Content-Type"); got != fx.Response.Headers["content-type"] {
			t.Errorf("%s: content-type %q", m, got)
		}
		if r.ContentLength != 20 || r.Header.Get("request-id") != "" {
			t.Errorf("%s: content-length %d, request-id %q", m, r.ContentLength, r.Header.Get("request-id"))
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		r, body := rawDo(t, addr, m, []byte(m+" /api/hello HTTP/1.1\r\nHost: wwz\r\nx-api-key: sk-ant-api03-bad\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		if r.StatusCode != 200 || body != `{"message": "hello"}` {
			t.Errorf("%s: %d %q", m, r.StatusCode, body)
		}
	}
	r, body := rawDo(t, addr, "GET", []byte("GET /api/other HTTP/1.1\r\nHost: wwz\r\nConnection: close\r\n\r\n"))
	if r.StatusCode != 404 || body != "" {
		t.Errorf("/api/other: %d %q", r.StatusCode, body)
	}

	// The driver alone: a public KindResource route on every method the
	// real answers, served by Extra; no longer a route miss (task 449).
	d := newDriver(t)
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		route := driver.Route{Method: m, Path: "/api/hello", Kind: driver.KindResource}
		found := false
		for _, r := range d.Routes() {
			found = found || r == route
		}
		if !found || !driver.IsPublicRoute(d, route) {
			t.Errorf("%s /api/hello: declared %v, public %v", m, found, driver.IsPublicRoute(d, route))
		}
		h := driver.MapHeaders{}
		driver.SetRoute(h, m, "/api/hello")
		w := core.NewMemWriter()
		d.Extra(driver.KindResource, w, h, nil, driver.ExtraContext{})
		if w.StatusCode() != 200 || string(w.Body()) != `{"message": "hello"}` || w.Headers()["Content-Length"] != "20" {
			t.Errorf("Extra %s: %d %q %v", m, w.StatusCode(), w.Body(), w.Headers())
		}
	}
	// Another resource path is no route of the driver: Extra's 404.
	h := driver.MapHeaders{}
	driver.SetRoute(h, "GET", "/api/other")
	w := core.NewMemWriter()
	d.Extra(driver.KindResource, w, h, nil, driver.ExtraContext{})
	if w.StatusCode() != 404 {
		t.Errorf("Extra /api/other: %d", w.StatusCode())
	}
}

// startWWZEngine is startWWZ that also returns the engine, for its metrics.
func startWWZEngine(t *testing.T, cfg string) (string, *engine.Engine) {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets), log)
	srv, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return ln.Addr().String(), eng
}

// TestAPIHelloMetrics: the Claude Code capture HEAD /api/hello
// (testdata/real/claude-code/2.1.308/001-api-hello-head.raw) sent raw gets
// the 200 with client_verification off and claude-code (the probe is never
// verified, ADR 0074, item 7) and counts in wwz_requests_total as a 200 of
// the resource endpoint, not a route_miss 404 (task 449); a bad key or none
// changes nothing.
func TestAPIHelloMetrics(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "claude-code", "2.1.308", "001-api-hello-head.raw"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n"))
	raw = []byte(strings.Replace(string(raw), "Connection: keep-alive", "Connection: close", 1))
	for _, verify := range []string{"off", "claude-code"} {
		t.Run(verify, func(t *testing.T) {
			cfg := strings.Replace(ccConfig, "drivers: [{name: anthropic}]", "drivers: [{name: anthropic, options: {client_verification: "+verify+"}}]", 1)
			addr, eng := startWWZEngine(t, cfg)
			r, body := rawDo(t, addr, "HEAD", raw)
			if r.StatusCode != 200 || body != "" || r.ContentLength != 20 || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("HEAD capture: %d %q content-length %d %q", r.StatusCode, body, r.ContentLength, r.Header.Get("Content-Type"))
			}
			r, _ = rawDo(t, addr, "GET", []byte("GET /api/hello HTTP/1.1\r\nHost: wwz\r\nx-api-key: sk-ant-api03-bad\r\nConnection: close\r\n\r\n"))
			if r.StatusCode != 200 {
				t.Errorf("GET with a bad key: %d", r.StatusCode)
			}
			if got := eng.M.Requests.Value("anthropic", "", "resource", "200"); got != 2 {
				t.Errorf(`wwz_requests_total{endpoint="resource",status="200"} = %d, want 2`, got)
			}
			if got := eng.M.Requests.Value("anthropic", "", "route_miss", "404"); got != 0 {
				t.Errorf(`wwz_requests_total{endpoint="route_miss",status="404"} = %d, want 0`, got)
			}
		})
	}
}
