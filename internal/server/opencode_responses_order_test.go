package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestOpenCodeResponsesModelFirst: on OpenCode Go's Responses route the
// real refuses an unknown model before the portable route's checks
// (order_unknown_model_vs_unknown_field, task 267), but after the body that
// is not JSON and the model that is not a string (testdata/real/opencode/
// responses/, recorded on 2026-10-31).
func TestOpenCodeResponsesModelFirst(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: deepseek-v4.1-flash, latency: fast}
drivers: [{name: opencode}]
`, nil)
	auth := map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}
	for _, name := range []string{
		"order_unknown_model_vs_unknown_field", "unknown_model", "order_store_before_unknown",
		"invalid_json", "missing_model", "order_unknown_before_missing_input",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "opencode", "responses", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var fx struct {
				Request struct {
					Path string `json:"path"`
					Body string `json:"body"`
				} `json:"request"`
				Response struct {
					Status int    `json:"status"`
					Body   string `json:"body"`
				} `json:"response"`
			}
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			resp := h.do("POST", fx.Request.Path, auth, fx.Request.Body)
			if resp.StatusCode() != fx.Response.Status || string(resp.Body()) != fx.Response.Body {
				t.Errorf("%d %s\nreal %d %s", resp.StatusCode(), resp.Body(), fx.Response.Status, fx.Response.Body)
			}
			if n := h.eng.M.Requests.Value("opencode", "", "responses", "400") + h.eng.M.Requests.Value("opencode", "", "responses", "503"); n == 0 {
				t.Errorf("not counted under endpoint responses")
			}
		})
	}
	// Only the Chat and Responses routes are checked by the validator; a
	// valid model passes to the driver.
	if err := validateAs(t, "opencode", nil, driver.KindResponses, `{"model":"x","input":"hi"}`, nil); err != nil {
		t.Errorf("no configuration: %+v", err)
	}
}
