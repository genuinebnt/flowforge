//go:build race

package htmlparse

// raceEnabled skips timing assertions under the race detector.
const raceEnabled = true
