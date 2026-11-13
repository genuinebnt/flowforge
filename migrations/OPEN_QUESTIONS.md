# migrations: open questions, deviations and additions

Files 0002-0006 transcribe the spec DDL verbatim (sections 10.2-10.6, plus
`graph.snapshots` from section 21 in 0005 and the pgvector tables from
22.9 in 0012). Everything else below is an addition or a decision the spec
leaves open.

## Runner

1. **Bookkeeping table.** `public.schema_migrations (version, name,
   checksum, status, applied_at)`. It lives in `public` because migrations
   span every service schema (10: "each service owns one schema").
2. **Concurrency.** Each file runs in its own transaction holding
   `pg_advisory_xact_lock(0x666c6f77666f7267)`; the "already applied?"
   check happens under the lock, so concurrent migrators serialize file by
   file and every one of them succeeds. A transaction-level lock (instead of
   a session lock held across files) works through connection pools.
3. **Edited files** are refused (`ErrChecksumMismatch`): migrations are
   forward-only (10.7).
4. **Database ahead of the binary.** Versions recorded in the database but
   unknown to the binary are ignored (an older binary may run after a newer
   migration job).
5. **Optional extension.** `0012_graph_vectors.sql` declares
   `-- requires-extension: vector`. Without pgvector it is recorded as
   `skipped`; a later `Apply` on a server that has gained the extension
   applies it. Files after 0012 must not depend on it.
6. **pgxpool.** `Apply` takes a `Beginner` (anything with
   `Begin(ctx) (pgx.Tx, error)`): `*pgx.Conn`, `*pgxpool.Pool` and
   `*pgxpool.Conn` all fit. `cmd/migrate` passes a `*pgxpool.Pool`; the
   tests use `*pgx.Conn`.

## Spec DDL issues (transcribed unchanged, noted here)

7. **`scope.kill_switches` primary key `(level, target_id)`** makes
   `target_id` NOT NULL, so a `global` switch cannot store NULL. Callers use
   the nil UUID `00000000-0000-0000-0000-000000000000` as the target of the
   global switch.
8. **`engine.run_events` is append-only** (trigger), yet 10.7 retention
   deletes whole runs after 400 days. The retention job must run as the
   table owner and use `ALTER TABLE engine.run_events DISABLE TRIGGER
   run_events_immutable` inside its transaction (or a future migration
   partitions the table and drops partitions). Not implemented here.
9. **`graph.embeddings.embedding vector(768)`** fixes one dimension; the
   spec comment says "one table or partition per dimension". Kept as is.
10. **`CREATE EXTENSION IF NOT EXISTS vector`** needs superuser or a
    trusted-extension setup on managed Postgres; 0001's `pg_trgm` and
    `citext` are trusted extensions on PostgreSQL 13+.

## Row-level security (0007, 10.7)

11. Policy: `USING (tenant_id = NULLIF(current_setting('app.tenant_id',
    true), '')::uuid)`. `missing_ok` plus `NULLIF` makes an unset or reset
    setting match nothing (fail closed) instead of raising an error.
    Services set the tenant per transaction with
    `set_config('app.tenant_id', $1, true)`.
12. **`FORCE` and role-scoped exemptions (0018).** 0007 used `ENABLE`, which
    exempts the table owner. 0018 adds `FORCE`, so only superusers skip the
    policies, and replaces "connect as the owner or a `BYPASSRLS` role" for
    cross-tenant paths with permissive policies scoped to one role
    (`CREATE POLICY svc_ff_engine ON engine.runs TO ff_engine USING (true)`).
    Chosen over `BYPASSRLS` because (a) a `CREATEROLE` migrator such as Cloud
    SQL's cannot grant `BYPASSRLS` without holding it (PostgreSQL 16), and
    (b) the policy exempts the role on its own schema's tables only. The
    engine scheduler (`ListReady`/`ClaimReady` span tenants and run as
    `ff_engine`) therefore needs no SECURITY DEFINER claim function and no
    second connection pool. The engine, scope, notifier and identity stores
    do not yet pass a tenant on every call (the event store is keyed by run
    id), so those roles get the exemption on their whole schema; graph sets
    `app.tenant_id` in every transaction and stays subject to RLS except on
    `graph.change_events`, which its relay scans across tenants (it reads
    entity snapshots per tenant with the tenant set). Maintenance jobs use
    `ff_maintenance`, exempt everywhere. Roles are created by
    `ff_sync_roles()` (NOLOGIN when absent), not by Terraform alone, so the
    policies can name them in every environment.
13. **Coverage.** Tenant tables (every table with `tenant_id` in a service
    schema, `identity.auth_audit` included: rows with a NULL tenant match
    nothing), every partition of `scope.scope_decisions`, and the child
    tables without `tenant_id` (`run_events`, `effects`, `timers`,
    `engine.snapshots`, `run_ext`, `run_requests`, `approval_ext`,
    `workflow_versions`, `workflow_version_ext`, `scope_versions`,
    `deliveries`), which follow their parent through `parent_visible`. Not
    covered: global tables (`identity.tenants`, `users`, `signing_keys`,
    `email_verifications`, `engine.tools`, `engine.workers`,
    `scope.kill_switches`) and the outbox and inbox tables, which belong to
    one service and are protected by the schema grants alone.

## Additions

14. **Outboxes** (`identity.outbox` for auth.events, `scope.outbox` for
    scope.events), same shape as `engine.outbox` (10.4 "same shape in every
    service schema", 23.7). The gateway hosts the identity module (3.1), so auth.events come from
    the identity schema. Graph uses
    `graph.change_events` as its outbox (10.5); notifier produces nothing.
15. **Inbox** `engine.processed_events`: the engine consumes scope.events
    (pause/resume) and graph.changes (event triggers). `notifier.processed_events`
    already exists. The similarity index consumes graph.changes with
    idempotent upserts ordered by `source_seq` (23.2) and needs no inbox.
16. **`engine.workers`** heartbeat table for membership (23.3): live for 15 s
    after `heartbeat_at`, which uses the database clock.
17. **`engine.runs.lease_acquired_at`, `engine.runs.woken_at`**: lost-wakeup
    protection for the lease protocol (see eventstore/OPEN_QUESTIONS.md).
    A wake that arrives while a worker holds the lease must survive the
    worker parking the run with a later `next_wake_at`.
18. **`runs_ready_prio`** partial index `(priority DESC, next_wake_at, id)`
    matching the ready scan order; `runs_ready` from 10.4 is kept.
19. **`identity.http_idempotency`** for the gateway's `Idempotency-Key`
    (12.1): key at most 128 characters, request hash, stored status and
    body, `completed_at` NULL while in flight, `expires_at` default 7 days
    (the 9.10 retention, applied to every POST). Keyed `(tenant_id, key)`;
    RLS enabled.
20. **Scope decision partitions.** `scope.scope_decisions_default` (so a
    missed maintenance run never loses a decision) and
    `scope.ensure_decision_partition(month timestamptz)`, which creates the
    monthly partition `scope.scope_decisions_yYYYYmMM` (UTC months), first
    moving that month's rows out of the default partition (ATTACH would
    fail otherwise). It is idempotent and serialized by an advisory lock.
    The migration creates the current and the next month; a scheduled job
    must call it monthly and drop partitions older than 13 months (10.7).
