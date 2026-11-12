package limiter

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// FuzzLimGCRA feeds arbitrary (now, cost) sequences to the mutex and the
// atomic GCRA. Neither panics, both agree exactly, every error is one of
// the documented input errors, results stay in range, and the admitted
// requests (placed at the latest reading seen) obey the window bound.
func FuzzLimGCRA(f *testing.F) {
	f.Add(uint16(10), uint8(5), int64(0), []byte{0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 100, 1})
	f.Add(uint16(1), uint8(1), int64(-1), []byte{0xff, 0xff, 2, 0x7f, 0xff, 1, 0x80, 0, 0})
	f.Add(uint16(65535), uint8(255), int64(1<<62), []byte{1, 2, 3, 4, 5, 6})
	f.Add(uint16(3), uint8(7), int64(-1<<62), []byte{0x10, 0, 7, 0xf0, 0, 3, 0x10, 0, 8})
	f.Fuzz(func(t *testing.T, rate uint16, burst uint8, start int64, data []byte) {
		p := Params{Rate: float64(rate), Burst: int(burst)}
		g, err1 := NewGCRA(all(p))
		a, err2 := NewAtomicGCRA(all(p))
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("constructors disagree: %v, %v", err1, err2)
		}
		if err1 != nil {
			if !errors.Is(err1, ErrInvalidParams) || (rate > 0 && burst > 0) {
				t.Fatalf("unexpected constructor error %v for %+v", err1, p)
			}
			return
		}
		now := start
		latest, valid := int64(0), false
		var events []admit
		for len(data) >= 3 {
			// 100 µs steps, signed, so the clock also runs backwards.
			now += int64(int16(binary.BigEndian.Uint16(data))) * int64(100*time.Microsecond)
			cost := int(int8(data[2]))
			data = data[3:]
			r1, e1 := g.Acquire("k", cost, time.Duration(now))
			r2, e2 := a.Acquire("k", cost, time.Duration(now))
			if r1 != r2 || errorCode(e1) != errorCode(e2) {
				t.Fatalf("now=%d cost=%d: gcra %+v %v, atomic %+v %v", now, cost, r1, e1, r2, e2)
			}
			inRange := now <= int64(MaxTime) && now >= -int64(MaxTime)
			switch {
			case !inRange:
				if !errors.Is(e1, ErrInvalidTime) {
					t.Fatalf("now=%d out of range: %v", now, e1)
				}
				continue
			case cost < 1:
				if !errors.Is(e1, ErrInvalidCost) {
					t.Fatalf("cost %d: %v", cost, e1)
				}
				continue
			case cost > p.Burst:
				if !errors.Is(e1, ErrCostExceedsCapacity) || r1.RetryAfter != Never {
					t.Fatalf("cost %d > burst: %+v %v", cost, r1, e1)
				}
				continue
			case e1 != nil:
				t.Fatalf("unexpected error %v", e1)
			}
			if r1.RetryAfter < 0 || (r1.Allowed && r1.RetryAfter != 0) || (!r1.Allowed && r1.RetryAfter == 0) ||
				r1.Remaining < 0 || r1.Remaining > p.Burst || r1.Limit != p.Burst {
				t.Fatalf("bad result %+v for %+v", r1, p)
			}
			if !valid || now > latest {
				latest, valid = now, true
			}
			if r1.Allowed {
				events = append(events, admit{latest, int64(cost)})
			}
		}
		if len(events) > 400 {
			events = events[:400] // keep the quadratic check fast
		}
		if msg := windowViolation(events, rateBound{int64(p.Burst), int64(rate), 1e9}); msg != "" {
			t.Fatalf("%+v: %s", p, msg)
		}
	})
}
