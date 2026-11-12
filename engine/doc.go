// Package engine is the Flowforge workflow engine (spec 9): it registers
// workflows (check, compile, verify, publish immutable versions), accepts
// runs with submit idempotency, and advances them with workers that hold
// a fenced lease on each run, replay its event log through the VM and
// perform effects at the live frontier with at-least-once execution and
// exactly-once recording.
//
// The Engine type implements the EngineService API (spec 11.4) over ports
// (ports.go): Compiler, Repo, Scope, ToolRegistry, ToolExecutor, Ingester,
// Limiter, Publisher, the optional Embedder and SimilarityIndex, and the
// event log in eventstore.Store. In-memory fakes for every port are in
// engine/enginetest; the Postgres adapter is engine/pgrepo.
//
// The event log is the source of truth (spec 2.4, 9.2): every run state
// is a fold of the log (runLog), the run row is a projection that the
// worker repairs, and only the lease holder appends. API calls that must
// change a run (cancel, unknown-outcome resolution, scope change) queue a
// request the worker applies at the next effect boundary; approval
// decisions update the approval row and wake the run.
//
// Spec sections: 2.4 (invariants), 3.3 (life of a run), 8.6 (effect
// protocol, replay, par_map), 8.7 (fuel, snapshots), 9.1-9.10 (engine),
// 10.4 (engine tables), 11.4 and 11.8 (API and error reasons), 12.6-12.7
// (REST shapes), 14.3 (rate-limit keys), 23.2, 23.3 and 23.8 (guarantees,
// work distribution, failure scenarios), 25.3 (eng_001..eng_008), 25.4
// (crash property), 25.7 (invariants inv_01..inv_11 checked on the logs)
// and 25.8 (b_eng_01).
package engine
