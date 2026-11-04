package openrouter_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// keyFixture is an answer of the key and account routes recorded against
// the real OpenRouter on 2026-10-31 (testdata/real/openrouter/key/, task
// 193, ADR 0051 G). The key had no credit limit and the account had
// purchased credits; the label, the user and workspace ids and the dollar
// figures are markers.
type keyFixture struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int               `json:"status"`
		ContentType string            `json:"content_type"`
		Headers     map[string]string `json:"headers"`
		Body        string            `json:"body"`
	} `json:"response"`
}

// The equivalent of the recorded key: one key, no key limit, an account
// with credits.
const keyFixtureCfg = `
server: {seed: 42}
accounts:
  - {id: acct-real, limits: {credits_usd: 28.34}}
keys:
  - {key: sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef, label: real, account: acct-real}
`

const keyFixtureKey = "sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var (
	fxLabel     = regexp.MustCompile(`"label":"[^"]*"`)
	fxCreator   = regexp.MustCompile(`"creator_user_id":"[^"]*"`)
	fxWorkspace = regexp.MustCompile(`"workspace_id":"[^"]*"`)
	fxUSD       = regexp.MustCompile(`"(usage|usage_daily|usage_weekly|usage_monthly|total_credits|total_usage)":[0-9.e+-]+`)
)

func normalizeKeyBody(b string) string {
	b = fxLabel.ReplaceAllString(b, `"label":"<label>"`)
	b = fxCreator.ReplaceAllString(b, `"creator_user_id":"<user_id>"`)
	b = fxWorkspace.ReplaceAllString(b, `"workspace_id":"<workspace_id>"`)
	return fxUSD.ReplaceAllString(b, `"$1":"<usd>"`)
}

// TestKeyRoutesMatchRealFixtures: the key-info, key-management and credits
// routes answer as the real API (status, content type, body byte for byte).
func TestKeyRoutesMatchRealFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "key", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	cfg, err := config.Parse([]byte(keyFixtureCfg))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(100), nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx keyFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fx.Name == "" || fx.Response.Status == 0 || fx.Response.Body == "" {
			t.Fatalf("%s: incomplete fixture", f)
		}
		t.Run(fx.Name, func(t *testing.T) {
			hdr := driver.MapHeaders{}
			for k, v := range fx.Request.Headers {
				switch v {
				case "Bearer <key>":
					v = "Bearer " + keyFixtureKey
				case "Bearer <wellformed_unknown_key>":
					v = "Bearer sk-or-v1-" + strings.Repeat("0", 64)
				}
				hdr[k] = v
			}
			kind, param, ok := keyRouteOf(drv, fx.Request.Method, fx.Request.Path)
			if !ok {
				t.Fatalf("no route for %s %s", fx.Request.Method, fx.Request.Path)
			}
			in := engine.Input{
				Driver: drv, Kind: kind, Method: fx.Request.Method, Path: fx.Request.Path, PathParam: param,
				Headers: hdr, Body: []byte(fx.Request.Body), ClientIP: "127.0.0.1",
			}
			w := core.NewMemWriter()
			e.Handle(context.Background(), in, w)
			got := normalizeKeyBody(string(w.Body()))
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d: %s", w.StatusCode(), fx.Response.Status, got)
			}
			if ct := contentType(w); ct != fx.Response.ContentType {
				t.Errorf("content type %q, real %q", ct, fx.Response.ContentType)
			}
			if got != fx.Response.Body {
				t.Errorf("body\n wwz  %s\n real %s", got, fx.Response.Body)
			}
			for _, h := range []string{"cache-control", "access-control-expose-headers", "access-control-allow-origin"} {
				want, ok := fx.Response.Headers[h]
				if !ok {
					continue
				}
				if v := headerOf(w, h); v != want {
					t.Errorf("header %s %q, real %q", h, v, want)
				}
			}
		})
	}
}

// TestKeyFixturesAreNormalized: no fixture carries a real key, label, user
// or workspace id.
func TestKeyFixturesAreNormalized(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "key", "*.json"))
	secret := regexp.MustCompile(`sk-or-v1-[0-9a-f]{3}|user_[A-Za-z0-9]{20,}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := secret.Find(raw); m != nil {
			t.Errorf("%s: not normalized: %s", f, m)
		}
	}
}

func keyRouteOf(drv driver.Driver, method, path string) (driver.Kind, string, bool) {
	for _, r := range drv.Routes() {
		if r.Method != method {
			continue
		}
		if prefix, ok := strings.CutSuffix(r.Path, "*"); ok {
			if rest, ok := strings.CutPrefix(path, prefix); ok {
				return r.Kind, rest, true
			}
			continue
		}
		if r.Path == path {
			return r.Kind, "", true
		}
	}
	return "", "", false
}

func headerOf(w *core.MemWriter, name string) string {
	for k, v := range w.Headers() {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
