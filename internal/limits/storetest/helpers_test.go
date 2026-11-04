package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// noResetStore reports ProjectTokensReset == 0 on every decision, as a store
// that forgets the reset of the project unit would.
type noResetStore struct{ limits.Store }

func (n noResetStore) Check(ctx context.Context, req limits.CheckRequest) (limits.Decision, error) {
	d, err := n.Store.Check(ctx, req)
	d.State.ProjectTokensReset = 0
	return d, err
}

// TestProjectTPMReportedNeedsReset covers the failure branch of the project
// reset assertion: a store with no project reset must fail the subtest.
func TestProjectTPMReportedNeedsReset(t *testing.T) {
	ft := runCase(testProjectTPMReported, noResetStore{limits.NewMemory()})
	if !ft.failed() || !strings.Contains(strings.Join(ft.msgs, "\n"), "project reset") {
		t.Fatalf("subtest did not fail on the missing reset: %v", ft.msgs)
	}
}
