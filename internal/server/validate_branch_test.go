package server

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// namedOnly is a driver with IDMinter and ModelNamer but no slugRouter.
type namedOnly struct {
	driver.Driver
	driver.IDMinter
	driver.ModelNamer
}

// Unwrap keeps the wrapped driver's validator reachable (driver.As).
func (n namedOnly) Unwrap() driver.Driver { return n.Driver }

func TestWithValidationNamedWithoutSlug(t *testing.T) {
	or, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := withValidation(namedOnly{or, or.(driver.IDMinter), or.(driver.ModelNamer)}, nil)
	if _, ok := w.(driver.ModelNamer); !ok {
		t.Fatal("ModelNamer lost")
	}
	if _, ok := w.(slugRouter); ok {
		t.Fatal("slugRouter invented")
	}
}

func TestKnownModel(t *testing.T) {
	cfg, err := config.Parse([]byte(fastCfg + "models: [{id: m, latency: fast}, {id: old, latency: fast, retires_at: \"2020-01-01\"}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := &validation{cfg: cfg, protocol: "openai"}
	// A retired model is unknown, as in the engine (ADR 0053).
	for name, want := range map[string]bool{"m": true, "m-scenario-x": true, "n": false, "n-scenario-x": false, "old": false} {
		if got := v.KnownModel(driver.KindChat, name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if !(&validation{}).KnownModel(driver.KindChat, "anything") {
		t.Fatal("no configuration must not refuse")
	}
}
