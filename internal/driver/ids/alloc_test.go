package ids_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestDrawOneAllocation: the id is built on the stack, the string is the
// only allocation (task 270).
func TestDrawOneAllocation(t *testing.T) {
	r := generate.NewRNG(1, "ids")
	if n := testing.AllocsPerRun(100, func() { _ = ids.OpenAIChat(r) }); n != 1 {
		t.Fatalf("OpenAIChat allocs = %v, want 1", n)
	}
}
