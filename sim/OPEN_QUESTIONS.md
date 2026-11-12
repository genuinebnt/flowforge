# sim: open questions and decisions

1. **Goroutines in the engine.** The engine runs the tool calls of a
   par_map round on goroutines and has a wall-clock watchdog. Rather than
   change the engine, the simulator keeps them deterministic: round
   executions only touch the tool runner, whose faults are seeded per
   (key, attempt, delivery); the watchdog's `PollInterval` is beyond any
   test and each worker is its own `Engine`, so it never acts. As a
   result mid-advance lease renewal, the watchdog's interrupt of in-flight
   calls and `CancelGrace` are not exercised. A clock-gated watchdog
   (taking a `core.Clock` timer port) would let the simulator cover them.
2. **Rendezvous claiming (23.3).** The engine's workers claim with
   `ClaimReady` and have no membership or rank-delayed claiming, so the
   simulator does not use `placement`. Ownership safety does not depend on
   it (leases and fences do); when the engine adds it, worker ticks should
   be delayed by rank.
3. **inv_10 and dispatch delay.** The engine admits a call at the limiter
   before it appends `effect_started`, so a slow append moves the request
   later and the executor can see admissions bunched. The checker
   therefore bounds arrivals in a window `w` by
   `burst + rate*(w + d)` with `d` the longest admission-to-arrival delay
   of the window's requests (exact for GCRA admissions), and requires a
   matching admission for every request. A strict executor-side bound
   would need the limiter at the egress (the tool runner), as 23.3's
   target affinity suggests.
4. **Transport duplicates and inv_10.** A request the network delivers
   twice counts once (it is one request with one key and attempt); the
   tool still sees both (inv_04 allows it).
5. **Kill switch races.** A kill switch engaged between the re-check and
   the delivery of a call is not reported: no allow decision is violated,
   and the egress proxy is the enforcement point in production.
6. **"Prior" decisions (inv_06)** are judged by store commit time against
   the call's arrival, because a call re-executed after a crash keeps its
   original `effect_started` and may follow a revalidation decision
   recorded later.
7. **Compensation order (inv_07)** is checked generically from the log
   (steps 0..n-1, outcome) and, for the workload's forward/undo pairs
   (`reserve`/`release`), at the executor: first deliveries of undo calls
   must be the forward results in reverse. A saga marked for an operator
   (`compensation_finished{failed}`) is not held to the order; a step
   failed with `cancelled` is a violation, since compensations run to the
   end.
8. **Alert rules.** The notifier runs a per-event rule (group_by run id
   and path, no throttle) and the default `ops_run_failed` (throttle 15 m).
   inv_08's "dedupe window" is the throttle measured in notifier
   processing time.
9. **Graph clock.** `graphstore.Mem` stamps covering runs with its clock;
   both the live and the reference graph use a frozen clock so inv_09
   compares merge results only.
10. **Repo faults.** The relational Repo shares the store's database in
    production; it is not faulted on its own, but a crashed worker cannot
    write to it. Faulting it separately would expose SubmitRun's two-step
    create (store row, then projection), which the worker repairs from
    `run_submitted`.
11. **Liveness bound.** 2 simulated hours after faults stop (approvals
    expire after 1 h, the approver decides within 5 min, leases expire
    after 30 s). No workload leaves a run open on purpose
    (`RunRecord.OpenEnded` exists for one that would).
12. **Seeds after changes.** A regress seed replays exactly only with the
    same code; every fixed bug therefore also has a targeted test in its
    package (`engine.TestRegress_*`).
