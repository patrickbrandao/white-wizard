package drivertest_test

import (
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestOpenRouterModelName runs the suite on the one driver with a
// driver.ModelNamer, so the slug checks (ADR 0038) are exercised here too.
func TestOpenRouterModelName(t *testing.T) {
	p, _ := drivertest.ProfileFor("openrouter")
	if p.ModelName == nil {
		t.Fatal("openrouter profile without ModelName")
	}
	for id, want := range map[string]string{"m": "white-wizard/m", "a/m": "a/m"} {
		if got := p.ModelName(id); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", id, got, want)
		}
	}
	drivertest.Run(t, drivertest.MustNew(t, "openrouter"))
}

// TestGemini runs the suite on the Gemini dialect (models/{m}:{method},
// KindGenerate), whose checks live in gemini.go.
func TestGemini(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "gemini")) }

// TestAzure runs the suite on the Azure dialect (content filter
// annotations, Azure envelope, apim-request-id).
func TestAzure(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "azure")) }

// TestNeutralProfile runs the suite with an empty profile, the path a new
// driver without provider expectations takes: only dialect-neutral checks.
func TestNeutralProfile(t *testing.T) {
	drivertest.Run(t, drivertest.MustNew(t, "openai"), drivertest.WithProfile(drivertest.Profile{}))
}

func TestProfileFor(t *testing.T) {
	for _, name := range []string{"openai", "anthropic", "opencode", "openrouter", "gemini", "azure"} {
		p, ok := drivertest.ProfileFor(name)
		if !ok || len(p.Surfaces) == 0 || p.Credential == nil || p.Policy.Entities == nil {
			t.Errorf("ProfileFor(%q) = %+v, %v", name, p, ok)
		}
		// A fresh copy each call: editing one must not leak into the next.
		p.Surfaces = nil
		if q, _ := drivertest.ProfileFor(name); q.Surfaces == nil {
			t.Errorf("ProfileFor(%q) shares state between calls", name)
		}
	}
	if _, ok := drivertest.ProfileFor("bedrock"); ok {
		t.Error("ProfileFor(bedrock): no built-in profile expected")
	}
}
