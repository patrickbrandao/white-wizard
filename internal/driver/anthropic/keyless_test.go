package anthropic

import "testing"

// TestKeylessModelMiss: only a sub-path of a model is answered as an
// unknown route without a key (task 316).
func TestKeylessModelMiss(t *testing.T) {
	d := &Driver{}
	if !d.KeylessModelMiss("claude-haiku-4-5/x") || d.KeylessModelMiss("claude-haiku-4-5") {
		t.Error("KeylessModelMiss")
	}
}
