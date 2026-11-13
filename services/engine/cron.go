package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression (minute hour day-of-month
// month day-of-week), evaluated in UTC, the format Cloud Scheduler uses
// (unix-cron). Each field accepts *, a value, a range a-b, a list a,b and
// a step */n, a-b/n or a/n (a to the field's maximum). Day of week is 0-7
// (0 and 7 are Sunday). When both day fields are restricted a day matches
// if either does, as in Vixie cron; a field counts as unrestricted only
// when it is exactly "*". Vixie cron also treats "*/n" as unrestricted (so
// "0 0 */2 * 1" there means odd days that are Mondays, here odd days or
// Mondays). Names (JAN, MON) and the @hourly shorthands are not supported.
type Cron struct {
	src                          string
	minute, hour, dom, month, dw uint64 // bit i set: value i allowed
	domStar, dowStar             bool
}

// ErrCron reports an invalid expression.
var ErrCron = errors.New("engine: invalid cron expression")

// ParseCron parses a five-field expression.
func ParseCron(s string) (Cron, error) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return Cron{}, fmt.Errorf("%w: want 5 fields, got %d", ErrCron, len(f))
	}
	c := Cron{src: strings.Join(f, " "), domStar: f[2] == "*", dowStar: f[4] == "*"}
	var err error
	for _, x := range []struct {
		dst    *uint64
		field  string
		lo, hi int
	}{{&c.minute, f[0], 0, 59}, {&c.hour, f[1], 0, 23}, {&c.dom, f[2], 1, 31}, {&c.month, f[3], 1, 12}, {&c.dw, f[4], 0, 7}} {
		if *x.dst, err = cronField(x.field, x.lo, x.hi); err != nil {
			return Cron{}, err
		}
	}
	if c.dw&(1<<7) != 0 {
		c.dw |= 1 // 7 is Sunday
	}
	return c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%w: step %q", ErrCron, stepS)
			}
			step = n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			as, bs, _ := strings.Cut(rng, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(as)
			b, err2 = strconv.Atoi(bs)
			if err1 != nil || err2 != nil || a > b {
				return 0, fmt.Errorf("%w: range %q", ErrCron, rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("%w: value %q", ErrCron, rng)
			}
			a, b = n, n
			if hasStep {
				b = hi // "5/15" means 5-max/15
			}
		}
		if a < lo || b > hi {
			return 0, fmt.Errorf("%w: %q outside %d-%d", ErrCron, part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

// String returns the normalized expression.
func (c Cron) String() string { return c.src }

func (c Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dw&(1<<int(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}

// Next returns the first minute strictly after t (UTC) the expression
// matches, or the zero time if none within five years (e.g. 30 February).
func (c Cron) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<int(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if c.hour&(1<<t.Hour()) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if c.minute&(1<<t.Minute()) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
