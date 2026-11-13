# eventstore/pgstore

PostgreSQL implementation of `eventstore.Store` (spec 23.4 implementation
A) over the engine schema from `migrations` (spec 10.4).

```go
s, err := pgstore.Open(ctx, pgstore.Config{DSN: dsn, MaxConns: 16})
defer s.Close()
// or, with a pool you already have (e.g. *pgxpool.Pool):
s, err := pgstore.New(pool, pgstore.Config{DSN: dsn}) // DSN only for LISTEN
```

Run the migrations first (`migrations.Apply`). Creating a run needs an
existing `engine.workflow_versions` row (foreign key).

| Operation | SQL |
|-----------|-----|
| Append | `BEGIN; SELECT last_seq, fence, lease_owner IS NOT NULL ... FOR UPDATE; INSERT ... SELECT FROM unnest(...); UPDATE last_seq; pg_notify('engine_events'); COMMIT` |
| AcquireLease | `UPDATE runs SET fence = fence + 1, lease_owner, lease_expires_at = now() + ttl ... WHERE lease free, expired or ours RETURNING fence` |
| ListReady / ClaimReady | `... FOR UPDATE SKIP LOCKED` ordered by `priority DESC, next_wake_at, id` |
| Wake | `next_wake_at = now(), woken_at = now()`, `pg_notify('engine_wake', id)` |
| Subscribe | batched reads; waits for `engine_events` (one shared LISTEN connection per Store) or the poll interval |

## Tests and benchmarks

Integration tests are skipped unless `FF_TEST_PG_DSN` is set. They create
a private database `ff_pgstore_test_<pid>` on that server, apply the
migrations, insert a workflow fixture and run the whole
`eventstore/storetest` suite (`TestES007_AllImplementations`), plus NOTIFY
and foreign-key mapping checks.

```sh
eval "$(scripts/devpg.sh start)"        # local PostgreSQL 16 on port 55432
go test -race ./eventstore/pgstore/
go test -run x -bench . -benchtime=100x ./eventstore/pgstore/
scripts/devpg.sh stop
```

The suite drives time through `Harness.Elapse`, which for Postgres shifts
`next_wake_at`, `lease_expires_at`, `lease_acquired_at` and `woken_at` back
by d: equivalent to advancing the database clock for every comparison the
store makes, with no sleeping. The random-interleavings property runs 200
iterations against Postgres unless `FF_PROPTEST_CASES` is set.

Benchmarks: `BenchmarkES01_Append` (1/16/256 runs, batches 1/10/100, at
most 64 connections) and `BenchmarkES02_LeaseClaim` (10k runs, 1/16/64
workers).

## Limitations

* `Open` uses a small built-in connection pool (it predates `pgxpool` in
  go.mod); `New` accepts any pool with the `DB` methods, `*pgxpool.Pool`
  included, which is what the engine service passes.
* The store maintains only the scheduling columns of `engine.runs`
  (`status`, `last_seq`, lease, wake, fence); `started_at`, `finished_at`,
  `output` and `failure_*` are the engine's.
* See `OPEN_QUESTIONS.md`.
