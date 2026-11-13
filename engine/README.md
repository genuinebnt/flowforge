# engine

The Flowforge workflow engine (spec 9). Layer 4: imports `core`,
`core/netnorm`, `scope`, `limiter`, `bytecode`, `vm` and `eventstore`.
The Postgres adapter is `engine/pgrepo`; fakes and the test harness are in
`engine/enginetest`.

## Architecture

```
            API (Engine)                          Worker (RunOnce / Run)
 CheckWorkflow / RegisterWorkflow ─ Compiler ─┐   ClaimReady ─ lease + fence
 SubmitRun ── Store.CreateRun(run_submitted)  │   read log ─ fold (runLog)
 GetRun / ListRuns / ListRunEvents / WatchRun │   restore snapshot or fresh VM
 CancelRun, ResolveUnknownEffect ─ RunRequest │   replay: recorded results by path
 DecideApproval ─ approval row ─ Store.Wake   │   live frontier: perform effect,
 HandleScopeChanged / HandleKillSwitch        │     append with the fence
                                              │   ReleaseLease(Next{status, wake})
```

* **The log is the truth.** A run's state is a fold of its events
  (`runlog.go`); the live path folds each event right after appending it,
  so replay and live execution share one state. `engine.runs` (Repo) is a
  projection the worker repairs on every advance; `GetRun` always derives
  status, waiting_on, output and failure from the log.
* **Single writer.** Only the lease holder appends (`writer.go`), with
  `Store.Append(expectedSeq, fence)`; `ErrFenced` stops the worker at once
  (`ErrLeaseLost`). A watchdog renews the lease every `RenewEvery` and
  cancels in-flight executions when the run is cancelled, times out or a
  kill switch engages. API calls never append: they queue a `RunRequest`
  (cancel, resolve_unknown, scope_changed) or update an approval row, then
  `Store.Wake` the run and signal local workers.
* **Replay** (`tasks.go`): at each suspension the effect is looked up by
  path; a kind or args_hash mismatch fails the run with
  `nondeterminism_detected`; a terminal result resumes the VM; otherwise
  the boundary checks run (cancel, deadline, scope change, pause, kill
  switch) and the effect is performed or continued.
* **Concurrency without nondeterminism.** `runTasks` drives the body or
  the children of a `par_map` in rounds: each active task (at most
  `limit`) runs to its next tool execution; the round's executions run
  concurrently and their results are appended in task order in one atomic
  batch. Logs therefore do not depend on timing, and a crash never splits
  a round, which is what makes eng_008's "final log equals the
  uninterrupted log" hold.
* **Deterministic ids.** Approval, timer, decision, observation, snapshot
  and outbox ids are derived from (run, path, ...), so re-performing a step
  after a crash yields the same ids and upserts stay idempotent.

## Effects (spec 9.5)

| Kind | Events |
|------|--------|
| scope_check | effect_scheduled, scope_decision + effect_completed (Ok(proof) / Err(ScopeDenied)) |
| tool_call | effect_scheduled, scope_decision (re-check per proof target), effect_started, observation_ingested*, effect_completed / effect_failed (+ effect_retry_scheduled) |
| approval | effect_scheduled, approval_requested (row first), then approval_decided or approval_expired (+ escalation) with effect_completed |
| sleep | effect_scheduled, timer_started, timer_fired + effect_completed |
| now, log, emit_alert | effect_scheduled + (log / alert_emitted) + effect_completed in one batch |
| par_map | effect_scheduled, the children's effects at `P/task/i`, effect_completed / effect_failed |
| embed, similarity_query | through the optional ports; without them a permanent failure |
| agent_call, execute_plan | rejected at registration (D1); fail defensively |

Tool calls verify inv_06 before running: every ScopeProof argument must
name an allow decision recorded in this run for exactly that normalized
target, the run must hold an allow decision for it under its current scope
version, and an active or intrusive tool without a proof is refused. The
call then acquires the spec 14.3 limiter keys (a denial waits
`retry_after` without counting an attempt) and runs with
`idempotency_key = hex(sha256(run_id/path))`. A `Limiter` that also
implements `WorkLimiter` is told whether the call is passive (effect class
passive or read) or active (everything else), so during a limiter-store
outage passive work can continue on local budgets while active work waits
(spec 23.8; `services/engine.FailoverLimiter`, G-LIM-02).

Retries: `delay(n) = min(max_ms, initial_ms * multiplier^(n-1))`, wait
uniform in `[0, delay]` from the injected Rng, only for retryable errors
(and, if the policy lists `retry_on`, listed codes), while attempts remain
and the retry time is before the run deadline. `#[retry]` on the
statement (bytecode ANNOTS) overrides the tool's policy; `#[timeout]` on
the statement bounds the effect (code `timeout`).

Graceful shutdown: cancelling `Worker.Run`'s context drains instead of
crashing (`Config.DrainTimeout`, default 20 s): no new claims, in-flight
effects finish and are recorded, runs are released at their next effect
boundary with `next_wake_at = now` (`OPEN_QUESTIONS.md` 21, G-ENG-05).

