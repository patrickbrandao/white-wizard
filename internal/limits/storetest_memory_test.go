package limits_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/limits/storetest"
)

// TestMemoryStoreContract runs the shared limits.Store suite against the
// in-memory store. A divergence of Memory from specs 010/012 is tracked with
// storetest.KnownBug and a fix task; none is open.
func TestMemoryStoreContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) limits.Store { return limits.NewMemory() })
}
