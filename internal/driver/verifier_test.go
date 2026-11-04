package driver

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// clientVerifier verifies the client of the Messages route when on, and
// refuses everything it verifies.
type clientVerifier struct {
	Driver
	on bool
}

func (d clientVerifier) VerifiesClient(kind Kind) bool { return d.on && kind == KindMessages }

func (clientVerifier) VerifyClient(Kind, RawHead, []byte) *core.Error {
	return ForgedRequest("query", "no line", "forged")
}

// TestClientVerifierOf: the verifier is found through wrappers, and only
// for the kinds the mount verifies.
func TestClientVerifierOf(t *testing.T) {
	cases := []struct {
		name string
		d    Driver
		kind Kind
		want bool
	}{
		{"nil", nil, KindMessages, false},
		{"without the interface", rawWrapper{}, KindMessages, false},
		{"off", clientVerifier{on: false}, KindMessages, false},
		{"another kind", clientVerifier{on: true}, KindModels, false},
		{"on", clientVerifier{on: true}, KindMessages, true},
		{"wrapped", rawWrapper{clientVerifier{on: true}}, KindMessages, true},
	}
	for _, c := range cases {
		v, ok := ClientVerifierOf(c.d, c.kind)
		if ok != c.want || (v != nil) != c.want {
			t.Errorf("%s: ClientVerifierOf = %v %v, want %v", c.name, v, ok, c.want)
		}
	}
}

// TestForgedRequest: the refusal carries its reason and detail, which
// ForgedReason reads back; any other error is not a forged request.
func TestForgedRequest(t *testing.T) {
	e := ForgedRequest("header_order", `expected "Accept" at position 1`, "msg")
	if e.Kind != core.ErrForgedRequest || e.Message != "msg" {
		t.Fatalf("%+v", e)
	}
	if r, d, ok := ForgedReason(e); !ok || r != "header_order" || d != `expected "Accept" at position 1` {
		t.Errorf("ForgedReason = %q %q %v", r, d, ok)
	}
	for _, other := range []*core.Error{nil, {Kind: core.ErrPermission}} {
		if r, d, ok := ForgedReason(other); ok || r != "" || d != "" {
			t.Errorf("%v: ForgedReason = %q %q %v", other, r, d, ok)
		}
	}
	// Without Extra (a driver's own error), the codes are empty.
	if r, d, ok := ForgedReason(&core.Error{Kind: core.ErrForgedRequest}); !ok || r != "" || d != "" {
		t.Errorf("bare = %q %q %v", r, d, ok)
	}
}
