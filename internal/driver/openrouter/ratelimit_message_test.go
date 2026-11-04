package openrouter_test

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestRateLimitMessage (task 063): OpenRouter's 429 text names its own
// free-model bucket, or says the model is rate-limited upstream.
func TestRateLimitMessage(t *testing.T) {
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &core.Request{Endpoint: core.EndpointChat, Model: "vendor/model"}
	cases := []struct{ limit, want string }{
		{"rpm", "Rate limit exceeded: free-models-per-min."},
		{"rps", "Rate limit exceeded: free-models-per-min."},
		{"rpd", "Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day"},
		{"tpm", "vendor/model is temporarily rate-limited upstream. Please retry shortly, or add your own key to accumulate your rate limits: https://openrouter.ai/settings/integrations"},
		{"", "Rate limit exceeded"},
	}
	for _, c := range cases {
		var extra map[string]any
		if c.limit != "" {
			extra = map[string]any{"limit": c.limit}
		}
		w := core.NewMemWriter()
		d.WriteError(w, req, &core.Error{Kind: core.ErrRateLimit, Extra: extra}, nil)
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body(), &env); err != nil {
			t.Fatal(err)
		}
		if w.StatusCode() != 429 || env.Error.Message != c.want {
			t.Errorf("limit %q: %d %q, want %q", c.limit, w.StatusCode(), env.Error.Message, c.want)
		}
	}
}