Crash recovery (9.9): `effect_started` without a terminal event is
ambiguous. Idempotent tools (spec flag, `#[idempotent]` site or workflow)
and workflows with `#[on_unknown(retry)]` are re-executed with the same
attempt and key and no new event; others get `effect_outcome_unknown`
and wait for `ResolveUnknownEffect` (retry, assume_completed with a
result, assume_failed). `on_unknown(retry)` is read from the module's
ANNOTS section (`bytecode.Annotations.OnUnknownRetry`) or the compiler's
`CompileResult.Attributes`.

Compensation (9.6): on failure or cancellation the closures of
`vm.Compensations()` run newest first as child VMs with paths
`N/k/i` (N recorded in compensation_started), with ordinary recorded,
retried effects; compensation_step per closure, compensation_finished
with completed or failed. Compensations run to the end: no cancellation,
deadline or watchdog interrupt applies to them; during a kill switch their
target-facing tool calls fail.

## Run state machine (spec 9.2)

`status.go` holds the table (`CanTransition`) and the event mapping
(`nextStatus`); the writer refuses an append whose transition is illegal
and the fold rejects a log that contains one (`TestEngStatusTable`,
`TestEngStatusEvents`). The eventstore row keeps the scheduler status:
cancelling and compensating are stored as running so a crashed worker's
run is picked up again; paused runs are not schedulable until a request
reschedules them.

## Ports and wiring

```go
e, _ := engine.New(engine.Deps{
    Store: pgstoreStore, Repo: pgrepo.New(pool), Tools: pgrepo.NewTools(pool),
    Publisher: pgrepo.NewOutbox(pool), Compiler: dslCompiler, Scope: scopeClient,
    Executor: toolRunnerClient, Ingester: graphClient, Limiter: gcra,
    Clock: &core.SystemClock{}, Rng: core.SystemRng{},
}, engine.Config{})
go e.NewWorker("worker-1").Run(ctx)
```

**Compiler port.** The DSL compiler implements `Check(ctx, CompileRequest)`
and `Compile(ctx, CompileRequest)`. The request carries the source, the
workflow name (file name `<name>.ff`), the program policy (max_class,
prohibited; nil without a program) and the registered tools (latest
enabled versions, with `SpecSHA256`). The compiler returns diagnostics in
the JSON shape of spec 7.7, the inferred effect names and class, and for
`Compile` the encoded module (TOOLS entries pinned to the given spec
hashes, an entry signature, workflow and per-site annotations in ANNOTS).
Source errors are diagnostics; the Go error is only for compiler
failures. The policy check (E0402/E0403) is the compiler's job; the engine
re-verifies every module, rejects agent effects (`E0490`), and checks the
class against the program again at submit.

## Spec coverage

9.1-9.10, 8.6 (replay, par_map with hierarchical paths and on_error
fail/collect), 8.7 (fuel limit, snapshots every 500 effects or 5 minutes,
cache semantics), 3.3, 2.4 (scope, determinism, append-only, tenant
isolation, fail closed), 10.4 (via pgrepo), 11.4 (1:1 methods), 11.8
reasons, 12.6 waiting_on, 23.2, 23.8 scenarios (worker killed, paused
worker fenced, crash between execution and recording, scope store
unreachable). Tests: `TestEng001`..`TestEng008`, `TestEngProp_CrashAnywhere`,
fencing, par_map ordering, nondeterminism, agent rejection, tenant
isolation, scope change, events API, snapshots; Postgres variants of
eng_001, eng_004 and eng_008 in `pgrepo`.

Not covered: agent steps and child runs (D1); per-tenant weighted fair
ranking across workers (only the per-worker tenant cap); worker heartbeat
membership and rendezvous claiming (23.3); `#[timeout]` on a block of
several statements (each effect of the statement is bounded instead);
budgets other than fuel; deleting snapshots of finished runs (the store
has no delete).

## Tests and benchmarks

```sh
go test -race ./engine/...
go test ./engine/ -run Eng001 -update          # rewrite conformance/engine/*.golden.json
FF_PROPTEST_CASES=100000 go test ./engine/ -run Prop
scripts/devpg.sh start && FF_TEST_PG_DSN=... go test ./engine/pgrepo/
go test ./engine/ -run '^$' -bench Eng01 -benchtime=200x
```

b_eng_01 (4 vCPU Xeon, MemStore + MemRepo, FakeToolExec at 0 ms, one
worker): 5 steps ~1,800 runs/s (~110 µs/step), 20 steps ~650-830 runs/s
(~60-75 µs/step), 100 steps ~140 runs/s (~70 µs/step). A tool step is
three appends (scheduled, started, completed), a goroutine per execution
and the JSON fold of each event.

See `OPEN_QUESTIONS.md` for the choices made where the spec is silent.
