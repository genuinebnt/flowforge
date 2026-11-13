package scope

import (
	"fmt"
	"time"

	// The zone database is embedded so window evaluation does not depend
	// on the host's tzdata (docs/PORTING_RUST.md 5.1).
	_ "time/tzdata"
)

const minutesPerDay = 24 * 60

// window is a compiled Window. Minutes count from local midnight.
type window struct {
	days  uint8 // bit i set for time.Weekday(i)
	start int   // inclusive
	end   int   // exclusive; end < start means the window runs past midnight
	loc   *time.Location
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func compileWindow(w Window) (window, error) {
	var c window
	if len(w.Days) == 0 {
		return c, fmt.Errorf("%w: no days", ErrInvalidWindow)
	}
	for _, d := range w.Days {
		wd, ok := dayNames[d]
		if !ok {
			return c, fmt.Errorf("%w: unknown day %q (want mon..sun)", ErrInvalidWindow, d)
		}
		c.days |= 1 << wd
	}
	var err error
	if c.start, err = parseClock(w.Start, false); err != nil {
		return c, fmt.Errorf("%w: start: %w", ErrInvalidWindow, err)
	}
	if c.end, err = parseClock(w.End, true); err != nil {
		return c, fmt.Errorf("%w: end: %w", ErrInvalidWindow, err)
	}
	if c.start == c.end {
		return c, fmt.Errorf("%w: start equals end (use 00:00-24:00 for a whole day)", ErrInvalidWindow)
	}
	// "" and "Local" would make the decision depend on the host.
	if w.TZ == "" || w.TZ == "Local" {
		return c, fmt.Errorf("%w: tz must be an IANA zone name, got %q", ErrInvalidWindow, w.TZ)
	}
	if c.loc, err = time.LoadLocation(w.TZ); err != nil {
		return c, fmt.Errorf("%w: tz %q: %w", ErrInvalidWindow, w.TZ, err)
	}
	return c, nil
}

// parseClock parses "HH:MM" into minutes after midnight. "24:00" is
// accepted only for end times.
func parseClock(s string, isEnd bool) (int, error) {
	if len(s) != 5 || s[2] != ':' || !isDigits(s[:2]) || !isDigits(s[3:]) {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	switch {
	case isEnd && h == 24 && m == 0:
		return minutesPerDay, nil
	case h > 23 || m > 59:
		return 0, fmt.Errorf("time %q out of range", s)
	}
	return h*60 + m, nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// contains reports whether instant now falls inside w, using wall-clock
// time in w's zone (so DST shifts move the window with local time).
// Seconds are ignored: only the local hour and minute are compared.
//
// PORT: the instant is converted to local time with the embedded tzdata
// (time.In); a skipped DST hour simply never occurs and both passes of a
// repeated hour get the same answer. chrono-tz's with_timezone behaves the same
// way; pin the tz database version in both implementations.
func (w *window) contains(now time.Time) bool {
	lt := now.In(w.loc)
	wd := lt.Weekday()
	m := lt.Hour()*60 + lt.Minute()
	if w.start < w.end {
		return w.hasDay(wd) && m >= w.start && m < w.end
	}
	// Overnight: the evening part belongs to today, the morning part to
	// the window that started yesterday.
	yesterday := (wd + 6) % 7
	return (w.hasDay(wd) && m >= w.start) || (w.hasDay(yesterday) && m < w.end)
}

func (w *window) hasDay(d time.Weekday) bool { return w.days&(1<<d) != 0 }

// inAnyWindow reports whether now is inside at least one window; an empty
// list is always open.
func inAnyWindow(ws []window, now time.Time) bool {
	if len(ws) == 0 {
		return true
	}
	for i := range ws {
		if ws[i].contains(now) {
			return true
		}
	}
	return false
}
