package azure_test

import (
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
)

// TestContract runs the driver contract suite (spec 005).
func TestContract(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "azure")) }
