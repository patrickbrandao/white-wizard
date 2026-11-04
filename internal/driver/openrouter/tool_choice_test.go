package openrouter_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// reUpstreamReq is the request id of Anthropic inside metadata.raw.
var reUpstreamReq = regexp.MustCompile(`req_011C[A-Za-z0-9]+`)

// TestRealFixturesToolChoice replays testdata/real/openrouter/tool-choice/
// (recorded on 2026-10-31, task 342, ADR 0014) through the driver: the
// requests the gateway accepts parse (tool_choice dropped without tools,
// extra keys ignored); its own validation of the type is the real 400 byte
// for byte; a refusal of Anthropic is relayed with the real envelope and
// metadata.raw, from WhiteWizard and without previous_errors (the real
// lists the providers it fell back from, which the fake does not have).
func TestRealFixturesToolChoice(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "tool-choice", "*.json"))
	if err != nil || len(files) < 9 {
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
			hdr := driver.MapHeaders{}
			for k, v := range fx.Request.Headers {
				hdr[k] = v
			}
			req, perr := d.ParseRequest(driver.KindMessages, []byte(fx.Request.Body), hdr)
			if fx.Response.Status == 200 {
				if perr != nil {
					t.Fatalf("refused: %s", perr.Message)
				}
				if len(req.Tools) == 0 && req.ToolChoice.Mode != core.ToolChoiceAuto {
					t.Errorf("tool_choice kept without tools: %+v", req.ToolChoice)
				}
				return
			}
			if perr == nil {
				t.Fatalf("accepted, real %d", fx.Response.Status)
			}
			if req == nil {
				req = &core.Request{Driver: "openrouter", Endpoint: core.EndpointMessages, Headers: hdr}
			}
			w := core.NewMemWriter()
			d.WriteError(w, req, perr, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			got := reUpstreamReq.ReplaceAllString(normalize(string(w.Body())), "<request_id>")
			want := normalize(fx.Response.Body)
			if i := strings.Index(want, `,"provider_name":`); i > 0 && strings.Contains(want, `"previous_errors"`) {
				want = want[:i] + `,"provider_name":"WhiteWizard","is_byok":false}}`
			}
			if got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}
