# sim

Deterministic simulation of a Flowforge cluster with fault injection and
the invariant checker of spec 25.7 (`ff-sim`). Layer 5: imports the pure
packages (`core`, `scope`, `limiter`, `oam`, `alerts`, `vm`, `bytecode`,
`eventstore`, `engine`, `compiler`, `graphstore`), no adapters.

Spec: 23.1 failure model, 23.2 guarantees, 23.3 work distribution, 23.4
EventStore properties, 23.5 convergent merge, 23.7 outbox/inbox, 23.8
failure scenarios, fault-injection harness and deterministic simulation,
25.7 invariants and fault profiles, 25.10 `xtask sim`, 25.11 CI.

## Usage

```go
r := sim.Run(sim.Scenario{Seed: 42, Profile: sim.ProfileChaos, Workload: sim.WorkloadMixed})
r.OK(), r.Violations, r.Hash, r.History, r.Summary(), r.Repro()
vs := sim.CheckInvariants(history) // pure function of a History
```

```sh
go test ./sim/...                                  # 20 seeds x 7 profiles, determinism, invariant unit tests, regress seeds
FF_SIM_SEEDS=200 go test ./sim -run TestSimProfiles   # the CI depth; nightly uses 10000
go test ./sim -run TestSimProfiles -sim.seed=21 -sim.profile=chaos -sim.workload=mixed -v
go run ./cmd/xtask sim --profile chaos --seeds 200
go run ./cmd/xtask sim --profile all --workload all --seeds 200
go run ./cmd/xtask sim --profile chaos --workload mixed --seed 21
go test ./sim -run '^$' -bench Sim_Chaos -benchtime 50x
```

A failing scenario prints its seed, profile, workload, the violations, the
two reproduce commands above and the line to add to
`conformance/sim/regress_seeds.json` (`[{seed, profile, workload, note}]`),
which `TestSimRegressSeeds` replays. A seed reproduces exactly as long as
the simulator and the code under test are unchanged; after changes a
regress seed is still a valid scenario but may take a different path, so
every fixed bug also has a targeted regression test in its package.

## Architecture

```
 workload (clients, approver, operator, scope/kill-switch admin)
        | API actors (SubmitRun, CancelRun, DecideApproval, ...)
        v                           API engine
 +---------------------------------------------------------------+
 | scheduler: simulated clock + event queue + seeded SplitMix64   |
 |   actors run one at a time; they park at every fault adapter   |
 +---------------------------------------------------------------+
   w1 (Engine, skewed clock)  w2 ...  wN    (crash -> restart as new id)
        |  faultStore -------> eventstore.MemStore (store clock = true clock)
        |  faultScope -------> fake scope service (versioned docs, kill switches)
        |  toolExec ---------> network edge (kill switch, per-target GCRA) ->
        |                      recording tool runner (faults, deterministic results)
        |  faultIngester ----> Pub/Sub (dup, delay, reorder) -> graphstore.Mem
        |  faultPublisher ---> Pub/Sub (run.events)          -> alerts.Processor (inbox)
        |  simLimiter -------> limiter.GCRA (rl:h 2/s burst 4, store clock)
        |  faultRepo --------> in-memory engine.Repo (no writes after a crash)
```

* **Step-driven scheduling.** Each engine call that does I/O happens on an
  *actor* goroutine (one `Worker.RunOnce`, or one API call), but only one
  actor runs at a time: at every adapter call it parks with the simulated
  time it is ready at (latency drawn from the rng, plus the time its tool
  executions took) and the scheduler resumes the earliest ready actor or
  timed event (worker ticks, crashes, partitions, Pub/Sub deliveries,
  approver and operator ticks). Faults are decided when the call is
  resumed, so they see the current time.
* **Engine goroutines.** `execBatch` runs the tool calls of a par_map round
  concurrently in production; `toolExec` implements
  `engine.SerialExecutor`, so here they reach it one after the other in
  task order (the edge's limiter is shared state, and the history must be
  a function of the seed). It draws their faults from a generator seeded
  by (seed, key, attempt, delivery) and accumulates the round's latency as
  a max, so the round still costs the time of its slowest call. The per-run
  watchdog uses a wall-clock ticker: `PollInterval` is set beyond any test
  so it never fires, and each worker is a separate `Engine`, so the API
  engine's in-process notifications never reach it; workers see
  cancellations, decisions and scope changes through the store at their
  next advance. Consequently leases are never renewed mid-advance (a slow
  advance can lose its lease, which exercises fencing). `CancelGrace`'s
  wall-clock timer is only reached when a call is interrupted mid-flight,
  which cannot happen here (tool calls return immediately in real time).
