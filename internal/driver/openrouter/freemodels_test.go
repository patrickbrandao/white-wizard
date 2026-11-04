package openrouter_test

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestFreeModelsPerMinuteForm (task 262): the per-minute cap of the
// free-models unit answers the real form, with or without the rate-limit
// state; the per-day cap and a plain rpm denial keep the generic one.
func TestFreeModelsPerMinuteForm(t *testing.T) {
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &core.Request{Endpoint: core.EndpointChat, Model: "x/y:free", RequestID: "gen-1"}
	for _, rl := range []*core.RateLimitState{nil, {Present: true, RequestsLimit: 20, RequestsRemaining: 0, RequestsReset: 30 * time.Second}} {
		w := core.NewMemWriter()
		d.WriteError(w, req, &core.Error{Kind: core.ErrRateLimit, RetryAfter: time.Second, Extra: map[string]any{"limit": "rpm", "free_models": true}}, rl)
		body := string(w.Body())
		if w.StatusCode() != 429 || !strings.Contains(body, `"message":"Rate limit exceeded: free-models-per-min. "`) ||
			!strings.Contains(body, `"limit_source":"openrouter_free_tier_per_minute"`) || strings.Contains(body, "error_type") {
			t.Errorf("rl %v: %d %s", rl != nil, w.StatusCode(), body)
		}
		if headerOf(w, "x-ratelimit-limit") != "20" || headerOf(w, "x-ratelimit-remaining") != "0" || headerOf(w, "retry-after") != "" {
			t.Errorf("rl %v: headers %v", rl != nil, w.Headers())
		}
		if r := headerOf(w, "x-ratelimit-reset"); !strings.HasSuffix(r, "0000") || !strings.Contains(body, `"X-RateLimit-Reset":"`+r+`"`) {
			t.Errorf("rl %v: reset %q", rl != nil, r)
		}
	}
	for _, extra := range []map[string]any{{"limit": "rpd", "free_models": true}, {"limit": "rpm"}} {
		w := core.NewMemWriter()
		d.WriteError(w, req, &core.Error{Kind: core.ErrRateLimit, Extra: extra}, nil)
		if strings.Contains(string(w.Body()), "openrouter_free_tier_per_minute") {
			t.Errorf("%v: %s", extra, w.Body())
		}
	}
}
