# engine: open questions and decisions

Each choice is pinned by a test where it changes behaviour.

1. **Who appends.** Only the lease holder appends to a run's log. CancelRun,
   ResolveUnknownEffect and HandleScopeChanged queue a `RunRequest` (pgrepo
   table `engine.run_requests`, a proposed migration); DecideApproval
   updates the approval row (first decision wins by a conditional update).
   Each then calls `Store.Wake` and signals local workers. GetRun shows
   `cancelling` as soon as a cancel request is queued.
2. **Effect results** are recorded twice in `effect_completed`: `result`
   (JSON for humans, proofs and Hosts shown as plain targets) and `value`
   (base64 canonical CBOR, `vm.EncodeValue`), which replay feeds to the VM.
   `effect_failed` carries `final` and, when final, `fail_code` (spec 8.7);
   a retryable failure is followed by `effect_retry_scheduled` in the same
   batch, so the fold never has to guess.
3. **Instant effects** (now, log, emit_alert, scope_check) record
   `effect_scheduled` and their result in one atomic append (scope_check
   after the scope call). Only tool calls have `effect_started` and
   attempts.
4. **Tool-call re-check.** Besides verifying the proof's recorded decision
   (inv_06), the engine re-decides each proof target with the tool's own
   class right before the call and records it (`purpose: recheck`), as
   spec 3.3 describes. scope.check itself decides with the workflow's
   inferred class (at least passive), so one proof covers every call.
5. **scope.allows after a scope change** uses the version pinned at
   submission for the whole run; otherwise replay could see different
   answers (vm OPEN_QUESTIONS 10). The authoritative gate (scope.check, the
   re-check, inv_06) uses the current version.
6. **Scope change re-validation**: at the next effect boundary every target
   with an allow decision under the old version is re-decided under the new
   one and the inferred class is compared with the new max_class. All pass:
   `scope_changed{action: continue}` and the run is pinned to the new
   version. Otherwise `scope_changed{action: pause}` + `run_paused`; the
   run stays paused until a later version passes (`run_resumed`) or it is
   cancelled. A run past its deadline that is woken while paused is resumed
   and failed with `timeout`.
7. **Kill switches** apply at every effect boundary of the body, for every
   run in reach (fail closed), and to submission of target-facing
   workflows (`KILL_SWITCH_ACTIVE`). The run is cancelled with reason
   `kill_switch:<level>`. Compensations still run, but their target-facing
   tool calls fail with `kill_switch_active`.
8. **Compensations** run to the end: cancellation requests, the deadline
   and watchdog interrupts do not apply once compensation started, and
   CancelRun on a compensating run returns RUN_NOT_CANCELLABLE. Their paths
   are `N/k/i` with N = the root VM's next sequence number at failure
   (recorded as `path_prefix`).
9. **Nondeterminism** fails the run without running compensations (their
   replay cannot be trusted); if compensations were registered the run is
   marked `compensation_outcome = failed` (inv_07 "needs operator action").
10. **Approvals.** Escalation roles come from `Config.EscalationRoles`
    (the approval effect has no escalation argument), at most
    `MaxEscalations` rounds; escalated rows use effect path `<path>#<n>`
    because (run, effect_path) is unique. An expired approval completes the
    effect with `Err(Denied{reason: "expired"})`, which `require` turns into
    `approval_denied`. A decision after `expires_at` is refused (ABORTED
    APPROVAL_ALREADY_DECIDED). "Idempotent per (approval, actor)": the same
    actor repeating the same decision gets the approval back; anything else
    on a decided approval is 409. The approval's `proposed` is `{}` (the
    effect carries no proposal).
11. **Tool inputs** are sent as the single input's JSON, or a JSON array
    for several inputs; proofs are unwrapped and Hosts sent as plain names.
    Outputs are decoded with the tool's `output_type` (`vm.FromJSON`); a
    mismatch is a permanent `invalid_output` failure.
12. **Rate-limit waits** park the run (`waiting`) without an event; the
    limiter itself failing waits `RateLimitErrorBackoff` (fail closed); a
    request that can never pass fails with `budget_exceeded`.
13. **Retry policy.** Retryable means the tool marked the error retryable
    and, if `retry_on` is given, lists its code. MaxAttempts 0 is one
    attempt. `#[retry(max = n, ...)]` is read as n attempts with multiplier
    2.
14. **Snapshots** are taken only for the workflow body when its VM is at
    the live frontier; the fold still reads the whole log (snapshots save VM
    re-execution only). The 5-minute rule uses event timestamps, so with a
    real clock a crash may move where a snapshot is taken; logs are equal
    up to `snapshot_taken` events (`TestEngSnapshots_AreACache`).
15. **The crash property** (eng_008, 25.4) holds for idempotent tools; a
    non-idempotent call cut by a crash legitimately adds
    `effect_outcome_unknown`. `now()` results and all timestamps are
    normalized when logs are compared.
16. **Statuses on the eventstore row** are coarse (cancelling and
    compensating are stored as running; eventstore OPEN_QUESTIONS 8). pgrepo
    keeps the fine status in `engine.run_ext`, which ListRuns filters on.
17. **Registration** requires an entry signature (inputs are decoded from
    it) and rejects agent effects with diagnostic `E0490` (the DSL's E0404
    and E0405 are taken). CheckWorkflow compiles too, because the effect
    names of spec 7.5 call agent steps `llm` and cannot tell them apart.
18. **Archtest.** `engine/pgrepo` imports pgx and needs an adapter entry in
    `internal/archtest` (`{prefix: "engine/pgrepo", layer: layerAdapters,
    adapter: true, thirdParty: []string{"github.com/jackc/"}}` before the
    `engine` entry); until then TestArchDeps reports it.
19. **pgrepo schema additions** (SchemaSQL): `workflow_version_ext`
    (actor text, program, attributes), `run_ext` (fine status, outbox
    watermark), `run_requests`, `approval_ext` (deciding actor). The uuid
    columns `created_by` and `decided_by` of spec 10.4 keep the uuid part
    of `user:<uuid>` actors. Tool "latest version" is the greatest version
    text per name.
20. **Outbox.** Every appended event is published to run.events (and
    approval lifecycle events to approval.events) after the append, not in
    the same transaction (pgstore owns the append transaction). Envelope
    ids derive from (run, seq) and a per-run watermark republishes what a
    crashed worker missed, so delivery is at-least-once without loss.
21. **Graceful shutdown** cancels in-flight executions like a crash: the
    next owner sees `effect_started` without a result.
