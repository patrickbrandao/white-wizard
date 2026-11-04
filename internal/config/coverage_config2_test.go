package config

import (
	"strings"
	"testing"
)

func TestCoverageValidateNoModelsAndUnknownDriver(t *testing.T) {
	if err := (&Config{}).Validate(); err == nil || !strings.Contains(err.Error(), "at least one model") {
		t.Fatalf("err=%v", err)
	}
	if err := validateDriverOptions(0, DriverMount{Name: "custom", Options: map[string]string{"x": "y"}}); err != nil {
		t.Fatalf("unknown drivers are left to the registry: %v", err)
	}
}
