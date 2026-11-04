package server

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Origin of the URLs of wwz itself (ADR 0073, spec 004 "URLs do próprio
// wwz"): the server computes <scheme>://<host><external prefix><mount
// prefix> after routing and hands it to the driver in driver.OriginHeader.

const originModels = `
models:
  - {id: deepseek-v4.1-flash, protocols: [openai], drivers: [opencode], latency: fast}
`

// originCfg mounts the opencode driver at mount ("" = the root), with
// 10.0.0.0/8 as the only trusted proxies.
func originCfg(server, mount string) string {
	d := "drivers:\n  - {name: opencode}\n"
	if mount != "" {
		d = "drivers:\n  - {name: opencode, prefix: " + mount + "}\n"
	}
	return "server: {seed: 42, trusted_proxies: [10.0.0.0/8]" + server + "}\n" + latCfg + originModels + d
}

var (
	trustedPeer   = &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 4711}
	untrustedPeer = &net.TCPAddr{IP: net.ParseIP("45.80.10.20"), Port: 4711}
	forwarded     = map[string]string{
		"X-Forwarded-Proto":  "https, http",
		"X-Forwarded-Host":   "wwz.exemplo, proxy.interno",
		"X-Forwarded-Prefix": "/edge/, /outro",
	}
)

// openaiBaseURL is api["chat-completions"].base_url of the first model of
// a listing body.
func openaiBaseURL(t *testing.T, body []byte) string {
	t.Helper()
	var list struct {
		Data []struct {
			API map[string]struct {
				BaseURL string `json:"base_url"`
			} `json:"api"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Data) == 0 {
		t.Fatalf("listing %v: %s", err, body)
	}
	return list.Data[0].API["chat-completions"].BaseURL
}

func TestOriginOfTheListing(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	for _, c := range []struct {
		name, mount string
		peer        net.Addr
		hdr         map[string]string
		want        string
	}{
		{"no proxy", "/opencode", untrustedPeer, nil, "http://wwz/opencode/inference/go/openai/v1"},
		{"trusted proxy", "/opencode/", trustedPeer, forwarded, "https://wwz.exemplo/edge/opencode/inference/go/openai/v1"},
		{"untrusted peer: x-forwarded-* ignored", "/opencode", untrustedPeer, forwarded, "http://wwz/opencode/inference/go/openai/v1"},
		{"trusted proxy, bad proto and prefix", "/opencode", trustedPeer, map[string]string{
			"X-Forwarded-Proto": "gopher", "X-Forwarded-Prefix": "edge", "X-Forwarded-Host": "",
		}, "http://wwz/opencode/inference/go/openai/v1"},
		{"root mount", "", untrustedPeer, nil, "http://wwz/inference/go/openai/v1"},
		{"root mount, trusted proxy", "", trustedPeer, forwarded, "https://wwz.exemplo/edge/inference/go/openai/v1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, originCfg("", c.mount), c.peer)
			resp := h.do("GET", strings.TrimSuffix(c.mount, "/")+"/inference/go/v1/models", c.hdr, "")
			if resp.StatusCode() != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
			}
			if got := openaiBaseURL(t, resp.Body()); got != c.want {
				t.Errorf("base_url %q, want %q", got, c.want)
			}
		})
	}
}

// TestOriginOverTLS: with server.tls the scheme is https, in the listing
// and in the Console's verification_uri_complete.
func TestOriginOverTLS(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	_, base := serveTLS(t, originCfg(", tls: {self_signed: true}", "/opencode"))
	cl := tlsClient(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test server
	get := func(method, path, body string) []byte {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return b
	}
	if got, want := openaiBaseURL(t, get("GET", "/opencode/inference/go/v1/models", "")), base+"/opencode/inference/go/openai/v1"; got != want {
		t.Errorf("base_url %q, want %q", got, want)
	}
	var dc struct {
		User string `json:"user_code"`
		URI  string `json:"verification_uri_complete"`
	}
	if err := json.Unmarshal(get("POST", "/opencode/console/auth/device/code", `{"client_id":"opencode-cli"}`), &dc); err != nil {
		t.Fatal(err)
	}
	if want := base + "/opencode/console/device?code=" + dc.User; dc.URI != want || dc.User == "" {
		t.Errorf("verification_uri_complete %q, want %q", dc.URI, want)
	}
}

// TestOriginOfTheConsoleBehindAProxy: the Console follows the same origin.
func TestOriginOfTheConsoleBehindAProxy(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	h := start(t, originCfg("", "/opencode"), trustedPeer)
	resp := h.do("POST", "/opencode/console/auth/device/code", forwarded, `{"client_id":"opencode-cli"}`)
	var dc struct {
		User string `json:"user_code"`
		URI  string `json:"verification_uri_complete"`
	}
	if err := json.Unmarshal(resp.Body(), &dc); err != nil || resp.StatusCode() != 200 {
		t.Fatalf("%d %v: %s", resp.StatusCode(), err, resp.Body())
	}
	if want := "https://wwz.exemplo/edge/opencode/console/device?code=" + dc.User; dc.URI != want {
		t.Errorf("verification_uri_complete %q, want %q", dc.URI, want)
	}
}

// TestOriginNotForged: a client's own driver.OriginHeader never reaches
// the driver, the server always overwrites it.
func TestOriginNotForged(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	s, err := New(newEngine(t, originCfg("", "/opencode")), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	var req fasthttp.Request
	req.Header.SetMethod("GET")
	req.SetRequestURI("http://wwz/opencode/inference/go/v1/models")
	req.Header.Set(driver.OriginHeader, "https://evil.example")
	if len(req.Header.Peek(driver.OriginHeader)) == 0 {
		t.Fatal("the forged header is not in the request")
	}
	var ctx fasthttp.RequestCtx
	ctx.Init(&req, untrustedPeer, nil)
	s.Handle(&ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if got := openaiBaseURL(t, ctx.Response.Body()); got != "http://wwz/opencode/inference/go/openai/v1" {
		t.Errorf("base_url %q", got)
	}
}

// missOriginDriver is the opencode driver whose route miss echoes the
// origin it got.
type missOriginDriver struct{ driver.Driver }

func (missOriginDriver) WriteRouteMiss(w core.ResponseWriter, req *core.Request, _ driver.RouteMiss, _ *core.Error) {
	w.Status(404)
	_, _ = w.Write([]byte(driver.Origin(req)))
}

func init() {
	driver.Register("wwz-test-miss-origin", func(opts map[string]string) (driver.Driver, error) {
		d, err := driver.New("opencode", opts)
		if err != nil {
			return nil, err
		}
		return missOriginDriver{d}, nil
	})
}

// TestOriginOfARouteMiss: the mount prefix is in the origin of a route
// miss too; without a host the origin is only the prefixes.
func TestOriginOfARouteMiss(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	cfg := "server: {seed: 42, trusted_proxies: [10.0.0.0/8]}\n" + latCfg + "drivers:\n  - {name: wwz-test-miss-origin, prefix: /oc}\n"
	h := start(t, cfg, trustedPeer)
	resp := h.do("GET", "/oc/nao/existe", map[string]string{"X-Forwarded-Prefix": "/edge"}, "")
	if resp.StatusCode() != 404 || string(resp.Body()) != "http://wwz/edge/oc" {
		t.Errorf("miss: %d %q", resp.StatusCode(), resp.Body())
	}

	s, err := New(newEngine(t, cfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	var req fasthttp.Request
	req.Header.SetMethod("GET")
	req.SetRequestURI("/oc/nao/existe")
	var ctx fasthttp.RequestCtx
	ctx.Init(&req, untrustedPeer, nil)
	s.Handle(&ctx)
	if string(ctx.Response.Body()) != "/oc" {
		t.Errorf("no host: %q", ctx.Response.Body())
	}
}
