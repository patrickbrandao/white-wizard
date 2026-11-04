package anthropic_test

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
)

// TestRateLimitMessage (task 063): Anthropic's 429 text quotes the exceeded
// limit with thousands separators; without figures it is the generic text.
func TestRateLimitMessage(t *testing.T) {
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The organization is the account's, the same as anthropic-organization-id.
	req := &core.Request{Endpoint: core.EndpointMessages, Model: "m"}
	head := "This request would exceed the rate limit for your organization (" + anth.OrganizationID(req) + ")"
	const (
		tail = ". For details, refer to: https://docs.claude.com/en/api/rate-limits. You can see the response headers for current usage. Please reduce the prompt length or the maximum tokens requested, or try again later. You may also contact sales at https://www.anthropic.com/contact-sales to discuss your options for a rate limit increase."
	)
	nums := func(limit string, n int) map[string]any {
		return map[string]any{"limit": limit, "limit_value": n, "limit_used": n, "limit_requested": 1}
	}
	cases := []struct {
		name string
		e    core.Error
		want string
	}{
		{"rpm", core.Error{Kind: core.ErrRateLimit, Extra: nums("rpm", 50)}, head + " of 50 requests per minute" + tail},
		{"tpm", core.Error{Kind: core.ErrRateLimit, Extra: nums("tpm", 1234567)}, head + " of 1,234,567 input tokens per minute" + tail},
		{"itpm", core.Error{Kind: core.ErrRateLimit, Extra: nums("itpm", 40000)}, head + " of 40,000 input tokens per minute" + tail},
		{"otpm", core.Error{Kind: core.ErrRateLimit, Extra: nums("otpm", 8000)}, head + " of 8,000 output tokens per minute" + tail},
		{"no_figures", core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "rps"}}, head + tail},
		{"slow_down", core.Error{Kind: core.ErrSlowDown, Extra: nums("rpm", 50)}, head + tail},
		{"chaos", core.Error{Kind: core.ErrRateLimit}, head + tail},
	}
	for _, c := range cases {
		w := core.NewMemWriter()
		d.WriteError(w, req, &c.e, nil)
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body(), &env); err != nil {
			t.Fatal(err)
		}
		if w.StatusCode() != 429 || env.Error.Message != c.want {
			t.Errorf("%s: %d %q", c.name, w.StatusCode(), env.Error.Message)
		}
		if got := w.Headers()["anthropic-organization-id"]; got != anth.OrganizationID(req) {
			t.Errorf("%s: anthropic-organization-id %q, the text names %q", c.name, got, anth.OrganizationID(req))
		}
	}
}
