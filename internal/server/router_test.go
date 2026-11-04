package server

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// fakeDriver serves GET /v1/shared and sniffs a header of its own.
type fakeDriver struct {
	driver.Driver // unimplemented methods panic
	name          string
}

func (f fakeDriver) Name() string { return f.name }
func (f fakeDriver) Routes() []driver.Route {
	return []driver.Route{{Method: "GET", Path: "/v1/shared", Kind: driver.KindModels}}
}

func (f fakeDriver) Sniff(h driver.Headers) bool { return strings.Contains(h.Get("x-sniff"), f.name) }

func (f fakeDriver) Authenticate(h driver.Headers) driver.Credential {
	return driver.Credential{Key: strings.TrimPrefix(h.Get("authorization"), "Bearer ")}
}

func init() {
	for _, n := range []string{"fake-a", "fake-b", "fake-c"} {
		driver.Register(n, func(map[string]string) (driver.Driver, error) { return fakeDriver{name: n}, nil })
	}
}

func TestPickSniffThenHintThenOrder(t *testing.T) {
	cfg := &config.Config{
		Drivers: []config.DriverMount{{Name: "fake-a"}, {Name: "fake-b"}, {Name: "fake-c"}},
		Keys:    []config.KeyConfig{{Key: "k-c", Driver: "fake-c"}, {Key: "k-b", Driver: "fake-b"}},
	}
	r, err := buildRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		h    driver.MapHeaders
		want string
	}{
		{"single sniff wins over hint", driver.MapHeaders{"x-sniff": "fake-b", "authorization": "Bearer k-c"}, "fake-b"},
		{"no sniff: hint breaks the tie", driver.MapHeaders{"authorization": "Bearer k-c"}, "fake-c"},
		{"ambiguous sniff: hint inside the sniffed set", driver.MapHeaders{"x-sniff": "fake-a fake-b", "authorization": "Bearer k-b"}, "fake-b"},
		{"hint outside the sniffed set is ignored", driver.MapHeaders{"x-sniff": "fake-a fake-b", "authorization": "Bearer k-c"}, "fake-a"},
		{"no sniff, unknown key: config order", driver.MapHeaders{"authorization": "Bearer other"}, "fake-a"},
		{"no credential: config order", driver.MapHeaders{}, "fake-a"},
	} {
		m, _ := r.lookup("GET", "/v1/shared", tc.h)
		if m == nil || m.drv.Name() != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.name, m, tc.want)
		}
	}
	if m, other := r.lookup("POST", "/v1/shared", driver.MapHeaders{}); m != nil || !other {
		t.Fatalf("POST on a GET path: %+v %v", m, other)
	}
	if m, other := r.lookup("GET", "/v1/nothing", driver.MapHeaders{}); m != nil || other {
		t.Fatalf("unknown path: %+v %v", m, other)
	}
}

func TestNormPrefix(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "/openai": "/openai", "/openai/": "/openai", " /x// ": "/x"} {
		if got := normPrefix(in); got != want {
			t.Errorf("normPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
