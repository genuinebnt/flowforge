# eventstore

The event-store port of the engine (spec 23.4) and `MemStore`, its
in-memory reference implementation. The Postgres implementation is in
`eventstore/pgstore`; the conformance suite every implementation passes is
in `eventstore/storetest`.

Spec: 9.3 event log, 9.4 scheduling and the single-writer lease with
fencing token, 9.9 crash recovery, 23.4 EventStore abstraction, 23.8
failure scenarios, test ids es_001-es_007, the random-interleavings
property (25.4) and benchmarks b_es_01, b_es_02 (25.8).

## API

```go
type Store interface {
    CreateRun(ctx, spec RunSpec, initial []Event) error
    GetRun(ctx, run RunID) (RunInfo, error)
    Append(ctx, run RunID, expectedSeq uint64, fence Fence, events []Event) (newHead uint64, err error)
    Read(ctx, run RunID, fromSeq uint64, limit int) ([]Event, error)
    Subscribe(ctx, run RunID, fromSeq uint64) iter.Seq2[Event, error]
    SaveSnapshot(ctx, run RunID, snap Snapshot) error
    LoadSnapshot(ctx, run RunID) (Snapshot, bool, error)
    AcquireLease(ctx, run RunID, owner WorkerID, ttl time.Duration) (Lease, error)
    RenewLease(ctx, lease Lease, ttl time.Duration) (Lease, error)
    ReleaseLease(ctx, lease Lease, next Next) error
    ListReady(ctx, shard Shard, limit int) ([]RunID, error)
    ClaimReady(ctx, shard Shard, owner WorkerID, ttl time.Duration, limit int) ([]Lease, error)
    Wake(ctx, run RunID) error
}
```

Errors are sentinels tested with `errors.Is`: `ErrSeqConflict`,
`ErrFenced`, `ErrLeaseHeld`, `ErrNotFound`, `ErrExists`, `ErrInvalid`,
`ErrClosed`. `Fence{Term, Counter}` compares lexicographically.

A worker's loop (spec 9.4) maps onto it as: `ClaimReady` (or `ListReady` +
`AcquireLease`), `LoadSnapshot` + `Read` from `AtSeq+1`, `Append` with the
lease's fence (stop on `ErrFenced`), `RenewLease` every ttl/3,
`ReleaseLease` with `Next{Status, WakeAt}` when the run waits or finishes.
Out-of-band events (timer fired, approval decided, cancel) call `Wake`.

`MemStore` takes a `core.Clock`: lease expiry uses its monotonic reading,
wake times its wall reading.

## Tests and benchmarks

```sh
go test ./eventstore/...                          # MemStore suite (es_001-es_007, property, scripts)
FF_TEST_PG_DSN=postgres://... go test ./eventstore/...   # also the Postgres suite
FF_PROPTEST_CASES=100000 go test -run ESProp ./eventstore/
go test -run x -bench . ./eventstore/...          # BenchmarkES01_Append, BenchmarkES02_LeaseClaim
```

Scripted scenarios live in `conformance/eventstore/*.json`.

## Limitations

* `MemStore.ListReady`/`ClaimReady` scan all runs (O(runs) under one
  mutex): fine for tests and the simulator, not a scheduler for 100k runs.
* Tenant fairness (`tenant_fair_rank`, per-tenant concurrency caps, 9.4)
  is not part of the store; ready runs are ordered by priority, then wake
  time, then id.
* See `OPEN_QUESTIONS.md`.
