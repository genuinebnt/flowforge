//go:build race

package limiter

// raceEnabled reports whether the race detector is on; it instruments
// allocations, so allocation assertions are skipped under -race.
const raceEnabled = true
