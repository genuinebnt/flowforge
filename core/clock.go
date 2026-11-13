package core

import (
	"sync"
	"time"
)

// Clock is the time port (spec 24.4). Wall time is for display and
// staleness heuristics only; ownership, deadlines and limiters use the
// monotonic reading. Implementations must be safe for concurrent use.
type Clock interface {
	// Now returns the wall-clock time.
	Now() time.Time
	// Monotonic returns a reading of a monotonic clock as the time elapsed
	// since an arbitrary, fixed origin. It never goes backwards.
	Monotonic() time.Duration
}

// SystemClock is the production Clock backed by the operating system. The
// zero value is ready to use and safe for concurrent use; it must not be
// copied after first use (it holds a sync.Once).
type SystemClock struct {
	once   sync.Once
	origin time.Time
}

// Now implements Clock.
func (c *SystemClock) Now() time.Time { return time.Now() }

// Monotonic implements Clock. The origin is fixed at the first call, so
// readings are only comparable within one SystemClock value.
//
// PORT: time.Since uses the monotonic reading Go attaches to time.Now
// values, so wall-clock steps do not affect it; use std::time::Instant.
func (c *SystemClock) Monotonic() time.Duration {
	c.once.Do(func() { c.origin = time.Now() })
	return time.Since(c.origin)
}

// TimeFormat is the JSON wire format for timestamps: RFC 3339 in UTC with
// millisecond precision (spec 1.2).
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t in the canonical JSON timestamp form, converted to
// UTC.
//
// PORT: Go's Format truncates to milliseconds (it never rounds), and the
// "Z07:00" layout prints "Z" for UTC, so the output is always
// 2006-01-02T15:04:05.000Z. Truncate the same way in Rust (chrono's
// SecondsFormat::Millis also truncates).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses an RFC 3339 timestamp with any number of fractional
// digits (including none) and any UTC offset, and returns it in UTC with
// full nanosecond precision (no truncation to milliseconds).
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
