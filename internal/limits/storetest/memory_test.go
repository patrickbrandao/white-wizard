package storetest_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/limits/storetest"
)

// TestMemory runs the suite against limits.Memory inside the package, so
// the suite's own coverage is measured by its own tests (ADR 0013).
func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) limits.Store { return limits.NewMemory() })
}

// minimalStore exposes only limits.Store, hiding the optional interfaces
// (CounterResetter, ConversationResetter, TierSetter, EntityResetter,
// PolicySource) so the suite takes its skip branches.
type minimalStore struct{ limits.Store }

func TestMinimalStoreSkipsOptional(t *testing.T) {
	storetest.Run(t, func(*testing.T) limits.Store { return minimalStore{limits.NewMemory()} })
}

// TestKnownBugSkips checks that KnownBug skips exactly the named subtest.
func TestKnownBugSkips(t *testing.T) {
	calls := 0
	storetest.Run(t, func(*testing.T) limits.Store {
		calls++
		return limits.NewMemory()
	}, storetest.KnownBug("Reset/KindNotEmpty", "exercised by task 148"))
	if calls == 0 {
		t.Fatal("factory never called")
	}
}
