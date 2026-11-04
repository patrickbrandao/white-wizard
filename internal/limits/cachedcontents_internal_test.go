package limits

import "testing"

// TestSharedCachedContentsLocal: before any store is shared, the gemini
// driver gets the process-local memory store.
func TestSharedCachedContentsLocal(t *testing.T) {
	prev := sharedCC.Swap(nil)
	t.Cleanup(func() { sharedCC.Store(prev) })
	if SharedCachedContents() != CachedContentStore(localCC) {
		t.Fatal("not the local store")
	}
}
