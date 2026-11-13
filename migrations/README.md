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
| `0016_v1_bus.sql` | `bus.subscriptions`, `bus.messages`, `bus.deliveries`: the Postgres event bus (services/internal/bus `PgBus`) | 11.7, 23.7 (addition; G-SVC-01) |
| `0017_v1_maintenance.sql` | `notifier.throttle_windows`, `notifier.digest_buffers` (durable alert state), `engine.schedules`, `engine.run_archives`; `engine.forbid_mutation` lets only the archival transaction delete one archived run's events | 15.3, 17, 10.7 (addition; G-LIM-06, G-OPS-10, G-OPS-03) |
| `0018_v1_db_roles.sql` | per-service roles and grants, forced RLS on tenant, partition and child tables, `ff_sync_roles()` | 10.7, 17.2 (addition) |

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

## Database roles (0018)

`cmd/migrate` runs `migrations.Deploy`: `ApplyReport`, then `SyncRoles`
(`SELECT public.ff_sync_roles()`), then `RoleProblems`. It fails the job
when a service role is a superuser, has `BYPASSRLS`, `CREATEROLE` or
`CREATEDB`, owns a service table, or is still in `cloudsqlsuperuser`.
`ff_sync_roles()` is idempotent and catalog-driven, so tables added by later
migrations get the same treatment on the next run (`Apply` also re-runs it
when a file after 0018 was applied).

| Role | Grants (USAGE + SELECT/INSERT/UPDATE/DELETE, sequences) | Row-level security |
|------|------------------|-----|
| `ff_gateway` | `identity` | cross-tenant policy `svc_ff_gateway` on identity tables (login, refresh and API-key lookups precede the tenant) |
| `ff_engine` | `engine` | cross-tenant policy `svc_ff_engine` (runs are keyed by id; `ListReady`/`ClaimReady`, outbox relay) |
| `ff_scope` | `scope` | cross-tenant policy `svc_ff_scope` (outbox relay) |
| `ff_notifier` | `notifier` | cross-tenant policy `svc_ff_notifier` (delivery worker) |
| `ff_graph` | `graph` | **subject to RLS**: graphstore/pg sets `app.tenant_id` per transaction; only `graph.change_events` (relay) has `svc_ff_graph` |
| `ff_similarity` | `graph.embeddings`, `vector_indexes`, `vector_segments` (when 0012 applied) | subject to RLS |
| `ff_maintenance` | every service schema | `svc_ff_maintenance` on every RLS table (retention, partitions, archival); NOLOGIN, the migrator is made a member |

Every service role also gets the shared `bus` schema when it exists (the
local Postgres bus). PUBLIC gets nothing in the service schemas, no CREATE
on `public`, and no access to `public.schema_migrations` or the `ff_`
functions. Roles that do not exist are created `NOLOGIN` (local and test
clusters); on Cloud SQL, Terraform creates them as login users (`ff_<name>`
for each entry of `db_users`) and `ff_sync_roles()` removes them from
`cloudsqlsuperuser`. The names match `infra/envs/*/main.tf` `db_users`;
`ff_migrator` is the migrations job itself and owns the tables.

Every table with a `tenant_id` column in a service schema, each partition
of `scope.scope_decisions` (including the ones
`scope.ensure_decision_partition` adds later; it is now `SECURITY DEFINER`)
and the child tables listed in 0018 (`engine.run_events`, `effects`,
`timers`, `snapshots`, `run_ext`, `run_requests`, `approval_ext`,
`workflow_versions`, `workflow_version_ext`, `scope.scope_versions`,
`notifier.deliveries`) have `ENABLE` + `FORCE ROW LEVEL SECURITY`. Child
tables use `parent_visible`: a row is visible when its parent row is. The
cross-tenant exemptions are role-scoped permissive policies rather than the
`BYPASSRLS` attribute; see `OPEN_QUESTIONS.md` 12.

Locally (`flowforge dev`, tests) everything runs as the `postgres`
superuser, which RLS never restricts; `TestRegress_G_OPS_02_*` in
`eventstore/pgstore`, `engine/pgrepo` and `graphstore/pg` run their suites
as `ff_engine` and `ff_graph` through the `role` startup parameter
(`options=-c role=ff_engine`).

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
  migrations; they connect as (or `SET ROLE` to) `ff_maintenance`.
* The engine, scope, notifier and identity stores do not set
  `app.tenant_id`, so their roles hold a cross-tenant policy on their own
  schema; RLS restricts them only outside it (where grants already deny).
* See `OPEN_QUESTIONS.md` for the spec ambiguities and the choices made.
