package driver_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestGatewayDriversReasoningOption: opencode and openrouter reject an
// unknown reasoning_field (ADR 0046).
func TestGatewayDriversReasoningOption(t *testing.T) {
	for _, name := range []string{"opencode", "openrouter"} {
		if _, err := driver.New(name, map[string]string{"reasoning_field": "inline_tag"}); err != nil {
			t.Errorf("%s inline_tag: %v", name, err)
		}
		if _, err := driver.New(name, map[string]string{"reasoning_field": "thoughts"}); err == nil {
			t.Errorf("%s accepted reasoning_field=thoughts", name)
		}
	}
}

// TestDefaultConventions: the request id header, the check order and the
// model of the response follow each real provider (spec 006 16.7, ADR 0049).
func TestDefaultConventions(t *testing.T) {
	want := map[string]driver.Conventions{
		"openai":     {RequestIDHeader: "x-request-id", AuthFirst: true, ModelVersion: true},
		"anthropic":  {RequestIDHeader: "request-id", AuthFirst: true, ModelVersion: true},
		"openrouter": {AuthFirst: true, ModelsPublic: true},
		"opencode":   {ModelsWithoutKey: true, ModelIgnoresKey: true},
		"":           {RequestIDHeader: "x-request-id", AuthFirst: true, ModelVersion: true},
	}
	for name, w := range want {
		if got := driver.DefaultConventions(name); got != w {
			t.Errorf("%q: %+v, want %+v", name, got, w)
		}
	}
	for _, name := range []string{"openai", "anthropic", "openrouter", "opencode"} {
		d, err := driver.New(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := driver.ConventionsOf(d); got != want[name] {
			t.Errorf("ConventionsOf(%s) = %+v", name, got)
		}
	}
	if got := driver.ConventionsOf(nil); got != want[""] {
		t.Errorf("ConventionsOf(nil) = %+v", got)
	}
}

// conventionDriver declares its own conventions.
type conventionDriver struct{ driver.Driver }

func (conventionDriver) Name() string { return "openai" }

func (conventionDriver) Conventions() driver.Conventions {
	return driver.Conventions{RequestIDHeader: "x-custom-id"}
}

// TestConventioner: a driver's own conventions win over its name's.
func TestConventioner(t *testing.T) {
	if got := driver.ConventionsOf(conventionDriver{}); got != (driver.Conventions{RequestIDHeader: "x-custom-id"}) {
		t.Errorf("ConventionsOf = %+v", got)
	}
}