* **Crashes** mark the worker's actor crashed: its pending call fails, it
  runs to its end on its own goroutine while the scheduler waits (so a dead
  process never runs concurrently with a live one), and every later store,
  repo, scope or tool call fails without effect. Its leases stay until they
  expire. A new worker with a new id starts after a delay.
* **Clocks.** The store, limiter, Pub/Sub and notifier use the true
  simulated clock (the database clock of 23.4, Redis TIME); each worker's
  engine sees wall time plus a fixed skew. The graph stores use a frozen
  clock so their exports do not depend on delivery times.
* **Workloads** (`WorkloadNames`): workflows compiled from DSL source by
  the real compiler (`workflows.go`): `scan` (scope checks, active probe,
  passive resolve with observations, alerts), `fanout` (par_map limit 3),
  `saga` (two compensated writes, an approval decided by the simulated
  approver after 10 s - 5 min, 25 % denials, a financial write that is
  declined for some amounts), `retry_timer` (sleeps and a tool that fails
  transiently with retries), `longrun` (timed probes; cancelled by the
  client, killed by a program kill switch, or paused/continued by a scope
  version change). Submissions use idempotency keys; 30 % are sent twice
  and failures are retried with the same key. The operator resolves
  unknown effect outcomes with retry. `mixed` combines everything.

## Fault model (`faults.go`)

| Profile | Faults (while `Scenario.Duration` runs; afterwards none) |
|---------|-------|
| none | no latency, immediate delivery |
| crashy | 1 % of worker store calls crash the process (before or after the call took effect), plus crashes at random instants (mean 3 min) |
| lossy | store/scope/ingest/publish calls: 2 % lost requests, 2 % lost replies (the call took effect), 3 % duplicated, 2 % transient errors; tools: 5 % transient errors, 3 % lost requests, 3 % lost replies, 5 % duplicate deliveries; Pub/Sub: 30 % of messages delivered 2-5 times, delays up to 30 s |
| slow | 2 % latency spikes of 2-45 s on calls and tools; Pub/Sub delays up to 20 s |
| partitioned | a worker cut from the store for 10-90 s, mean every 4 min |
| skewed | per-worker wall-clock offsets up to +-90 s |
| chaos | all of the above at lower rates |

## Invariants (`invariants.go`, `replay.go`)

| Id | Check on the history |
|----|----------------------|
| inv_01 | each log is seq 1..n; the store's commits tile it with no gap or overlap |
| inv_02 | per run, lease fences strictly increase in store order and each is granted once; every fenced commit is by the fence's owner and one actor |
| inv_03 | no commit with a fence lower than one already granted (store linearization order) |
| inv_04 | at most one terminal result per effect; every executor call of (run, path) carries `sha256(run/path)` and no key serves two effects; a recorded tool result came from a delivered call; one run per submission key |
| inv_05 | a fresh VM driven by the log's recorded results (par_map children and compensations included) asks for the same effects (kind, args hash) in the log's order per VM, and reaches the logged output or failure code; engine-imposed ends (cancel, timeout, nondeterminism, budget) may stop at the frontier |
| inv_06 | every delivered active call names a target with an allow decision for it and the call's scope version committed before the call, cites only recorded allow decisions, and the scope document of that version (independent oracle) allows it; no target-facing call (passive and above) is delivered while a kill switch covering its program is engaged |
| inv_07 | a failed/cancelled run with registered compensations recorded compensation_started, steps 0..n-1, and finished completed (all ok) or failed (operator); no step is "cancelled"; when completed, the executor saw the undo calls in reverse order of the forward results |
| inv_08 | per rule, every matching run event yields exactly one alert record (delivered or throttled), none for other events, no two deliveries of a dedupe key within the throttle, despite redeliveries |
| inv_09 | the graph equals the accepted observations applied once each in id order to an empty `graphstore.Mem`; one content per observation id; every logged observation was accepted |
| inv_10 | measured at the network edge (G-ENG-09): per `rl:h` key every request was admitted by the engine's limiter (matched per actor in order; a transport duplicate is the same request there), every delivery passed the edge's limiter at its arrival (duplicates included), and deliveries in any window stay within `burst + rate*window` |
| inv_11 | every run is terminal, at most `LivenessBound` (2 h) after faults stop |

