package csr

import "time"

// Compaction defaults (spec 21.4).
const (
	DefaultMaxOverlayFraction = 0.01
	DefaultMaxOverlayAge      = 5 * time.Minute
)

// CompactionPolicy decides when the overlay is folded into a new
// snapshot. Zero fields take the defaults.
type CompactionPolicy struct {
	// MaxOverlayFraction triggers compaction when the overlay holds more
	// events than this fraction of the snapshot's edges (default 1%).
	MaxOverlayFraction float64
	// MaxOverlayAge triggers compaction when the overlay has been
	// non-empty for this long (default 5 minutes).
	MaxOverlayAge time.Duration
}

// ShouldCompact reports whether to rebuild. overlayEvents is
// Overlay.Size(), snapshotEdges the snapshot's E, nonEmptySince the time
// the first event entered the overlay and now the current time (time is
// passed in so the decision is deterministic). An empty overlay never
// triggers.
func (p CompactionPolicy) ShouldCompact(overlayEvents, snapshotEdges int, nonEmptySince, now time.Time) bool {
	if overlayEvents <= 0 {
		return false
	}
	frac, age := p.MaxOverlayFraction, p.MaxOverlayAge
	if frac <= 0 {
		frac = DefaultMaxOverlayFraction
	}
	if age <= 0 {
		age = DefaultMaxOverlayAge
	}
	if float64(overlayEvents) > frac*float64(snapshotEdges) {
		return true
	}
	return !now.Before(nonEmptySince.Add(age))
}
