package lab

import (
	"sync"
	"time"
)

// Clock is the lab's settable clock (core.Clock): wall time jumps to each
// deploy's time, so the monitored history spans weeks while the test runs
// in seconds. It is safe for concurrent use. Pass it to the tool runner
// (devstack.Options.ToolClock) so capture times follow the deploys.
type Clock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{wall: start} }

// Now implements core.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

// Monotonic implements core.Clock: it advances with every forward Set.
func (c *Clock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// Set moves the clock to t; the monotonic reading advances by the step
// when t is later and never goes backwards.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := t.Sub(c.wall); d > 0 {
		c.mono += d
	}
	c.wall = t
}
