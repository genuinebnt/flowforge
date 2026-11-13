package auth

import (
	"fmt"
	"math"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/genuinebnt/flowforge/core"
)

// Calibration targets of spec 13.1: one hash should take 200-500 ms on the
// production machine, which the 19 MiB / t=2 floor (PolicyParams) reaches
// only on very slow hardware (G-LIM-03).
const (
	CalibrateMin = 200 * time.Millisecond
	CalibrateMax = 500 * time.Millisecond
)

// Measure returns how long one Argon2id hash with p takes, read from
// clock's monotonic reading. It is the production measure for Calibrate.
func Measure(clock core.Clock) func(Params) time.Duration {
	return func(p Params) time.Duration {
		salt := make([]byte, p.SaltLen)
		start := clock.Monotonic()
		argon2.IDKey([]byte("flowforge-calibration"), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
		return clock.Monotonic() - start
	}
}

// Calibrate raises base's iteration count until one hash takes at least
// target (clamped to [CalibrateMin, CalibrateMax]) according to measure,
// and returns the parameters and the last measured duration. Memory and
// parallelism stay as configured: memory is the operator's budget per
// concurrent login, and iterations are the knob that scales time linearly
// (RFC 9106 section 4). It never returns parameters below base, and stops
// at the parse bound of 64 iterations, so a very slow measure cannot pick
// parameters a stored hash could not be verified with. base must meet the
// policy floor (ErrWeakParams otherwise).
func Calibrate(base Params, target time.Duration, measure func(Params) time.Duration) (Params, time.Duration, error) {
	if err := base.validate(); err != nil {
		return base, 0, err
	}
	if !base.MeetsPolicy() {
		return base, 0, fmt.Errorf("%w: %+v", ErrWeakParams, base)
	}
	target = min(max(target, CalibrateMin), CalibrateMax)
	p := base
	d := measure(p)
	// Three rounds: the first estimate assumes time is linear in t; the
	// rest correct for the fixed cost (memory fill) that is not.
	for range 3 {
		if d >= target || p.Time >= maxTime {
			break
		}
		perIter := float64(max(d, time.Microsecond)) / float64(p.Time)
		next := uint32(math.Ceil(float64(target) / perIter))
		next = min(max(next, p.Time+1), maxTime)
		p.Time = next
		d = measure(p)
	}
	return p, d, nil
}
