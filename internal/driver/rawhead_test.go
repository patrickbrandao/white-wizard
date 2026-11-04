package driver

import (
	"reflect"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// rawWanter is a Driver that answers WantsRawHead with want.
type rawWanter struct {
	Driver
	want bool
}

func (d rawWanter) WantsRawHead() bool { return d.want }

// rawWrapper wraps a Driver the way the server's validation does.
type rawWrapper struct{ Driver }

func (w rawWrapper) Unwrap() Driver { return w.Driver }

// TestWantsRawHead: the declaration is found through wrappers; a driver
// without it, or that says false, does not get the raw head.
func TestWantsRawHead(t *testing.T) {
	cases := []struct {
		name string
		d    Driver
		want bool
	}{
		{"nil", nil, false},
		{"without the interface", rawWrapper{}, false},
		{"says false", rawWanter{want: false}, false},
		{"says true", rawWanter{want: true}, true},
		{"wrapped", rawWrapper{rawWanter{want: true}}, true},
	}
	for _, c := range cases {
		if got := WantsRawHead(c.d); got != c.want {
			t.Errorf("%s: WantsRawHead %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRawHeadRoundTrip: what SetRawHead stores comes back from the
// headers and from the request byte for byte, and Headers splits the
// block in arrival order with the spelling and the repeats kept.
func TestRawHeadRoundTrip(t *testing.T) {
	block := "Accept: application/json\r\n" +
		"X-Stainless-OS: MacOS\r\n" +
		"user-agent: claude-cli/2.1.308 (external, sdk-cli)\r\n" +
		"anthropic-beta: a-1\r\n" +
		"anthropic-beta:b-2 \t\r\n" +
		"Odd\n" +
		"Empty:\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"
	target := []byte("/v1/messages?beta=true")
	h := MapHeaders{"x-api-key": "k"}
	SetRawHead(h, []byte("POST"), target, []byte("HTTP/1.1"), []byte(block))
	target[1] = 'X' // SetRawHead copied the bytes

	want := RawHead{Line: "POST /v1/messages?beta=true HTTP/1.1", Block: block}
	got, ok := RawHeadOf(h)
	if !ok || got != want {
		t.Fatalf("RawHeadOf %v %#v", ok, got)
	}
	got, ok = RawHeadOfRequest(&core.Request{Headers: h})
	if !ok || got != want {
		t.Fatalf("RawHeadOfRequest %v %#v", ok, got)
	}
	wantHeaders := []RawHeader{
		{"Accept", "application/json"},
		{"X-Stainless-OS", "MacOS"},
		{"user-agent", "claude-cli/2.1.308 (external, sdk-cli)"},
		{"anthropic-beta", "a-1"},
		{"anthropic-beta", "b-2"},
		{"Odd", ""},
		{"Empty", ""},
		{"Transfer-Encoding", "chunked"},
	}
	if hs := got.Headers(); !reflect.DeepEqual(hs, wantHeaders) {
		t.Errorf("Headers\n got %q\nwant %q", hs, wantHeaders)
	}
	if hs := (RawHead{Block: "A: 1\r\nB: 2\r\n"}).Headers(); len(hs) != 2 || hs[1] != (RawHeader{"B", "2"}) {
		t.Errorf("a block without the final empty line: %q", hs)
	}
	if hs := (RawHead{}).Headers(); len(hs) != 0 {
		t.Errorf("empty block: %q", hs)
	}
}

// TestRawHeadAbsent: without SetRawHead (the driver did not ask) nothing
// is found, and nil headers or requests are safe.
func TestRawHeadAbsent(t *testing.T) {
	if _, ok := RawHeadOf(MapHeaders{"user-agent": "x"}); ok {
		t.Error("RawHeadOf found a head that was never stored")
	}
	if _, ok := RawHeadOf(nil); ok {
		t.Error("RawHeadOf(nil)")
	}
	if _, ok := RawHeadOfRequest(nil); ok {
		t.Error("RawHeadOfRequest(nil)")
	}
	if _, ok := RawHeadOfRequest(&core.Request{}); ok {
		t.Error("RawHeadOfRequest without headers")
	}
	SetRawHead(nil, []byte("GET"), []byte("/"), []byte("HTTP/1.1"), nil) // no panic
}
