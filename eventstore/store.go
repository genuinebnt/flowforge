package eventstore

import (
	"context"
	"iter"
	"time"
)

// Store is the event-store port (spec 23.4). All methods are safe for
// concurrent use. Implementations: MemStore (reference, in memory) and
// pgstore.Store (Postgres, spec 23.4 implementation A). The conformance
// suite in eventstore/storetest pins the behaviour described here.
type Store interface {
	// CreateRun creates a run in status pending and its event stream. The
	// initial events, if any, are appended atomically with the run at
	// seq 1..n without a lease (the engine records run_submitted this
	// way). It fails with ErrExists if the id or the (tenant, idempotency
	// key) pair is taken, and ErrInvalid for a malformed spec or event.
	CreateRun(ctx context.Context, spec RunSpec, initial []Event) error

	// GetRun returns the scheduling state of a run, or ErrNotFound.
	GetRun(ctx context.Context, run RunID) (RunInfo, error)

	// Append atomically appends events if the stream head equals
	// expectedSeq AND fence is the run's current fence held by a lease
	// that has not been released (spec 23.4). It returns the new head,
	// expectedSeq+len(events). Event.Seq and Event.RunID may be left zero;
	// if set they must match the assigned values. A zero Event.At is set
	// by the store's clock. Payloads must be valid JSON (nil means {}) and
	// are stored in canonical form.
	//
	// Errors: ErrFenced (checked first: a stale worker must stop even if
	// its expectedSeq happens to match), ErrSeqConflict, ErrNotFound,
	// ErrInvalid. On error nothing is written. An empty batch performs
	// the checks and writes nothing.
	//
	// A lease that expired without being replaced still fences correctly:
	// its holder may keep appending until another worker acquires a new
	// fence.
	Append(ctx context.Context, run RunID, expectedSeq uint64, fence Fence, events []Event) (uint64, error)

	// Read returns up to limit events with Seq >= fromSeq in seq order
	// (limit <= 0: no limit). fromSeq 0 is treated as 1. Reading past the
	// head returns no events and no error; an unknown run is ErrNotFound.
	Read(ctx context.Context, run RunID, fromSeq uint64, limit int) ([]Event, error)

	// Subscribe streams the run's events with Seq >= fromSeq, in order,
	// each exactly once, and then follows new appends until ctx is done or
	// the consumer stops iterating. Resuming from the last seen seq + 1
	// continues without gaps or duplicates. A failure (including ctx
	// cancellation and ErrNotFound) is yielded once as (Event{}, err),
	// after which the sequence ends.
	Subscribe(ctx context.Context, run RunID, fromSeq uint64) iter.Seq2[Event, error]

	// SaveSnapshot stores a snapshot at snap.AtSeq <= head and keeps only
	// the newest SnapshotsKept snapshots of the run. Saving the same AtSeq
	// twice keeps the first (snapshots are deterministic caches).
	SaveSnapshot(ctx context.Context, run RunID, snap Snapshot) error

	// LoadSnapshot returns the snapshot with the highest AtSeq; ok is
	// false when the run has none.
	LoadSnapshot(ctx context.Context, run RunID) (snap Snapshot, ok bool, err error)

	// AcquireLease grants owner a lease for ttl if the run is unleased,
	// its lease expired, or owner already holds it (re-acquiring yields a
	// new fence and stales the old one). The returned fence is strictly
	// greater than any previous fence of the run, across restarts.
	// Errors: ErrLeaseHeld, ErrNotFound, ErrInvalid.
	AcquireLease(ctx context.Context, run RunID, owner WorkerID, ttl time.Duration) (Lease, error)

	// RenewLease extends a lease whose fence is still current to now+ttl.
	// Errors: ErrFenced (the lease was replaced or released), ErrNotFound.
	RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)

	// ReleaseLease gives up a lease whose fence is still current and sets
	// the run's status and next wake time from next. The fence stays the
	// current fence but can no longer append. Errors: ErrFenced,
	// ErrNotFound, ErrInvalid.
	ReleaseLease(ctx context.Context, lease Lease, next Next) error

	// ListReady returns up to limit runs of the shard that are ready for
	// work: schedulable status, next wake time due, and unleased or lease
	// expired. Order: priority descending, then next wake time, then id.
	ListReady(ctx context.Context, shard Shard, limit int) ([]RunID, error)

	// ClaimReady atomically leases up to limit ready runs of the shard to
	// owner (ListReady plus AcquireLease without the race in between;
	// Postgres uses FOR UPDATE SKIP LOCKED so concurrent claimers never
	// block each other or claim the same run). Result order is ListReady
	// order.
	ClaimReady(ctx context.Context, shard Shard, owner WorkerID, ttl time.Duration, limit int) ([]Lease, error)

	// Wake makes the run due now (timer fired, effect completed out of
	// band, approval decided, cancel requested, scope changed; spec 9.4)
	// and notifies waiting workers. If a lease is held, the wake is
	// remembered and applied when the lease is released.
	Wake(ctx context.Context, run RunID) error
}
