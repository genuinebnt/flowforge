# services/maintenance

The spec 10.7 retention rules, the monthly partition job and run archival
(G-OPS-03, G-ENG-04). `Runner.RunOnce` runs every job once, bounded
(`Limits`: 5,000 rows per table, 20 runs, 3 partitions) and idempotent, so
an interrupted pass is finished by the next. Passes are serialized across
processes by `pg_try_advisory_lock` (a second caller gets `ErrBusy`).

| Job | Rule |
|---|---|
| `decision_partitions` | `scope.ensure_decision_partition` for this month and the next two |
| `decision_retention` | drop `scope_decisions_yYYYYmMM` partitions whose month ended 13+ months ago; trim the default partition |
| `run_archive` | runs finished 400+ days ago (outbox fully published, no child runs): events to `archives/run_events/<ten>/<run>.jsonl.zst`, a row in `engine.run_archives`, then the whole run deleted in one transaction |
| `snapshots` | snapshots of finished runs (the engine deletes them at finish; this sweeps leftovers) |
| `observations` | `graph.observations` ingested 90+ days ago (raw blobs: bucket lifecycle rule) |
| `auth_audit` | `identity.auth_audit` older than 12 months |
| `outbox` | engine, scope and identity outbox rows published 7+ days ago |
| `processed_events` | engine and notifier inbox rows older than 30 days |
| `bus` | acked (24 h) and dead (14 days) rows of the Postgres bus |
| `alert_state` | ended notifier throttle windows |

## run_events and the append-only trigger

`engine.run_events` stays append-only (trigger of 0004). 0017 replaces the
trigger function so that a DELETE passes only when the transaction set
`ff.archive_run` (transaction-local) to that run's id and an
`engine.run_archives` row for the run exists. The archive is written to
the blob store before that transaction, so a crash leaves the run in place
and the next pass rewrites the same object. The trigger is never disabled
(that needs table ownership and would expose every session). Archival is
skipped when the blob store is missing or in memory.

The archive is a zstd frame of JSON Lines, one `ArchiveLine` (run_id, seq,
type, at, actor, payload) per event in seq order, with a content checksum.
It uses stored (uncompressed) zstd blocks, since the module has no zstd
encoder (`blob.ZstdStored`); every zstd decoder reads it.

## Where it runs

* in the engine process with Postgres, every `FF_MAINTENANCE_INTERVAL`
  (default 1 h; the first pass waits min(interval, 5 min); `0` disables);
* `migrate maintain` (cron, a Cloud Run job), archiving to
  `FF_ARTIFACT_BUCKET` or `FF_BLOB_DIR`;
* on GCP, every `POST /internal/schedules:tick` (the one Cloud Scheduler
  job), with `FF_MAINTENANCE_INTERVAL=0` on the engine.

## Privileges

DELETE on engine (runs and child tables, snapshots, outbox,
processed_events, run_archives INSERT), scope (scope_decisions_default,
outbox), graph.observations, identity (auth_audit, outbox), notifier
(processed_events, throttle_windows) and bus.*; EXECUTE on
`scope.ensure_decision_partition`; ownership of the scope_decisions
partitions for DROP TABLE. Under the 0018 roles: the migrator acting as
`ff_maintenance`, which has the cross-tenant policy on every RLS table.

## Tests

```sh
eval "$(scripts/devpg.sh start)"
go test ./services/maintenance/   # TestRegress_GOPS03_* (skipped without FF_TEST_PG_DSN)
```
