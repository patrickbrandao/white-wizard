package gemini_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
)

// TestContract runs the driver contract suite (spec 005).
func TestContract(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "gemini")) }
