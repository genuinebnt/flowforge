package alerts

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Quiet-hours parse errors.
var (
	ErrQuietHoursSyntax = errors.New("alerts: quiet_hours must look like \"22:00-07:00 Asia/Kolkata\"")
	ErrQuietHoursEmpty  = errors.New("alerts: quiet_hours window is empty")
	ErrUnknownZone      = errors.New("alerts: unknown time zone")
)

// QuietHours is a daily window, in a time zone, during which deliveries
// below High are held until the window ends (spec 15.3).
type QuietHours struct {
	Start, End int            // minutes after local midnight; End may be < Start (wraps midnight)
	Location   *time.Location // zone the window is expressed in
	Text       string         // source text
}

// parseQuietHours parses "HH:MM-HH:MM [Zone]". The zone defaults to UTC.
// load resolves zone names; "Local" is refused because the result would
// depend on the host.
func parseQuietHours(s string, load func(string) (*time.Location, error)) (*QuietHours, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return nil, ErrQuietHoursSyntax
	}
	from, to, ok := strings.Cut(fields[0], "-")
	if !ok {
		return nil, ErrQuietHoursSyntax
	}
	start, err1 := parseClock(from)
	end, err2 := parseClock(to)
	if err1 != nil || err2 != nil {
		return nil, ErrQuietHoursSyntax
	}
	if start == end {
		return nil, ErrQuietHoursEmpty
	}
	q := &QuietHours{Start: start, End: end, Location: time.UTC, Text: s}
	if len(fields) == 2 {
		zone := fields[1]
		if zone == "Local" {
			return nil, fmt.Errorf("%w: Local", ErrUnknownZone)
		}
		loc, err := load(zone)
		if err != nil || loc == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnknownZone, zone)
		}
		q.Location = loc
	}
	return q, nil
}

// parseClock parses HH:MM (two-digit fields, 00:00 to 23:59).
func parseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, ErrQuietHoursSyntax
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil || h < 0 || h > 23 || s[0] == '+' || s[0] == '-' {
		return 0, ErrQuietHoursSyntax
	}
	m, err := strconv.Atoi(s[3:])
	if err != nil || m < 0 || m > 59 || s[3] == '+' || s[3] == '-' {
		return 0, ErrQuietHoursSyntax
	}
	return h*60 + m, nil
}

// Contains reports whether t falls inside the window.
func (q *QuietHours) Contains(t time.Time) bool {
	_, ok := q.windowEnd(t)
	return ok
}

// DeliverAt returns when a notification of severity sev produced at t may
// be delivered: t itself for High and Critical or outside the window,
// otherwise the end of the current window.
func (q *QuietHours) DeliverAt(sev Severity, t time.Time) time.Time {
	if q == nil || sev >= SeverityHigh {
		return t
	}
	if end, ok := q.windowEnd(t); ok && end.After(t) {
		return end
	}
	return t
}

// windowEnd returns the end of the window containing t, if any.
func (q *QuietHours) windowEnd(t time.Time) (time.Time, bool) {
	lt := t.In(q.Location)
	tod := lt.Hour()*60 + lt.Minute()
	y, mo, d := lt.Date()
	at := func(dayOffset, minutes int) time.Time {
		return time.Date(y, mo, d+dayOffset, minutes/60, minutes%60, 0, 0, q.Location)
	}
	if q.Start < q.End {
		if tod >= q.Start && tod < q.End {
			return at(0, q.End), true
		}
		return time.Time{}, false
	}
	switch {
	case tod >= q.Start:
		return at(1, q.End), true
	case tod < q.End:
		return at(0, q.End), true
	}
	return time.Time{}, false
}
