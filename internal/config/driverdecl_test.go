package config

import (
	"slices"
	"strings"
	"testing"
)

// TestDriverRegistryEmpty: before any driver registers, the configuration
// knows none (every model in every catalog, no option checked, no default
// mount).
func TestDriverRegistryEmpty(t *testing.T) {
	saved := decls.Load()
	decls.Store(nil)
	defer decls.Store(saved)
	if _, ok := declOf("openai"); ok {
		t.Fatal("declaration without registry")
	}
	if ds := declaredDrivers(); ds != nil {
		t.Fatalf("declared: %v", ds)
	}
	if registered("openai") {
		t.Fatal("registered without registry")
	}
	if ms := defaultMounts(); len(ms) != 0 {
		t.Fatalf("default mounts: %v", ms)
	}
}

// TestDriverRegistryDeclarations: the declarations the drivers publish
// drive the defaults and the checks that once listed driver names.
func TestDriverRegistryDeclarations(t *testing.T) {
	var names []string
	for _, m := range defaultMounts() {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "openai,anthropic,openrouter,opencode" {
		t.Fatalf("default mounts: %v", names)
	}
	if got := strings.Join(toolMisbehaviorDrivers(), ","); got != "anthropic,openai,opencode,openrouter" {
		t.Fatalf("tool misbehavior drivers: %s", got)
	}
	if !legacyProtocol("openrouter") || legacyProtocol("openai") {
		t.Fatal("legacy protocols")
	}
	// A registered driver that declares nothing is unknown to the checks.
	RegisterDriver("wwz-undeclared", func() (DriverDecl, bool) { return DriverDecl{}, false })
	if _, ok := declOf("wwz-undeclared"); ok || slices.Contains(declaredDrivers(), "wwz-undeclared") {
		t.Fatal("undeclared driver listed")
	}
	// ... and accepts no option (spec 005); a name no driver registered is
	// the driver registry's to refuse.
	if err := validateDriverOptions(0, DriverMount{Name: "wwz-undeclared", Options: map[string]string{"x": "1"}}); err == nil || !strings.Contains(err.Error(), `unknown option "x"`) {
		t.Fatalf("option on an undeclared driver: %v", err)
	}
	if err := validateDriverOptions(0, DriverMount{Name: "wwz-unregistered", Options: map[string]string{"x": "1"}}); err != nil {
		t.Fatalf("unregistered driver: %v", err)
	}
	if !slices.Equal(ReasoningFields(), reasoningFields) {
		t.Fatal("ReasoningFields")
	}
	ReasoningFields()[0] = "x"
	if reasoningFields[0] == "x" {
		t.Fatal("ReasoningFields shares its slice")
	}
}
