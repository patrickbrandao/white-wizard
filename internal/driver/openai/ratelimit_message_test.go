package openai_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// TestRateLimitMessage (task 063): OpenAI's 429 text names the limit window
// and quotes Limit/Used/Requested when the denial carries them.
func TestRateLimitMessage(t *testing.T) {
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	const visit = " Visit https://platform.openai.com/account/rate-limits to learn more."
	cases := []struct {
		name string
		e    core.Error
		want string
	}{
		{
			"rpm",
			core.Error{Kind: core.ErrRateLimit, RetryAfter: 20 * time.Second, Extra: map[string]any{"limit": "rpm", "limit_value": 3, "limit_used": 3, "limit_requested": 1}},
			"Rate limit reached for gpt-x in organization org-wwz on requests per min (RPM): Limit 3, Used 3, Requested 1. Please try again in 20s." + visit,
		},
		{
			"tpm",
			core.Error{Kind: core.ErrRateLimit, RetryAfter: 6 * time.Millisecond, Extra: map[string]any{"limit": "tpm", "limit_value": 10000, "limit_used": 9000, "limit_requested": 1500}},
			"Rate limit reached for gpt-x in organization org-wwz on tokens per min (TPM): Limit 10000, Used 9000, Requested 1500. Please try again in 6ms." + visit,
		},
		{
			"rpd",
			core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "rpd"}},
			"Rate limit reached for gpt-x in organization org-wwz on requests per day (RPD)." + visit,
		},
		{
			"tpd",
			core.Error{Kind: core.ErrRateLimit, Extra: map[string]any{"limit": "tpd"}},
			"Rate limit reached for gpt-x in organization org-wwz on tokens per day (TPD)." + visit,
		},
		{
			"chaos",
			core.Error{Kind: core.ErrRateLimit},
			"Rate limit reached for gpt-x in organization org-wwz on requests per min (RPM)." + visit,
		},
	}
	req := &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x"}
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
			t.Errorf("%s: %d %q\nwant %q", c.name, w.StatusCode(), env.Error.Message, c.want)
		}
	}

	// A key of an account names the account's organization id (task 201).
	h := driver.MapHeaders{"authorization": "Bearer sk-a"}
	driver.SetAccount(h, driver.Account{Key: "sk-a", Account: "Acme", Project: "web"})
	w := core.NewMemWriter()
	d.WriteError(w, &core.Request{Endpoint: core.EndpointChat, Model: "gpt-x", Headers: h}, &core.Error{Kind: core.ErrRateLimit}, nil)
	want := "Rate limit reached for gpt-x in organization " + oai.OrganizationID(&core.Request{Headers: h}) + " on requests per min (RPM)." + visit
	if !strings.Contains(string(w.Body()), want) || strings.Contains(string(w.Body()), "org-wwz") {
		t.Errorf("account 429: %s", w.Body())
	}
}
