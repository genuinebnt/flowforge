# eventstore: open questions and decisions

Each decision is pinned by the conformance suite (eventstore/storetest and
conformance/eventstore/*.json), so every implementation behaves the same.

1. **Stream creation.** The spec trait has no create operation, but the
   Postgres implementation needs an `engine.runs` row. `CreateRun(spec,
   initial)` inserts the run (status pending, ready at `spec.WakeAt` or now)
   and appends `initial` at seq 1..n atomically, without a lease: that is how
   SubmitRun records `run_submitted`. Duplicate id or (tenant, idempotency
   key) is `ErrExists`, mirroring `UNIQUE (tenant_id, idempotency_key)`.
2. **What "fence is current" means.** Append succeeds only if the fence
   equals the run's current fence *and* that lease has not been released.
   An expired lease that nobody replaced still appends (spec 9.4 checks
   `runs.fence = :fence` only; ownership moves when another worker acquires
   a higher fence, not when the clock passes). `ReleaseLease` retires the
   fence. The same rule governs `RenewLease` (renewing an expired but
   unreplaced lease is allowed) and `ReleaseLease`.
3. **Error precedence.** A stale fence is reported as `ErrFenced` even when
   `expectedSeq` is also wrong: the worker must stop (9.4), and a seq
   conflict would invite a retry. Unknown runs are `ErrNotFound` before
   either.
4. **Read and Subscribe bounds.** `fromSeq` is inclusive (`Seq >= fromSeq`),
   0 is treated as 1, `limit <= 0` means no limit, reading past the head is
   an empty result. A consumer that has seen seq k resumes with k+1.
   `Subscribe` follows the run forever (it does not end at a terminal
   event); cancellation and errors are yielded once as `(Event{}, err)`.
5. **Empty append** performs the fence and seq checks and writes nothing.
6. **Event normalization** (shared `PrepareEvents`): `Seq`/`RunID` are
   assigned and, if supplied, must match; `Type` and `Actor` must be
   non-empty valid UTF-8 without NUL; payloads must be valid JSON (nil means
   `{}`) without U+0000 (jsonb cannot store it) and are stored in canonical
   form (`core.CanonicalJSON`); `At` is UTC with microsecond precision
   (Postgres resolution) and a zero `At` is set by the store's clock.
   Postgres jsonb may rewrite number spellings (`1e3` reads back as `1000`);
   payloads are equal by JSON value, not always byte for byte.
7. **Release, wake and lost wakeups.** Spec 9.4 says "on wait: set
   next_wake_at, release lease" and "wakeups set next_wake_at = now()". Done
   naively, a wake that lands while a worker holds the lease is overwritten
   when the worker parks the run with a later wake time. `ReleaseLease(lease,
   Next{Status, WakeAt})` does both steps atomically, and if `Wake` was called
   after the lease was acquired the run becomes due now regardless of
   `WakeAt`. Postgres implements this with two added columns
   (`lease_acquired_at`, `woken_at`, migration 0009). A spurious wake is
   harmless; a lost one could stall a run until its next timer.
8. **Ready set.** `ListReady` lists status pending, running or waiting (the
   `runs_ready` index predicate of 10.4), due (`next_wake_at <= now`) and
   unleased or expired (`lease_expires_at < now`, strictly). Runs in
   cancelling or compensating also need a worker but are not in the spec's
   predicate: the engine keeps such runs in `running` on the row (the log
   holds the detail) or calls `Wake` after setting the status. Open question
   for the spec.
9. **Order.** Priority descending, then `next_wake_at`, then run id, so the
   order is total and identical across implementations. `tenant_fair_rank`
   (9.4) needs per-tenant scheduler state and belongs to the engine.
10. **ClaimReady** is an addition: ListReady followed by AcquireLease races
    between workers; Postgres does both in one `FOR UPDATE SKIP LOCKED`
    statement (b_es_02).
11. **Shards.** `ShardOf(run, n)` is the last four bytes of the UUID
    (random in UUIDv7) modulo n, computable in SQL
    (`('x' || right(replace(id::text,'-',''), 8))::bit(32)::bigint % n`).
    Spec 23.4 B says `hash(run_id) mod S`; for random UUID bits the bytes
    already are a uniform hash. The zero `Shard` selects every run.
12. **Fences in single-node stores** are `(0, n)` with n incremented on
    every acquisition; a replicated log uses the leader term as `Term`.
    A fence with an unknown term never matches.
13. **Snapshots** are not fenced (a snapshot is a cache of replay, 9.1);
    `AtSeq` must be at most the head; saving an existing `AtSeq` keeps the
    first; the newest three are kept (10.7). Deleting snapshots when a run
    finishes (10.7) is the engine's job.
14. **MemStore time.** Lease expiry uses the monotonic clock reading
    (conventions: ownership uses monotonic time); wake times are wall times
    because timers carry absolute `fire_at` values. Property (4) "across
    restarts" is vacuous for MemStore (no durable state); pgstore checks it.
15. **RunID** is an alias of `core.UUID` so ids pass between packages
    without conversion.
