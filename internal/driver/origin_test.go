package driver

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestOrigin: the origin is what the server stored in OriginHeader (ADR
// 0073); without it (nil request or headers, a driver called outside the
// server) it is empty. Host and X-Forwarded-* are never read.
func TestOrigin(t *testing.T) {
	h := MapHeaders{OriginHeader: "https://wwz.exemplo/edge/opencode", "host": "x", "x-forwarded-host": "y"}
	if got := Origin(&core.Request{Headers: h}); got != "https://wwz.exemplo/edge/opencode" {
		t.Errorf("Origin %q", got)
	}
	if got := OriginOf(h); got != "https://wwz.exemplo/edge/opencode" {
		t.Errorf("OriginOf %q", got)
	}
	if Origin(nil) != "" || Origin(&core.Request{}) != "" || OriginOf(nil) != "" {
		t.Error("origin without the header must be empty")
	}
	if got := Origin(&core.Request{Headers: map[string]string{"host": "wwz", "x-forwarded-proto": "https"}}); got != "" {
		t.Errorf("origin from client headers: %q", got)
	}
}
