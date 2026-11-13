package eventstore

import (
	"bytes"
	"cmp"
	"container/heap"
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// MemStore is the in-memory reference Store. It is mutex protected and
// takes time from an injected core.Clock: lease expiry uses the monotonic
// reading, wake times the wall reading. Its fences are (0, n) like
// Postgres. State lives only as long as the value, so property (4) "across
// restarts" is vacuous for it.
type MemStore struct {
	clock core.Clock

	mu   sync.Mutex
	runs map[RunID]*memRun
	idem map[idemKey]RunID
	// gen orders lease acquisitions against wakes (see memRun.wokenGen).
	gen uint64
}

type idemKey struct {
	tenant core.UUID
	key    string
}

type memRun struct {
	spec       RunSpec
	status     RunStatus
	events     []Event
	nextWake   time.Time
	fence      uint64
	owner      WorkerID
	leaseUntil time.Duration // monotonic deadline
	leaseWall  time.Time     // informational ExpiresAt
	acquired   uint64        // gen at the last acquisition
	wokenGen   uint64        // gen at the last Wake (0: never)
	snapshots  []Snapshot    // ascending AtSeq, at most SnapshotsKept
	// notify is closed and replaced on every append.
	notify chan struct{}
}

// NewMemStore returns an empty MemStore. A nil clock means the system clock.
func NewMemStore(clock core.Clock) *MemStore {
	if clock == nil {
		clock = &core.SystemClock{}
	}
	return &MemStore{clock: clock, runs: map[RunID]*memRun{}, idem: map[idemKey]RunID{}}
}

var _ Store = (*MemStore)(nil)

func (m *MemStore) get(run RunID) (*memRun, error) {
	r, ok := m.runs[run]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, run)
	}
	return r, nil
}

// leased reports whether r has an unexpired lease.
func (m *MemStore) leased(r *memRun) bool {
	return r.owner != "" && m.clock.Monotonic() <= r.leaseUntil
}

func (m *MemStore) fillTimes(evs []Event) {
	now := m.clock.Now().UTC().Truncate(time.Microsecond)
	for i := range evs {
		if evs[i].At.IsZero() {
			evs[i].At = now
		}
	}
}

// CreateRun implements Store.
func (m *MemStore) CreateRun(ctx context.Context, spec RunSpec, initial []Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spec, err := PrepareRunSpec(spec)
	if err != nil {
		return err
	}
	evs, err := PrepareEvents(spec.ID, 0, initial)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[spec.ID]; ok {
		return fmt.Errorf("%w: id %s", ErrExists, spec.ID)
	}
	k := idemKey{spec.Tenant, spec.IdempotencyKey}
	if other, ok := m.idem[k]; ok {
		return fmt.Errorf("%w: idempotency key %q is used by run %s", ErrExists, spec.IdempotencyKey, other)
	}
	if !spec.Parent.IsZero() {
		if _, ok := m.runs[spec.Parent]; !ok {
			return fmt.Errorf("%w: parent run %s does not exist", ErrInvalid, spec.Parent)
		}
	}
	m.fillTimes(evs)
	wake := spec.WakeAt
	if wake.IsZero() {
		wake = m.clock.Now().UTC().Truncate(time.Microsecond)
	}
	m.runs[spec.ID] = &memRun{
		spec: spec, status: StatusPending, events: evs, nextWake: wake, notify: make(chan struct{}),
	}
	m.idem[k] = spec.ID
	return nil
}

// GetRun implements Store.
func (m *MemStore) GetRun(ctx context.Context, run RunID) (RunInfo, error) {
	if err := ctx.Err(); err != nil {
		return RunInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return RunInfo{}, err
	}
	info := RunInfo{
		ID: run, Tenant: r.spec.Tenant, Status: r.status, Head: uint64(len(r.events)),
		Priority: r.spec.Priority, NextWakeAt: r.nextWake, Fence: Fence{Counter: r.fence},
		LeaseOwner: r.owner,
	}
	if r.owner != "" {
		info.LeaseExpiresAt = r.leaseWall
	}
	return info, nil
}

// Append implements Store.
func (m *MemStore) Append(ctx context.Context, run RunID, expectedSeq uint64, fence Fence, events []Event) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := CheckSeq("expected seq", expectedSeq); err != nil {
		return 0, err
	}
	evs, err := PrepareEvents(run, expectedSeq, events)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return 0, err
	}
	cur := Fence{Counter: r.fence}
	if r.owner == "" || fence != cur {
		return 0, fmt.Errorf("%w: run %s fence %s, current %s (held: %t)", ErrFenced, run, fence, cur, r.owner != "")
	}
	head := uint64(len(r.events))
	if head != expectedSeq {
		return 0, fmt.Errorf("%w: run %s expected seq %d, head %d", ErrSeqConflict, run, expectedSeq, head)
	}
	if len(evs) == 0 {
		return head, nil
	}
	m.fillTimes(evs)
	r.events = append(r.events, evs...)
	close(r.notify)
	r.notify = make(chan struct{})
	return uint64(len(r.events)), nil
}

