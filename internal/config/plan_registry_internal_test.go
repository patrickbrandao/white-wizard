package config

import (
	"strings"
	"testing"
)

// TestValidatePlanWithoutPlans: with no registered driver declaring plans
// (a build without opencode), any tiers.<name>.plan is refused and the
// message says why (task 425, ADR 0072, item 9). Not parallel: it swaps
// the registry and restores it.
func TestValidatePlanWithoutPlans(t *testing.T) {
	saved := decls.Load()
	defer decls.Store(saved)
	decls.Store(&map[string]*driverDecl{})
	err := validatePlan("gold", "go")
	if err == nil || !strings.Contains(err.Error(), `tiers.gold.plan: unknown plan "go" (no driver declares plans)`) {
		t.Errorf("err = %v", err)
	}
	if err := validatePlan("gold", ""); err != nil {
		t.Errorf("empty plan: %v", err)
	}
}
