# services/scope

ScopeService (spec 4, 11.3): programs, versioned scope documents, kill
switches and the decision log.

* `Server` — the gRPC service. `PutScope` creates version `base_version+1`
  or fails with ABORTED `VERSION_CONFLICT` (`Program.current_version` is
  that head, the latest published version).
* **effective_from** (spec 4.1) is enforced: `GetScope` version 0, `Check`
  with `scope_version` 0 and the client's `CurrentVersion` (the engine's
  pin at submission) use the highest version whose effective_from is at or
  before the injected clock's now. A future-dated version is stored and
  visible (`GetScope` by number, `ListScopeVersions`), but a pinned check
  of it is FAILED_PRECONDITION until it takes effect. Its `PutScope`
  writes `scope.version_scheduled`; `ActivationLoop` (every 10 s, looking
  back 24 h on start) writes `scope.version_published` with an event id
  derived from (program, version) once effective_from passes, so the
  engine re-validates running runs only then. A version can therefore
  reach running runs up to one tick late. `DueVersions` reads across
  tenants, so the scope role must bypass row-level security. `Check`/`CheckBatch` compile and
  cache each (program, version) policy (`scope.Compile` with the resolver
  port; `NetResolver` adapts `net.Resolver`), check kill switches first
  (global, tenant, program), resolve and pin addresses when `resolve` is set,
  record every decision in `scope_decisions` before returning it and fail
  closed: an unrecordable decision is UNAVAILABLE, never an allow. Every
  decision is logged as `scope.decision` and counted in
  `ff_scope_decisions_total{decision,reason}`.
* State changes write scope.events (`scope.program_created`,
  `scope.version_published`, `scope.version_scheduled`, `scope.kill_switch_set`) to the outbox in the
  same transaction; a `bus.Relay` publishes them.
* `MemRepo` / `PgRepo` (scope schema, migrations 0003, 0008, 0011; the
  default partition takes decisions of any month).
* `Client` — the client side: implements `engine.Scope` (current version,
  locally compiled policies, checks with `resolve`, kill switches cached
  for one second and invalidated on scope.events) and the per-target checks
  of the tool runner, egress proxy and similarity service.
* `DecisionLog` — reads recorded decisions by id (the egress proxy).

Uuid columns `created_by`/`set_by` keep the uuid of `user:<uuid>` actors
(nil for service actors). The global kill switch needs the owner role or a
service identity.

Tests: `go test ./services/scope/` (memory; Postgres too with
`FF_TEST_PG_DSN`): tenant isolation on every RPC, optimistic concurrency,
pinning, kill switch first, decision log, scope.events, fail closed.
