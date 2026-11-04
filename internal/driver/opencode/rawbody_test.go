package opencode_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// TestWriteErrorRawBody: a chaos/next raw body replaces the OpenCode
// envelope (OpenAI and Anthropic dialects) byte for byte, keeping status and
// retry-after, with and without streaming.
func TestWriteErrorRawBody(t *testing.T) {
	d, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		req     *core.Request
		e       *core.Error
		status  int
		ct      string
		headers []string
	}{
		{
			name:   "chat json non-stream",
			req:    &core.Request{Endpoint: core.EndpointChat, Model: "m"},
			e:      &core.Error{Kind: core.ErrServerError, Status: 500, Extra: map[string]any{core.ExtraRawBody: `{"type":"error","raw":1}`}},
			status: 500, ct: core.RawBodyJSON,
		},
		{
			name:   "chat rate limit stream keeps retry-after",
			req:    &core.Request{Endpoint: core.EndpointChat, Model: "m", Stream: true},
			e:      &core.Error{Kind: core.ErrRateLimit, Status: 429, RetryAfter: time.Second, Extra: map[string]any{core.ExtraRawBody: "rate limited"}},
			status: 429, ct: core.RawBodyText, headers: []string{"retry-after"},
		},
		{
			name:   "messages text non-stream",
			req:    &core.Request{Endpoint: core.EndpointMessages, Model: "m"},
			e:      &core.Error{Kind: core.ErrGatewayTimeout, Status: 504, Extra: map[string]any{core.ExtraRawBody: "upstream timeout"}},
			status: 504, ct: core.RawBodyText,
		},
		{
			name:   "messages rate limit stream keeps retry-after",
			req:    &core.Request{Endpoint: core.EndpointMessages, Model: "m", Stream: true},
			e:      &core.Error{Kind: core.ErrRateLimit, Status: 429, Extra: map[string]any{core.ExtraRawBody: `{"a":[1]}`}},
			status: 429, ct: core.RawBodyJSON, headers: []string{"retry-after"}, // no request-id: OpenCode sends none (spec 006 16.7)
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := core.NewMemWriter()
			d.WriteError(w, tc.req, tc.e, nil)
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
