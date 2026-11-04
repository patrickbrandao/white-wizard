package opencode_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// TestContract runs the driver contract suite (spec 005): one suite per
// dialect, "dialeto/openai" on the Chat, Responses and Messages routes and
// "dialeto/gemini" on the /google routes (task 283).
func TestContract(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "opencode")) }
