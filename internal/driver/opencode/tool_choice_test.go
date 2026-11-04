package opencode_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestRealFixturesToolChoice replays testdata/real/opencode/tool-choice/
// (recorded on 2026-10-31 with claude-haiku-5-5, task 342, ADR 0014)
// through the driver: the gateway relays every refusal of Anthropic
// (missing or unknown type and keys a type does not accept included)
// behind "Upstream request failed: [invalid_request_error] ", byte for
// byte, with x-zen-model; the accepted request parses. The refusals are
// raised after the model is resolved (driver.AfterModel).
func TestRealFixturesToolChoice(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "tool-choice", "*.json"))
	if err != nil || len(files) < 7 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	d := newDriver(t)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx shapeFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(fx.Name, func(t *testing.T) {
			hdr := driver.MapHeaders{"x-wwz-path": fx.Request.Path}
			for k, v := range fx.Request.Headers {
				hdr[k] = v
			}
			_, perr := d.ParseRequest(driver.KindMessages, []byte(fx.Request.Body), hdr)
			if fx.Response.Status == 200 {
				if perr != nil {
					t.Fatalf("refused: %s", perr.Message)
				}
				return
			}
			if perr == nil {
				t.Fatalf("accepted, real %d", fx.Response.Status)
			}
			if m, ok := driver.AfterModel(perr); !ok || m != "claude-haiku-5-5" {
				t.Errorf("after model = %q %v", m, ok)
			}
			// What the engine passes on a parse error: the headers only.
			bare := &core.Request{Endpoint: core.EndpointMessages, Headers: hdr}
			w := core.NewMemWriter()
			d.WriteError(w, bare, perr, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got := string(w.Body()); got != fx.Response.Body {
				t.Errorf("body\n wwz  %s\n real %s", got, fx.Response.Body)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}
