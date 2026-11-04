//go:build race

package loadgen

// raceEnabled: the race detector allocates on its own, so allocation
// counts are only asserted without it.
const raceEnabled = true
