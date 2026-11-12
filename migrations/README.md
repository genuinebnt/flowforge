# migrations

Forward-only SQL migrations for the Flowforge database (spec section 10)
and a Go runner (`migrations.Apply`).

| File | Content | Spec |
|------|---------|------|
| `0001_extensions.sql` | `pg_trgm`, `citext` | 10 |
| `0002_identity.sql` | identity schema | 10.2 |
| `0003_scope.sql` | scope schema | 10.3 |
| `0004_engine.sql` | engine schema | 10.4 |
| `0005_graph.sql` | graph schema, `graph.snapshots` | 10.5, 21 |
| `0006_notifier.sql` | notifier schema | 10.6 |
| `0007_rls.sql` | row-level security on tenant-scoped tables | 10.7 |
| `0008_outbox_inbox.sql` | `identity.outbox`, `scope.outbox`, `engine.processed_events` | 23.7 (addition) |
| `0009_engine_workers.sql` | `engine.workers` heartbeats; `runs.lease_acquired_at`, `runs.woken_at`; ready index | 23.3, 23.4 (addition) |
| `0010_gateway_idempotency.sql` | `identity.http_idempotency` (7-day retention) | 12.1, 9.10 (addition) |
| `0011_scope_decision_partitions.sql` | default partition, `scope.ensure_decision_partition(month)` | 10.3 (addition) |
| `0012_graph_vectors.sql` | `graph.embeddings`, `vector_indexes`, `vector_segments` (needs pgvector) | 22.9 |
| `0013_engine_extras.sql` | `engine.workflow_version_ext`, `run_ext`, `run_requests`, `approval_ext` (engine/pgrepo's SchemaSQL, idempotent) | 9, 10.4 (addition) |
| `0014_notifier_delivery_target.sql` | `notifier.deliveries.target` (Slack channel override / e-mail recipient of an action) | 15.4 (addition) |

## Usage

```go
conn, _ := pgx.Connect(ctx, dsn)       // or a *pgxpool.Pool
err := migrations.Apply(ctx, conn)     // idempotent, safe to run concurrently
res, err := migrations.ApplyReport(ctx, conn) // per-file outcome
```

A file whose first line is `-- requires-extension: <name>` is applied only
when `pg_available_extensions` lists that extension; otherwise it is
recorded as `skipped` and applied by a later run once the extension is
installed. An applied file must never change (checksum check); fix forward
with a new file.

## Tests

```sh
go test ./migrations/...                        # file-set checks only
FF_TEST_PG_DSN=postgres://... go test ./migrations/...
```

With `FF_TEST_PG_DSN` the tests create throwaway databases on that server,
run four migrators concurrently, re-apply (no changes), check the
checksum guard, the append-only trigger on `engine.run_events`, the
partition helper and the RLS policies (with a temporary non-owner role).
`scripts/devpg.sh start` starts a local PostgreSQL 16 cluster for this.

## Limitations

* Retention jobs (10.7) and the monthly partition job are not part of the
  migrations; the helper function exists, the scheduler does not.
* See `OPEN_QUESTIONS.md` for the spec ambiguities and the choices made.
