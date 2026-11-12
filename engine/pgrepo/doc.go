// Package pgrepo implements the engine's relational ports over PostgreSQL
// (spec 10.4): engine.Repo (workflows and immutable versions, the runs
// projection, approvals, timers, the effects projection and queued run
// requests), engine.ToolRegistry over engine.tools and engine.Publisher
// over engine.outbox. The event log itself lives in eventstore/pgstore on
// the same database; Repo and pgstore share engine.runs.
//
// Tables the spec does not define (workflow_version_ext, run_ext,
// run_requests, approval_ext) are created by migration
// 0013_engine_extras; EnsureSchema applies the same idempotent SchemaSQL for
// databases that predate it.
package pgrepo
