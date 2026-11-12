# eventstore/pgstore: open questions and decisions

Behavioural decisions shared with every implementation are in
`eventstore/OPEN_QUESTIONS.md`; these are Postgres specific.

1. **Fence mapping.** `engine.runs.fence` is one bigint. `Fence{Term: 0,
   Counter: fence}`; a fence with a non-zero term (or a counter above
   MaxInt64) is mapped to -1 and never matches. Strict monotonicity across
   restarts comes from the durable `fence = fence + 1`.
2. **Two NOTIFY channels.** Spec 9.4 names `engine_wake` for wakeups. The
   store notifies `engine_wake` (payload: run id) when a run becomes ready
   now (CreateRun without WakeAt, Wake, ReleaseLease to a due schedulable
   state) and notifies a separate `engine_events` channel on every append,
   which Subscribe listens on. Putting appends on `engine_wake` would wake
   every idle worker on every event. Both names are configurable.
3. **Subscribe** shares one LISTEN connection per Store and polls every
   `PollInterval` (default 1 s) as a fallback; a lost notification or a
   dropped listener only delays delivery. After reconnecting the listener
   signals every subscription once to catch up.
4. **jsonb normalization.** jsonb reorders keys and rewrites whitespace, so
   Read re-encodes each payload canonically (same bytes as MemStore for
   ordinary documents). Number spellings jsonb rewrites (`1e3` to `1000`,
   `-0` to `0`) stay rewritten: equal by value only.
5. **Connection pool.** `pgxpool` needs `github.com/jackc/puddle/v2`, which
   is not in go.mod (and go.mod is not changed by this package). `Open` uses
   a minimal bounded pool over `pgx.Conn`; `New(db DB, cfg)` accepts any
   value with `Begin`, `Exec`, `Query`, `QueryRow` (a `*pgxpool.Pool` fits).
6. **Append round trips.** BEGIN, `SELECT ... FOR UPDATE`, one statement
   that inserts the events (via `unnest` arrays), advances `last_seq` and
   notifies, COMMIT. A single CTE statement could do it in one round trip
   but would hide the lock-check-write sequence the spec describes.
7. **ListReady's SKIP LOCKED** runs as a single autocommit statement, so its
   row locks last only for the statement: it skips rows a concurrent claim
   or append holds instead of waiting. ClaimReady holds them until its
   UPDATE commits, which is what makes concurrent claims disjoint.
8. **Row-level security.** The store is cross-tenant (scheduler) and must
   connect as the table owner or a `BYPASSRLS` role; RLS is ENABLEd, not
   FORCEd (migrations/OPEN_QUESTIONS.md).
9. **Benchmark scale.** b_es_02 asks for 100k runs and 1k workers. Loading
   100k runs per sub-benchmark through CreateRun takes minutes and 1k
   connections exceed `max_connections`; the Postgres benchmark uses 10k
   runs and up to 64 workers on a 64-connection pool. MemStore runs the full
   scale.
10. **Constraint mapping.** unique violations map to `ErrExists`; foreign
    key, check, not-null and data errors (bad encoding, out of range) to
    `ErrInvalid`. Everything else (connection loss, timeouts) is returned
    wrapped; a partitioned engine sees appends fail and pauses (23.8).
