package core

import (
	"sync"
	"time"
)

// Clock is the time port (spec 24.4). Wall time is for display and
// staleness heuristics only; ownership, deadlines and limiters use the
// monotonic reading.
type Clock interface {
	// Now returns the wall-clock time.
	Now() time.Time
	// Monotonic returns a reading of a monotonic clock as the time elapsed
	// since an arbitrary, fixed origin. It never goes backwards.
	Monotonic() time.Duration
}

// SystemClock is the production Clock backed by the operating system.
type SystemClock struct {
	once   sync.Once
	origin time.Time
}

// Now implements Clock.
func (c *SystemClock) Now() time.Time { return time.Now() }

// Monotonic implements Clock.
func (c *SystemClock) Monotonic() time.Duration {
	c.once.Do(func() { c.origin = time.Now() })
	return time.Since(c.origin)
}

// TimeFormat is the JSON wire format for timestamps: RFC 3339 in UTC with
// millisecond precision (spec 1.2).
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t in the canonical JSON timestamp form.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses an RFC 3339 timestamp and returns it in UTC.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
