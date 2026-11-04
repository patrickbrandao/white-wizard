package opencode_test

import (
	"fmt"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 396: the Responses usage reports the reasoning (the wwz-drift
// recording in shape/) and Vertex refuses thinkingBudget on the gateway's
// Gemini model (google/gen_thinking*.json).

// TestRealFixturesGoogleThinkingBudget: on gemini-4-argon any
// thinkingBudget (128, 0, -1) is Vertex's 400 with x-zen-model, and with a
// thinkingLevel the pair's text; thinkingLevel alone passes.
func TestRealFixturesGoogleThinkingBudget(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"gen_thinking", "gen_thinking_budget_zero", "gen_thinking_budget_dynamic", "gen_thinking_budget_level"} {
		t.Run(name, func(t *testing.T) {
			fx := loadGoogle(t, name)
			h := googleReqHeaders(fx)
			req, perr := d.ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), h)
			if perr == nil {
				t.Fatalf("accepted: %+v", req)
			}
			if perr.Status != fx.Response.Status {
				t.Errorf("error status %d, real %d", perr.Status, fx.Response.Status)
			}
			w := core.NewMemWriter()
			d.WriteError(w, &core.Request{Headers: h}, perr, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, want := string(w.Body()), fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkGoogleHeaders(t, fx, w.Headers())
		})
	}
	// thinkingLevel alone is served (gen_thinking_level).
	googleRequest(t, d, loadGoogle(t, "gen_thinking_level"))
}

// TestGoogleThinkingBudgetForms: snake_case keys count, a null budget is
// absent, a key-less request goes on to the engine's 401 and the
// translated models take the budget.
func TestGoogleThinkingBudgetForms(t *testing.T) {
	d := newDriver(t)
	const contents = `"contents":[{"role":"user","parts":[{"text":"oi"}]}]`
	path := "/inference/go/google/v1beta/models/%s:generateContent"
	cases := []struct {
		name, model, body string
		key, refused      bool
	}{
		{"snake", "gemini-4-argon", `{` + contents + `,"generation_config":{"thinking_config":{"thinking_budget":64}}}`, true, true},
		{"snake level", "gemini-4-argon", `{` + contents + `,"generationConfig":{"thinkingConfig":{"thinkingBudget":64,"thinking_level":"LOW"}}}`, true, true},
		{"null budget", "gemini-4-argon", `{` + contents + `,"generationConfig":{"thinkingConfig":{"thinkingBudget":null}}}`, true, false},
		{"no key", "gemini-4-argon", `{` + contents + `,"generationConfig":{"thinkingConfig":{"thinkingBudget":64}}}`, false, false},
		{"translated", "deepseek-v4.1-flash", `{` + contents + `,"generationConfig":{"thinkingConfig":{"thinkingBudget":64}}}`, true, false},
		{"other gemini", "gemini-m-1", `{` + contents + `,"generationConfig":{"thinkingConfig":{"thinkingBudget":64}}}`, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := driver.MapHeaders{"x-wwz-path": fmt.Sprintf(path, c.model), "x-opencode-session": "s"}
			if c.key {
				h["x-goog-api-key"] = "oc_sk_x"
			}
			_, perr := d.ParseRequest(driver.KindGenerate, []byte(c.body), h)
			if (perr != nil) != c.refused {
				t.Errorf("refused %v, want %v (%+v)", perr != nil, c.refused, perr)
			}
		})
	}
}