func cloneEvent(ev Event) Event {
	ev.Payload = bytes.Clone(ev.Payload)
	return ev
}

// Read implements Store.
func (m *MemStore) Read(ctx context.Context, run RunID, fromSeq uint64, limit int) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return nil, err
	}
	return readLocked(r, fromSeq, limit), nil
}

func readLocked(r *memRun, fromSeq uint64, limit int) []Event {
	fromSeq = max(fromSeq, 1)
	head := uint64(len(r.events))
	if fromSeq > head {
		return []Event{}
	}
	evs := r.events[fromSeq-1:]
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}
	out := make([]Event, len(evs))
	for i, ev := range evs {
		out[i] = cloneEvent(ev)
	}
	return out
}

// subscribeBatch bounds how many events Subscribe copies per lock hold.
const subscribeBatch = 256

// Subscribe implements Store.
func (m *MemStore) Subscribe(ctx context.Context, run RunID, fromSeq uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		next := max(fromSeq, 1)
		for {
			if err := ctx.Err(); err != nil {
				yield(Event{}, err)
				return
			}
			m.mu.Lock()
			r, err := m.get(run)
			if err != nil {
				m.mu.Unlock()
				yield(Event{}, err)
				return
			}
			batch := readLocked(r, next, subscribeBatch)
			wait := r.notify
			m.mu.Unlock()
			for _, ev := range batch {
				if !yield(ev, nil) {
					return
				}
				next = ev.Seq + 1
			}
			if len(batch) > 0 {
				continue
			}
			select {
			case <-wait:
			case <-ctx.Done():
				yield(Event{}, ctx.Err())
				return
			}
		}
	}
}

// SaveSnapshot implements Store.
func (m *MemStore) SaveSnapshot(ctx context.Context, run RunID, snap Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckSeq("snapshot seq", snap.AtSeq); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return err
	}
	if snap.AtSeq > uint64(len(r.events)) {
		return fmt.Errorf("%w: snapshot at seq %d beyond head %d", ErrInvalid, snap.AtSeq, len(r.events))
	}
	i, found := slices.BinarySearchFunc(r.snapshots, snap.AtSeq, func(s Snapshot, at uint64) int {
		return cmp.Compare(s.AtSeq, at)
	})
	if found {
		return nil
	}
	snap = Snapshot{
		AtSeq: snap.AtSeq, ModuleSHA256: bytes.Clone(nonNil(snap.ModuleSHA256)), State: bytes.Clone(nonNil(snap.State)),
	}
	r.snapshots = slices.Insert(r.snapshots, i, snap)
	if n := len(r.snapshots); n > SnapshotsKept {
		r.snapshots = slices.Delete(r.snapshots, 0, n-SnapshotsKept)
	}
	return nil
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// LoadSnapshot implements Store.
func (m *MemStore) LoadSnapshot(ctx context.Context, run RunID) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return Snapshot{}, false, err
	}
	if len(r.snapshots) == 0 {
		return Snapshot{}, false, nil
	}
	s := r.snapshots[len(r.snapshots)-1]
	return Snapshot{AtSeq: s.AtSeq, ModuleSHA256: bytes.Clone(s.ModuleSHA256), State: bytes.Clone(s.State)}, true, nil
}

// DeleteSnapshots implements Store.
func (m *MemStore) DeleteSnapshots(ctx context.Context, run RunID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return err
	}
	r.snapshots = nil
	return nil
}

// acquireLocked leases r to owner unconditionally.
func (m *MemStore) acquireLocked(run RunID, r *memRun, owner WorkerID, ttl time.Duration) Lease {
	m.gen++
	r.fence++
	r.owner = owner
	r.leaseUntil = m.clock.Monotonic() + ttl
	r.leaseWall = m.clock.Now().UTC().Add(ttl).Truncate(time.Microsecond)
	r.acquired = m.gen
	return Lease{Run: run, Owner: owner, Fence: Fence{Counter: r.fence}, ExpiresAt: r.leaseWall}
}

// AcquireLease implements Store.
func (m *MemStore) AcquireLease(ctx context.Context, run RunID, owner WorkerID, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if err := CheckWorker(owner); err != nil {
		return Lease{}, err
	}
	if err := CheckTTL(ttl); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return Lease{}, err
	}
	if m.leased(r) && r.owner != owner {
		return Lease{}, fmt.Errorf("%w: run %s is leased to %s", ErrLeaseHeld, run, r.owner)
	}
	return m.acquireLocked(run, r, owner, ttl), nil
}

