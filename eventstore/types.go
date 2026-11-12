package eventstore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Sentinel errors. Implementations wrap them with %w and context; callers
// test with errors.Is.
var (
	// ErrSeqConflict: the stream head is not the expectedSeq named by an
	// append. Nothing was written.
	ErrSeqConflict = errors.New("eventstore: sequence conflict")
	// ErrFenced: the fence is not the run's current fence, or the lease it
	// belongs to was released. Nothing was written. A worker that sees it
	// lost the lease and MUST stop advancing the run (spec 9.4).
	ErrFenced = errors.New("eventstore: fenced")
	// ErrLeaseHeld: another worker holds an unexpired lease on the run.
	ErrLeaseHeld = errors.New("eventstore: lease held by another worker")
	// ErrNotFound: the run does not exist.
	ErrNotFound = errors.New("eventstore: run not found")
	// ErrExists: CreateRun found a run with the same id, or the same
	// (tenant, idempotency key).
	ErrExists = errors.New("eventstore: run already exists")
	// ErrInvalid: the arguments are malformed (empty type or actor,
	// invalid JSON payload, mismatched seq, non-positive TTL, ...).
	ErrInvalid = errors.New("eventstore: invalid argument")
	// ErrClosed: the store was closed.
	ErrClosed = errors.New("eventstore: store closed")
)

// RunID identifies a run, and therefore an event stream.
type RunID = core.UUID

// WorkerID identifies a worker process that leases runs.
type WorkerID string

// Fence is the fencing token of a lease: (term, counter), compared
// lexicographically (spec 23.4). Every successful lease acquisition yields
// a fence strictly greater than every earlier fence of the same run. A
// single-node store (MemStore, Postgres) keeps Term at 0 and increments
// Counter; a replicated log uses the leader term as Term.
type Fence struct {
	Term    uint64 `json:"term"`
	Counter uint64 `json:"counter"`
}

// Compare returns -1, 0 or +1 as f is less than, equal to or greater than o.
func (f Fence) Compare(o Fence) int {
	switch {
	case f.Term < o.Term:
		return -1
	case f.Term > o.Term:
		return 1
	case f.Counter < o.Counter:
		return -1
	case f.Counter > o.Counter:
		return 1
	}
	return 0
}

// Less reports whether f orders before o.
func (f Fence) Less(o Fence) bool { return f.Compare(o) < 0 }

// IsZero reports whether f is the zero fence, which no lease ever carries.
func (f Fence) IsZero() bool { return f == Fence{} }

// String renders the fence as "term.counter".
func (f Fence) String() string { return fmt.Sprintf("%d.%d", f.Term, f.Counter) }

// Event is one entry of a run's event log (spec 9.3). Seq starts at 1 and
// is gapless per run. Actor is "engine", "vm", "tool:<name>" or
// "user:<id>".
type Event struct {
	RunID   RunID           `json:"run_id"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// Lease grants Owner the right to append to Run while Fence is the run's
// current fence. ExpiresAt is informational: expiry is decided by the
// store's clock (the database clock for Postgres), never the worker's.
type Lease struct {
	Run       RunID
	Owner     WorkerID
	Fence     Fence
	ExpiresAt time.Time
}

// Shard selects a subset of runs: those with ShardOf(run, Count) == Index.
// The zero value (Count 0) selects every run.
type Shard struct {
	Index uint32
	Count uint32
}

// AllShards selects every run.
var AllShards = Shard{}

// ShardOf maps a run to one of count shards. It uses the last four bytes
// of the UUID, which are random in a version 7 UUID, so the mapping is
// uniform and can be computed in SQL. count 0 or 1 maps every run to 0.
func ShardOf(run RunID, count uint32) uint32 {
	if count <= 1 {
		return 0
	}
	return binary.BigEndian.Uint32(run[12:16]) % count
}

// Contains reports whether the shard selects run.
func (s Shard) Contains(run RunID) bool {
	return s.Count == 0 || ShardOf(run, s.Count) == s.Index
}

// Valid reports whether the shard is well formed.
func (s Shard) Valid() bool { return s.Count == 0 || s.Index < s.Count }

// RunStatus is the run state stored on the run row (spec 9.2). The event
// log is authoritative; the status is a projection kept for scheduling and
// querying.
type RunStatus string

// Run statuses (spec 9.2).
const (
	StatusPending      RunStatus = "pending"
	StatusRunning      RunStatus = "running"
	StatusWaiting      RunStatus = "waiting"
	StatusPaused       RunStatus = "paused"
	StatusCancelling   RunStatus = "cancelling"
	StatusCompensating RunStatus = "compensating"
	StatusSucceeded    RunStatus = "succeeded"
	StatusFailed       RunStatus = "failed"
	StatusCancelled    RunStatus = "cancelled"
)

// Valid reports whether s is one of the defined statuses.
func (s RunStatus) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusWaiting, StatusPaused, StatusCancelling,
		StatusCompensating, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Schedulable reports whether a run in status s is picked up by ListReady
// once due (spec 9.4: pending, running, waiting).
func (s RunStatus) Schedulable() bool {
	return s == StatusPending || s == StatusRunning || s == StatusWaiting
}

// RunSpec describes a run to create. It mirrors the identifying columns of
// engine.runs (spec 10.4); the status starts as pending.
type RunSpec struct {
	ID              RunID
	Tenant          core.UUID
	Program         core.UUID // zero: none
	WorkflowVersion core.UUID
	Parent          RunID // zero: none
	ScopeVersion    int32 // 0: none
	CreatedBy       string
	IdempotencyKey  string
	RequestHash     []byte
	Priority        int16
	Input           json.RawMessage // nil: {}
	Deadline        time.Time       // zero: none
	// WakeAt is when the run first becomes ready; zero means immediately.
	WakeAt time.Time
}

// RunInfo is the scheduling state of a run.
type RunInfo struct {
	ID     RunID
	Tenant core.UUID
	Status RunStatus
	// Head is the seq of the last event (0 for an empty stream).
	Head       uint64
	Priority   int16
	NextWakeAt time.Time
	// Fence is the run's current fence (zero before the first lease).
	Fence Fence
	// LeaseOwner is empty when no lease is held (never acquired or
	// released); an expired lease keeps its owner until replaced.
	LeaseOwner     WorkerID
	LeaseExpiresAt time.Time
}

// Snapshot is a cached VM state at a given seq (spec 9.1). Dropping all
// snapshots never changes behaviour.
type Snapshot struct {
	AtSeq        uint64
	ModuleSHA256 []byte
	State        []byte
}

// SnapshotsKept is how many snapshots per run a store retains (spec 10.7).
const SnapshotsKept = 3

// Next tells ReleaseLease how to leave the run.
type Next struct {
	// Status, when set, replaces the run status (for example waiting, or a
	// terminal status after the terminal event was appended).
	Status RunStatus
	// WakeAt is when the run becomes ready again; zero means now. A wake
	// requested with Store.Wake while the lease was held overrides it with
	// now, so an out-of-band wakeup is never lost.
	WakeAt time.Time
}
