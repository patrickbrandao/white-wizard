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

// modelFixture is a request about the model field or the single-model
// route recorded against the real OpenRouter (testdata/real/openrouter/
// models/*.json, task 163). Per-call ids are markers; an accepted request
// keeps only the model the real response carried.
type modelFixture struct {
	Name    string `json:"name"`
	Request struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int     `json:"status"`
		ContentType string  `json:"content_type"`
		Body        *string `json:"body"`
		Model       string  `json:"model"`
	} `json:"response"`
}

// The models the fixtures name, by the real slugs.
const fixtureModelsCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: openai/gpt-4o-mini, latency: fast}
  - {id: anthropic/claude-haiku-4.5, latency: fast}
  - {id: nvidia/nemotron-3.5-lightning, latency: fast}
  - {id: "nvidia/nemotron-3.5-lightning:free", latency: fast}
  - {id: "google/gemma-4-31b-it:free", latency: fast}
  - {id: "recursive/flint-v1.0:free", latency: fast}
`

var (
	fxUserID    = regexp.MustCompile(`"user_id":"user_[A-Za-z0-9]+"`)
	fxRequestID = regexp.MustCompile(`"request_id":"(req_[A-Za-z0-9]+|gen-[0-9]+-[A-Za-z0-9]+)"`)
)

func normalizeFixtureBody(b string) string {
	b = fxUserID.ReplaceAllString(b, `"user_id":"<user_id>"`)
	return fxRequestID.ReplaceAllString(b, `"request_id":"<request_id>"`)
}

// TestModelFieldMatchesRealFixtures: the model field and the single-model
// route answer as the real API (status, content type, body byte for byte;
// the model of an accepted request).
func TestModelFieldMatchesRealFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "models", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	cfg, err := config.Parse([]byte(fixtureModelsCfg))
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
		var fx modelFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(fx.Name, func(t *testing.T) {
			kind, param := routeOf(t, drv, fx.Request.Method, fx.Request.Path)
			in := engine.Input{
				Driver: drv, Kind: kind, Method: fx.Request.Method, Path: fx.Request.Path, PathParam: param,
				Headers: driver.MapHeaders{"authorization": "Bearer wwz-key-01"}, Body: []byte(fx.Request.Body), ClientIP: "127.0.0.1",
			}
			w := core.NewMemWriter()
			e.Handle(context.Background(), in, w)
			got := normalizeFixtureBody(string(w.Body()))
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d: %s", w.StatusCode(), fx.Response.Status, got)
			}
			if ct := contentType(w); ct != fx.Response.ContentType {
				t.Errorf("content type %q, real %q", ct, fx.Response.ContentType)
			}
			if fx.Response.Body != nil {
				if got != *fx.Response.Body {
					t.Errorf("body\n wwz  %s\n real %s", got, *fx.Response.Body)
				}
				return
			}
			var resp struct {
				Model string
				Data  struct{ ID string }
			}
			body := w.Body()
			if fx.Response.ContentType == "text/event-stream" {
				// The model of the first chunk (task 186).
				_, chunk, _ := strings.Cut(string(body), "data: ")
				chunk, _, _ = strings.Cut(chunk, "\n")
				body = []byte(chunk)
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("%v: %s", err, w.Body())
			}
			if m := resp.Model + resp.Data.ID; m != fx.Response.Model {
				t.Errorf("model %q, real %q", m, fx.Response.Model)
			}
		})
	}
}

// routeOf is the driver's route for method and path, and the path
// parameter a wildcard route captures.
func routeOf(t *testing.T, drv driver.Driver, method, path string) (driver.Kind, string) {
	t.Helper()
	for _, r := range drv.Routes() {
		if r.Method != method {
			continue
		}
		if prefix, ok := strings.CutSuffix(r.Path, "*"); ok {
			if rest, ok := strings.CutPrefix(path, prefix); ok {
				return r.Kind, rest
			}
			continue
		}
		if r.Path == path {
			return r.Kind, ""
		}
	}
	t.Fatalf("no route for %s %s", method, path)
	return "", ""
}

func contentType(w *core.MemWriter) string {
	for k, v := range w.Headers() {
		if strings.EqualFold(k, "content-type") {
			mt, _, _ := strings.Cut(v, ";")
			return strings.TrimSpace(mt)
		}
	}
	return ""
}
