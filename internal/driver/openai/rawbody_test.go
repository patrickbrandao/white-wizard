package openai_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

// TestWriteErrorRawBody: a chaos/next raw body replaces the OpenAI envelope
// byte for byte, keeping status, rate-limit headers and retry-after, with
// and without streaming (error before the first byte).
func TestWriteErrorRawBody(t *testing.T) {
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	rl := &core.RateLimitState{Present: true, RequestsLimit: 10, RequestsRemaining: 0, RequestsReset: time.Second}
	cases := []struct {
		name    string
		req     *core.Request
		e       *core.Error
		status  int
		ct      string
		headers []string
	}{
		{
			name:   "json non-stream",
			req:    &core.Request{Endpoint: core.EndpointChat, Model: "m"},
			e:      &core.Error{Kind: core.ErrServerError, Status: 500, Extra: map[string]any{core.ExtraRawBody: `{"custom":  true}`}},
			status: 500, ct: core.RawBodyJSON,
		},
		{
			name:   "text stream",
			req:    &core.Request{Endpoint: core.EndpointChat, Model: "m", Stream: true},
			e:      &core.Error{Kind: core.ErrBadGateway, Status: 502, Extra: map[string]any{core.ExtraRawBody: "<html>bad gateway</html>\n"}},
			status: 502, ct: core.RawBodyText,
		},
		{
			name:   "rate limit keeps headers",
			req:    &core.Request{Endpoint: core.EndpointChat, Model: "m"},
			e:      &core.Error{Kind: core.ErrRateLimit, Status: 429, RetryAfter: 3 * time.Second, Extra: map[string]any{core.ExtraRawBody: "slow down"}},
			status: 429, ct: core.RawBodyText,
			headers: []string{"retry-after", "x-ratelimit-limit-requests", "x-ratelimit-remaining-requests"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := core.NewMemWriter()
			d.WriteError(w, tc.req, tc.e, rl)
			want, _ := tc.e.Extra[core.ExtraRawBody].(string)
			if got := string(w.Body()); got != want {
				t.Fatalf("body %q, want %q", got, want)
			}
			if w.StatusCode() != tc.status {
				t.Fatalf("status %d, want %d", w.StatusCode(), tc.status)
			}
			h := w.Headers()
			if h["Content-Type"] != tc.ct {
				t.Fatalf("content-type %q, want %q", h["Content-Type"], tc.ct)
			}
			for _, k := range tc.headers {
				if h[k] == "" {
					t.Fatalf("missing header %s: %v", k, h)
				}
			}
		})
	}
}
