package drivertest_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
)

// withSurface returns the profile of a driver with one surface changed.
func withSurface(t *testing.T, name string, k driver.Kind, edit func(*drivertest.Surface)) drivertest.Profile {
	t.Helper()
	p, ok := drivertest.ProfileFor(name)
	if !ok {
		t.Fatalf("no profile for %s", name)
	}
	m := map[driver.Kind]drivertest.Surface{}
	for kk, v := range p.Surfaces {
		m[kk] = v
	}
	sf := m[k]
	edit(&sf)
	m[k] = sf
	p.Surfaces = m
	return p
}

// renamed is a driver the suite has no profile for.
type renamed struct{ driver.Driver }

func (renamed) Name() string { return "no-such-profile" }

func TestRunWithoutAProfileSkipsTheProviderChecks(t *testing.T) {
	runSuite(renamed{mustNew(t, "openai")})
}

// A profile that declares a keep-alive comment: the suite demands it as the
// first frame of a Chat stream.
func TestKeepAliveFrame(t *testing.T) {
	p := withSurface(t, "openai", driver.KindChat, func(s *drivertest.Surface) { s.KeepAlive = "wwz" })
	if runSuite(mustNew(t, "openai"), drivertest.WithProfile(p)) {
		t.Error("a Chat stream without the keep-alive comment passes")
	}
	d := wrap(mustNew(t, "openai"), &mutation{name: "keep-alive", frame: func(i int, b []byte) [][]byte {
		if i == 0 {
			return [][]byte{[]byte(": wwz\n\n"), b}
		}
		return [][]byte{b}
	}})
	runSuite(d, drivertest.WithProfile(p))
}

// A surface whose provider sends no request-id header: the driver that
// sends it fails.
func TestRequestIDHeaderAbsent(t *testing.T) {
	p := withSurface(t, "anthropic", driver.KindMessages, func(s *drivertest.Surface) { s.RequestIDHeader = false })
	if runSuite(mustNew(t, "anthropic"), drivertest.WithProfile(p)) {
		t.Error("a request-id header on a surface that sends none passes")
	}
}

// Without a ReasoningField in the profile the suite infers it from what the
// driver writes.
func TestReasoningFieldInferred(t *testing.T) {
	for _, name := range sweepDrivers {
		p, _ := drivertest.ProfileFor(name)
		for _, k := range []driver.Kind{driver.KindChat, driver.KindCompletions} {
			if _, ok := p.Surfaces[k]; !ok {
				continue
			}
			p := withSurface(t, name, k, func(s *drivertest.Surface) { s.ReasoningField = "" })
			runSuite(mustNew(t, name), drivertest.WithProfile(p))
		}
	}
}

// Without id formats in the profile the id checks have nothing to match.
func TestNoIDFormatsInTheProfile(t *testing.T) {
	for _, name := range sweepDrivers {
		p, _ := drivertest.ProfileFor(name)
		surfaces := map[driver.Kind]drivertest.Surface{}
		for k, sf := range p.Surfaces {
			sf.IDs = nil
			surfaces[k] = sf
		}
		p.Surfaces = surfaces
		if p.Gemini != nil {
			g := *p.Gemini
			gs := map[driver.Kind]drivertest.Surface{}
			for k, sf := range g.Surfaces {
				sf.IDs = nil
				gs[k] = sf
			}
			g.Surfaces = gs
			p.Gemini = &g
		}
		runSuite(mustNew(t, name), drivertest.WithProfile(p))
		runSuite(wrap(mustNew(t, name), &mutation{name: "no ids", hideIDs: true}), drivertest.WithProfile(p))
	}
}

// The suite's own request bodies are embedded files; a missing or broken
// one fails the run instead of going unnoticed.
func TestBrokenTestdataFailsTheRun(t *testing.T) {
	cases := []struct {
		name string
		fs   fstest.MapFS
		fn   func(t *testing.T)
	}{
		{"missing file", fstest.MapFS{}, func(t *testing.T) { drivertest.ReadTestdata(t, "chat_full.json") }},
		{"variants not JSON", fstest.MapFS{"testdata/chat_variants.json": {Data: []byte("{garbled")}}, func(t *testing.T) { drivertest.Variant(t, "chat_variants.json", "x") }},
		{"no such variant", fstest.MapFS{"testdata/chat_variants.json": {Data: []byte(`{"y":{}}`)}}, func(t *testing.T) { drivertest.Variant(t, "chat_variants.json", "x") }},
		{"invalid not JSON", fstest.MapFS{"testdata/invalid.json": {Data: []byte("{garbled")}}, func(t *testing.T) { drivertest.InvalidBodies(t, "chat") }},
		{"invalid not strings", fstest.MapFS{"testdata/invalid.json": {Data: []byte(`{"chat":5}`)}}, func(t *testing.T) { drivertest.InvalidBodies(t, "chat") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := drivertest.SwapTestdata(fs.ReadFileFS(c.fs))
			defer restore()
			if runNested(c.fn) {
				t.Error("the broken testdata did not fail the run")
			}
		})
	}
}
