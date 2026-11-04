package config

import (
	"encoding/json"
	"os"
	"testing"
)

// TestMain registers the six drivers' declarations from
// testdata/driver_decls.json: the configuration knows no driver by name and
// its tests cannot import the drivers (they import config). The copy is
// kept equal to the drivers by internal/driver's
// TestDeclarationsMatchConfigFixture.
func TestMain(m *testing.M) {
	b, err := os.ReadFile("testdata/driver_decls.json")
	if err != nil {
		panic(err)
	}
	var decls map[string]DriverDecl
	if err := json.Unmarshal(b, &decls); err != nil {
		panic(err)
	}
	for name, d := range decls {
		RegisterDriver(name, func() (DriverDecl, bool) { return d, true })
	}
	os.Exit(m.Run())
}
