package enginetest

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/eventstore"
)

// ErrCrashed is returned by every CrashStore call after the crash point.
var ErrCrashed = errors.New("enginetest: simulated worker crash")

// CrashStore wraps a store and simulates the death of the worker using it:
// after a given number of successful appends, the next append and every
// later call fail with ErrCrashed and nothing more reaches the store. The
// lease the worker held stays until it expires, as after a real crash.
type CrashStore struct {
	eventstore.Store
	mu      sync.Mutex
	left    int // appends still allowed; -1: unlimited
	appends int
	crashed bool
	// BeforeAppend, if set, runs before each forwarded append.
	BeforeAppend func(events []eventstore.Event)
}

// NewCrashStore crashes on append number after+1 (after < 0: never).
func NewCrashStore(s eventstore.Store, after int) *CrashStore {
	return &CrashStore{Store: s, left: after}
}

// Crashed reports whether the crash point was reached.
func (c *CrashStore) Crashed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crashed
}

// Appends returns the number of successful appends.
func (c *CrashStore) Appends() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appends
}

// Crash makes every later call fail.
func (c *CrashStore) Crash() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.crashed = true
}

func (c *CrashStore) check() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crashed {
		return ErrCrashed
	}
	return nil
}

// Append implements eventstore.Store.
func (c *CrashStore) Append(ctx context.Context, run eventstore.RunID, expectedSeq uint64, fence eventstore.Fence, events []eventstore.Event) (uint64, error) {
	c.mu.Lock()
	if c.crashed {
		c.mu.Unlock()
		return 0, ErrCrashed
	}
	if c.left == 0 {
		c.crashed = true
		c.mu.Unlock()
		return 0, ErrCrashed
	}
	if c.left > 0 {
		c.left--
	}
	c.mu.Unlock()
	if c.BeforeAppend != nil {
		c.BeforeAppend(events)
	}
	head, err := c.Store.Append(ctx, run, expectedSeq, fence, events)
	if err == nil {
		c.mu.Lock()
		c.appends++
		c.mu.Unlock()
	}
	return head, err
}

// CreateRun implements eventstore.Store.
func (c *CrashStore) CreateRun(ctx context.Context, spec eventstore.RunSpec, initial []eventstore.Event) error {
	if err := c.check(); err != nil {
		return err
	}
	return c.Store.CreateRun(ctx, spec, initial)
}

// GetRun implements eventstore.Store.
func (c *CrashStore) GetRun(ctx context.Context, run eventstore.RunID) (eventstore.RunInfo, error) {
	if err := c.check(); err != nil {
		return eventstore.RunInfo{}, err
	}
	return c.Store.GetRun(ctx, run)
}

// Read implements eventstore.Store.
func (c *CrashStore) Read(ctx context.Context, run eventstore.RunID, fromSeq uint64, limit int) ([]eventstore.Event, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	return c.Store.Read(ctx, run, fromSeq, limit)
}

// Subscribe implements eventstore.Store.
func (c *CrashStore) Subscribe(ctx context.Context, run eventstore.RunID, fromSeq uint64) iter.Seq2[eventstore.Event, error] {
	return c.Store.Subscribe(ctx, run, fromSeq)
}

// SaveSnapshot implements eventstore.Store.
func (c *CrashStore) SaveSnapshot(ctx context.Context, run eventstore.RunID, snap eventstore.Snapshot) error {
	if err := c.check(); err != nil {
		return err
	}
	return c.Store.SaveSnapshot(ctx, run, snap)
}

// LoadSnapshot implements eventstore.Store.
func (c *CrashStore) LoadSnapshot(ctx context.Context, run eventstore.RunID) (eventstore.Snapshot, bool, error) {
	if err := c.check(); err != nil {
		return eventstore.Snapshot{}, false, err
	}
	return c.Store.LoadSnapshot(ctx, run)
}

// DeleteSnapshots implements eventstore.Store.
func (c *CrashStore) DeleteSnapshots(ctx context.Context, run eventstore.RunID) error {
	if err := c.check(); err != nil {
		return err
	}
	return c.Store.DeleteSnapshots(ctx, run)
}

// AcquireLease implements eventstore.Store.
func (c *CrashStore) AcquireLease(ctx context.Context, run eventstore.RunID, owner eventstore.WorkerID, ttl time.Duration) (eventstore.Lease, error) {
	if err := c.check(); err != nil {
		return eventstore.Lease{}, err
	}
	return c.Store.AcquireLease(ctx, run, owner, ttl)
}

// RenewLease implements eventstore.Store.
func (c *CrashStore) RenewLease(ctx context.Context, lease eventstore.Lease, ttl time.Duration) (eventstore.Lease, error) {
	if err := c.check(); err != nil {
		return eventstore.Lease{}, err
	}
	return c.Store.RenewLease(ctx, lease, ttl)
}

// ReleaseLease implements eventstore.Store.
func (c *CrashStore) ReleaseLease(ctx context.Context, lease eventstore.Lease, next eventstore.Next) error {
	if err := c.check(); err != nil {
		return err
	}
	return c.Store.ReleaseLease(ctx, lease, next)
}

// ListReady implements eventstore.Store.
func (c *CrashStore) ListReady(ctx context.Context, shard eventstore.Shard, limit int) ([]eventstore.RunID, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	return c.Store.ListReady(ctx, shard, limit)
}

// ClaimReady implements eventstore.Store.
func (c *CrashStore) ClaimReady(ctx context.Context, shard eventstore.Shard, owner eventstore.WorkerID, ttl time.Duration, limit int) ([]eventstore.Lease, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	return c.Store.ClaimReady(ctx, shard, owner, ttl, limit)
}

// Wake implements eventstore.Store.
func (c *CrashStore) Wake(ctx context.Context, run eventstore.RunID) error {
	if err := c.check(); err != nil {
		return err
	}
	return c.Store.Wake(ctx, run)
}
