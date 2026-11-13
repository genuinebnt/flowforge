# services/engine

EngineService (spec 9, 11.4) over `engine.Engine`.

* `Server` — one handler per RPC; the caller is the token's identity
  (`platform.Identity.EngineCaller`). `SubmitRun` requires the
  `idempotency-key` metadata. `WatchRun` streams the run's events after
  `after_seq` and follows new ones (resume with the last seq seen).
* `Service` (`New`, `Run`) — wires the engine: `Compiler` adapts
  `compiler.EngineAdapter` to the engine's Compiler port; the event store and
  repo are `pgstore` + `pgrepo` (or `eventstore.MemStore` + `MemRepo`, a copy
  of enginetest's, in memory); `scope.Client` is the Scope port; `Executor`
  calls `ToolRunnerService.Execute` (stream: observations collected,
  `ToolError` -> `*engine.ToolError`, cancel -> cancel); `Ingester` calls
  `GraphService.IngestObservations`; the limiter is Redis
  (`redislimiter` behind `ContextLimiter`) or an in-process GCRA with
  `DefaultLimitRules`; the publisher is `pgrepo.Outbox` (or an in-memory
  outbox) wrapped by `MeteredPublisher` (ff_runs_total, ff_effects_total,
  ff_approvals_pending, ff_replay_nondeterminism_total).
* The tool registry is fed from `ToolRunnerService.ListTools` (`SyncTools`,
  every minute); specs get `convert.DefaultToolRetry` and their canonical
  hash, which must match the runner's `spec_sha256`.
* `Run` starts N workers (woken by `LISTEN engine_wake` in Postgres, by
  local signals otherwise), the outbox relay, and the scope.events consumer
  (`HandleScopeEvent`: new version -> `HandleScopeChanged`, engaged kill
  switch -> `HandleKillSwitch`; duplicates skipped through the
  processed_events inbox).
* `POST /internal/schedules/{id}:fire` (Cloud Scheduler) — internal token in
  `Authorization`; creates a run as `schedule:<id>` with idempotency key
  `schedule:<id>:<fire time>` (body `fire_time`, else the
  `X-CloudScheduler-ScheduleTime` header, else the current minute).
* Workflow schedules (G-OPS-10, `schedules.go`, `cron.go`): a JSON API
  under `/internal/schedules` (GET list, POST create, GET/DELETE `{id}`)
  that the gateway's `/v1/schedules` forwards to, a `ScheduleStore`
  (`MemSchedules`, `PgSchedules` over engine.schedules), and a `Scheduler`
  that fires due schedules every 15 s in process and on
  `POST /internal/schedules:tick` (internal token, or Cloud Scheduler's
  Google ID token with `TickAuth.Audience`). Each firing uses idempotency
  key `schedule:<sch_id>:<fire time>`; missed firings coalesce. The tick
  also runs a maintenance pass (services/maintenance). Cron: five fields,
  UTC, `*`, values, ranges, lists, steps; day-of-month OR day-of-week when
  both are restricted (Vixie cron); no names or `@` shorthands.

Tests: `go test ./services/engine/` — diagnostics as error details,
idempotent submit, WatchRun resume, tenant isolation, schedule idempotency
(memory and Postgres), `TestCron`, `TestRegress_GOPS10_Schedules_{Memory,Postgres}`.