**The network edge** (G-ENG-09). `toolExec` first plays the egress proxy:
a target-facing call for a program under an engaged kill switch is
refused (`kill_switch_active`, recorded in `KillSwitchRaces`: the switch
was engaged after the engine's re-check), and a call with an `rl:h` key is
charged against the edge's own GCRA at the store clock (same rate as the
engine's limiter, separate state, as the proxy's `rl:edge:h` keys are); a
denial answers `rate_limited` with the limiter's delay, which the engine
waits out without counting an attempt. The `edge` workload contends one
registrable domain with many fan-outs and pulses a kill switch, so both
paths run under every fault profile (`TestRegress_G_ENG_09_EdgeWorkload`).

Each invariant has unit tests (`TestInv01_*` .. `TestInv11_*`) that break a
clean history and assert the checker reports it.

## Results

`go run ./cmd/xtask sim --profile <p> --seeds 200`, every profile: 0
violations (after the fixes below); `--profile all --workload all --seeds
300` (16,800 scenarios): 0. `BenchmarkSim_Chaos`: ~12 scenarios/s on one
core (84 ms, ~265k allocations per scenario of 20 simulated minutes, 3
workers, ~20 runs, ~10k scheduler steps); `xtask sim` runs scenarios in
parallel, 200 chaos seeds take ~4 s on 4 cores.

## Bugs found and fixed

1. **Outbox loss on the terminal advance** (engine/advance.go `finish`;
   lossy seed 1, inv_08). When publishing the batch holding the terminal
   event failed, the worker still released the run as terminal; terminal
   runs are never scheduled again, so those events never reached
   run.events and their alerts were lost (spec 23.7: duplicates, never
   losses). The run now stays schedulable (`running`, woken after
   `IdleBackoff`) until its outbox watermark reaches the head.
   `engine.TestRegress_OutboxTerminalLoss`.
2. **A scope-service outage paused runs forever** (engine/effects_scope.go
   `revalidate`, engine/tasks.go; chaos seed 3, inv_11). Re-validation
   after a scope change treated "policy cannot be loaded" and
   `scope_unavailable` re-decisions as a failed validation, recorded
   `scope_changed{pause}` and `run_paused`, and nothing resumed the run
   unless yet another version arrived. Now an outage records nothing and
   the run waits at the boundary (fail closed) and re-validates later; a
   version that does not exist still pauses. `engine.TestRegress_ScopeOutagePause`.
3. **In-flight compensation cancelled on resume** (engine/compensate.go
   `closeOpenBody`; chaos seed 21, inv_07). When a worker died during a
   compensation of a cancelled run, the next owner re-entered the cancel
   flow, which failed every open effect with `cancelled`, including the
   compensation's own in-flight effect: the saga was marked for an
   operator although compensations run to the end, and the dead worker's
   late undo executed after the next undo (reverse order broken at the
   tool). The cancel and timeout flows now leave compensation effects
   alone, so the next owner re-executes the idempotent undo with its key.
   `engine.TestRegress_CompCancelledOnResume`.

Checker bugs found while building it (pinned by regress seeds): a call
re-executed after a crash reuses its original `effect_started`, so "prior
allow decision" is judged by commit time, not seq (inv_06); a par_map the
engine closed on cancel is not compared with its children's join (inv_05).

## Files

`sim.go` (Scenario, Report, Run), `sched.go` (clock, event queue,
actors), `faults.go` (profiles), `adapters.go` (fault adapters, history
recording), `cluster.go` (wiring, workers, crashes, partitions, Pub/Sub,
notifier, graph), `fakes.go` (scope service, registry, tool runner),
`workflows.go` (DSL sources, tool specs), `workload.go` (clients,
approver, operator), `compiler.go` (compiler port), `memrepo.go` (copy of
`enginetest.MemRepo`: test-support packages may not be imported by
non-test code), `history.go`, `invariants.go`, `replay.go`, `regress.go`.

## Limitations

See `OPEN_QUESTIONS.md`: no mid-advance lease renewal, no rendezvous
claiming (the engine has none), the Repo shares the store's fate rather
than being faulted, one tenant.
