package openrouter_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestContract runs the driver contract suite (spec 005): one dialect per
// surface (Chat, Messages, Responses), all in the same suite.
func TestContract(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "openrouter")) }
