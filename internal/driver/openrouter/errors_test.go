package openrouter_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestWriteErrorKeepsCallerExtra: the X-RateLimit-* headers of a 429 are
// added to a copy; the caller's Error (shared by retries, chaos rules and
// the engine) keeps its Extra untouched, nil or not (task 030).
func TestWriteErrorKeepsCallerExtra(t *testing.T) {
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	rl := &core.RateLimitState{Present: true, RequestsLimit: 10, RequestsRemaining: 0, RequestsReset: time.Second}
	req := &core.Request{Endpoint: core.EndpointChat, Model: "m"}
	for _, extra := range []map[string]any{nil, {"limit": "rpm"}} {
		e := &core.Error{Kind: core.ErrRateLimit, RetryAfter: time.Second, Extra: extra}
		w := core.NewMemWriter()
		d.WriteError(w, req, e, rl)
		if w.Headers()["X-RateLimit-Limit"] != "10" {
			t.Errorf("X-RateLimit-Limit = %q, want 10", w.Headers()["X-RateLimit-Limit"])
		}
		if extra == nil && e.Extra != nil {
			t.Errorf("nil Extra replaced: %v", e.Extra)
		}
		if extra != nil && len(e.Extra) != 1 {
			t.Errorf("caller Extra mutated: %v", e.Extra)
		}
	}
}
