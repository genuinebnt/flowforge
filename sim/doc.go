// Package sim is Flowforge's deterministic simulation harness and
// invariant checker (spec 23.8 fault-injection harness and deterministic
// simulation testing, 25.7 simulation and invariants, 25.11 CI).
//
// Run executes a Scenario (seed, fault profile, workload) on a
// single-process cluster: several engine workers (each its own
// engine.Engine) sharing one eventstore.MemStore, the real DSL compiler,
// a fake scope service, a recording tool runner, an at-least-once Pub/Sub
// feeding the alerts notifier (with its inbox) and a graphstore.Mem
// ingester, and the shared GCRA limiter. A simulated clock and one seeded
// SplitMix64 drive all nondeterminism; a cooperative scheduler lets
// exactly one actor (a Worker.RunOnce or an API call) run at a time and
// switches only at the fault adapters, so a seed reproduces exactly.
// Every store, scope, ingest, publish and tool call goes through an
// adapter that can delay, drop, duplicate or fail it, crash the calling
// worker, or (store calls) find the worker partitioned; workers also run
// with skewed wall clocks.
//
// CheckInvariants checks inv_01..inv_11 of spec 25.7 on the recorded
// History. See README.md for the architecture, the fault model and how to
// reproduce a failing seed.
package sim
