package placement

import (
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Membership reports the workers currently considered live. Live returns a
// fresh slice sorted by id, which the caller may keep or modify.
type Membership interface {
	Live() []WorkerID
}

// StaticMembership is a fixed worker set.
type StaticMembership []WorkerID

var _ Membership = StaticMembership(nil)

// Live implements Membership.
func (s StaticMembership) Live() []WorkerID { return distinct(s) }

// Heartbeat timings from spec 23.3.
const (
	// HeartbeatInterval is how often a worker should call Heartbeat.
	HeartbeatInterval = 5 * time.Second
	// DefaultLiveness is how long a worker stays live after its last
	// heartbeat.
	DefaultLiveness = 15 * time.Second
)

// HeartbeatMembership tracks worker heartbeats against the monotonic
// reading of an injected clock. A worker is live while less than the
// liveness window has elapsed since its last heartbeat. Sending heartbeats
// every HeartbeatInterval is the caller's job. It is safe for concurrent
// use.
type HeartbeatMembership struct {
	clock    core.Clock
	liveness time.Duration

	mu   sync.Mutex
	last map[WorkerID]time.Duration
}

var _ Membership = (*HeartbeatMembership)(nil)

// NewHeartbeatMembership returns an empty membership. liveness <= 0 means
// DefaultLiveness.
func NewHeartbeatMembership(clock core.Clock, liveness time.Duration) *HeartbeatMembership {
	if liveness <= 0 {
		liveness = DefaultLiveness
	}
	return &HeartbeatMembership{clock: clock, liveness: liveness, last: make(map[WorkerID]time.Duration)}
}

// Heartbeat records that w is alive now.
func (m *HeartbeatMembership) Heartbeat(w WorkerID) {
	now := m.clock.Monotonic()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last[w] = now
}

// Leave removes w immediately (graceful shutdown).
func (m *HeartbeatMembership) Leave(w WorkerID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.last, w)
}

// Live implements Membership. Expired workers are pruned as a side effect.
func (m *HeartbeatMembership) Live() []WorkerID {
	now := m.clock.Monotonic()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkerID, 0, len(m.last))
	for w, at := range m.last {
		if now-at < m.liveness {
			out = append(out, w)
		} else {
			delete(m.last, w)
		}
	}
	slices.Sort(out)
	return out
}