// current returns r if lease is its current, unreleased lease.
func (m *MemStore) current(lease Lease) (*memRun, error) {
	r, err := m.get(lease.Run)
	if err != nil {
		return nil, err
	}
	if r.owner == "" || r.owner != lease.Owner || lease.Fence != (Fence{Counter: r.fence}) {
		return nil, fmt.Errorf("%w: run %s lease %s of %s is not current", ErrFenced, lease.Run, lease.Fence, lease.Owner)
	}
	return r, nil
}

// RenewLease implements Store.
func (m *MemStore) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if err := CheckTTL(ttl); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.current(lease)
	if err != nil {
		return Lease{}, err
	}
	r.leaseUntil = m.clock.Monotonic() + ttl
	r.leaseWall = m.clock.Now().UTC().Add(ttl).Truncate(time.Microsecond)
	lease.ExpiresAt = r.leaseWall
	return lease, nil
}

// ReleaseLease implements Store.
func (m *MemStore) ReleaseLease(ctx context.Context, lease Lease, next Next) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckNext(next); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.current(lease)
	if err != nil {
		return err
	}
	now := m.clock.Now().UTC().Truncate(time.Microsecond)
	r.owner, r.leaseUntil, r.leaseWall = "", 0, time.Time{}
	if next.Status != "" {
		r.status = next.Status
	}
	switch {
	case r.wokenGen > r.acquired, next.WakeAt.IsZero():
		r.nextWake = now
	default:
		r.nextWake = next.WakeAt.UTC().Truncate(time.Microsecond)
	}
	return nil
}

// ready reports whether r is ready for work at wall time now and
// monotonic reading mono.
func ready(r *memRun, now time.Time, mono time.Duration) bool {
	return r.status.Schedulable() && !r.nextWake.After(now) && (r.owner == "" || mono > r.leaseUntil)
}

// readyLocked returns up to limit ready runs of the shard in ListReady order.
func (m *MemStore) readyLocked(shard Shard, limit int) []RunID {
	if limit <= 0 {
		return []RunID{}
	}
	now, mono := m.clock.Now(), m.clock.Monotonic()
	h := &readyHeap{}
	for id, r := range m.runs {
		if !ready(r, now, mono) || !shard.Contains(id) {
			continue
		}
		e := readyEntry{id: id, prio: r.spec.Priority, wake: r.nextWake}
		if h.Len() < limit {
			heap.Push(h, e)
		} else if e.before((*h)[0]) {
			(*h)[0] = e
			heap.Fix(h, 0)
		}
	}
	out := make([]RunID, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(readyEntry).id
	}
	return out
}

// ListReady implements Store.
func (m *MemStore) ListReady(ctx context.Context, shard Shard, limit int) ([]RunID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckShard(shard); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readyLocked(shard, limit), nil
}

// ClaimReady implements Store.
func (m *MemStore) ClaimReady(ctx context.Context, shard Shard, owner WorkerID, ttl time.Duration, limit int) ([]Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckShard(shard); err != nil {
		return nil, err
	}
	if err := CheckWorker(owner); err != nil {
		return nil, err
	}
	if err := CheckTTL(ttl); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.readyLocked(shard, limit)
	out := make([]Lease, len(ids))
	for i, id := range ids {
		out[i] = m.acquireLocked(id, m.runs[id], owner, ttl)
	}
	return out, nil
}

// Wake implements Store.
func (m *MemStore) Wake(ctx context.Context, run RunID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.get(run)
	if err != nil {
		return err
	}
	m.gen++
	r.wokenGen = m.gen
	r.nextWake = m.clock.Now().UTC().Truncate(time.Microsecond)
	return nil
}

// readyEntry and readyHeap keep the best `limit` ready runs: the heap root
// is the worst of the kept entries.
type readyEntry struct {
	id   RunID
	prio int16
	wake time.Time
}

// before reports whether e orders before o in ListReady order.
func (e readyEntry) before(o readyEntry) bool {
	if e.prio != o.prio {
		return e.prio > o.prio
	}
	if !e.wake.Equal(o.wake) {
		return e.wake.Before(o.wake)
	}
	return bytes.Compare(e.id[:], o.id[:]) < 0
}

type readyHeap []readyEntry

func (h readyHeap) Len() int           { return len(h) }
func (h readyHeap) Less(i, j int) bool { return h[j].before(h[i]) }
func (h readyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *readyHeap) Push(x any)        { *h = append(*h, x.(readyEntry)) }
func (h *readyHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}
