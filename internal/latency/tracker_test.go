package latency

import "testing"

// TestTrackerCountsActive: Enter and Leave move the in-flight count the load
// factor reads.
func TestTrackerCountsActive(t *testing.T) {
	var tr Tracker
	if tr.Enter() != 1 || tr.Enter() != 2 || tr.Active() != 2 {
		t.Fatalf("active %d after two Enter", tr.Active())
	}
	tr.Leave()
	if tr.Active() != 1 {
		t.Fatalf("active %d after Leave", tr.Active())
	}
}
