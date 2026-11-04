package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
)

// gatewayRouteFixture is a real route miss of OpenRouter, OpenCode or
// Gemini recorded on 2026-10-31 (task 317): unknown paths, paths outside
// the API prefix and wrong methods, with a valid key (<key>), an invalid
// one (sent as is) or none. The fixtures live one level below routes/
// (routes/miss/) so TestRealFixturesRouteMiss, which replays OpenAI and
// Anthropic with their own header list, does not pick them up.
// Placeholders: <log_id> (x-opencode-log-id), dur=<ms> (server-timing)
// and a body "<html>" for a page of the provider's web site, whose bytes
// change with each deploy.
type gatewayRouteFixture struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Request  struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    *string           `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

// gatewayRouteHeaders are the headers each provider's fixtures keep; the
// wwz answer must carry exactly those of them the real sent.
var gatewayRouteHeaders = map[string][]string{
	"openrouter": {
		"content-type", "cache-control", "access-control-allow-origin", "access-control-expose-headers",
		"access-control-allow-methods", "access-control-allow-headers",
	},
	"opencode": {
		"content-type", "cache-control", "access-control-allow-origin", "access-control-allow-methods",
		"access-control-allow-headers", "x-opencode-log-id",
	},
	"gemini": {"content-type", "server", "server-timing", "x-content-type-options", "x-frame-options", "x-xss-protection"},
}

var (
	reRouteLogID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	reRouteTiming = regexp.MustCompile(`dur=[0-9]+`)
)

// gatewayRouteDivergences are the fixtures the wwz does not reproduce yet;
// each one is a driver task (task 317 follow-ups). None is left.
var gatewayRouteDivergences = map[string]string{}

// gatewayRouteCfg serves the models the gateway fixtures name.
const gatewayRouteCfg = fixtureCfg + `  - {id: gemini-3.5-flash-lite, latency: fast}
  - {id: gemini-3.5-flash, latency: fast}
keys: [{key: wwz-key-01}]
`

// TestRealFixturesRoutes replays testdata/real/{openrouter,opencode,
// gemini}/routes/miss/ with the provider's driver at the root: status,
// the kept headers and the body byte for byte.
func TestRealFixturesRoutes(t *testing.T) {
	for _, provider := range []string{"openrouter", "opencode", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", provider, "routes", "miss", "*.json"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no fixtures: %v", err)
			}
			h := start(t, gatewayRouteCfg+"drivers: [{name: "+provider+"}]\n", nil)
			for _, f := range files {
				fx := loadGatewayRouteFixture(t, f)
				if fx.Provider != provider {
					t.Fatalf("%s: provider %q", f, fx.Provider)
				}
				t.Run(fx.Name, func(t *testing.T) {
					if why, ok := gatewayRouteDivergences[provider+"/"+fx.Name]; ok {
						t.Skip("divergência → tarefa nova: " + why)
					}
					replayGatewayRoute(t, h, fx)
				})
			}
		})
	}
}

func loadGatewayRouteFixture(t *testing.T, f string) gatewayRouteFixture {
	t.Helper()
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx gatewayRouteFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", f, err)
	}
	return fx
}

func replayGatewayRoute(t *testing.T, h *harness, fx gatewayRouteFixture) {
	h.t = t
	defer func(parent *testing.T) { h.t = parent }(h.t)
	hdr := map[string]string{}
	for k, v := range fx.Request.Headers {
		if k == "content-type" {
			continue // h.do sets it with the body
		}
		hdr[k] = strings.ReplaceAll(v, "<key>", "wwz-key-01")
	}
	body := ""
	if fx.Request.Body != nil {
		body = *fx.Request.Body
	}
	resp := h.do(fx.Request.Method, fx.Request.Path, hdr, body)
	got := string(resp.Body())
	if resp.StatusCode() != fx.Response.Status {
		t.Fatalf("status %d, real %d: %.200s", resp.StatusCode(), fx.Response.Status, got)
	}
	switch {
	case fx.Response.Body == "<html>":
		if !strings.HasPrefix(strings.ToLower(got), "<!doctype html") {
			t.Errorf("body %.200q, real a page of the site (<!DOCTYPE html>...)", got)
		}
	case got != fx.Response.Body:
		t.Errorf("body\n wwz  %q\n real %q", got, fx.Response.Body)
	}
	checkGatewayRouteHeaders(t, fx, resp)
}

func checkGatewayRouteHeaders(t *testing.T, fx gatewayRouteFixture, resp *fasthttp.Response) {
	t.Helper()
	sent := strings.ToLower(resp.Header.String())
	for _, k := range gatewayRouteHeaders[fx.Provider] {
		got := string(resp.Header.Peek(k))
		if k == "content-type" && !strings.Contains(sent, "\ncontent-type:") {
			got = "" // the client reports its default when none was sent
		}
		want, ok := fx.Response.Headers[k]
		switch {
		case !ok && got != "":
			t.Errorf("header %s = %q, not in the real answer", k, got)
		case !ok:
		case got == "":
			t.Errorf("header %s missing (real %q)", k, want)
		case want == "<log_id>":
			if !reRouteLogID.MatchString(got) {
				t.Errorf("header %s = %q, not in the real format", k, got)
			}
		case strings.Contains(want, "dur=<ms>"):
			if reRouteTiming.ReplaceAllString(got, "dur=<ms>") != want {
				t.Errorf("header %s = %q, real %q", k, got, want)
			}
		case got != want:
			t.Errorf("header %s = %q, real %q", k, got, want)
		}
	}
}
