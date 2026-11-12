# vm: open questions and decisions

1. **Effect sequence numbers start at 0** ("0", "1", ...). par_map task t of
   effect P numbers its effects P/t/0, P/t/1, ...; the task index is the
   element index (0-based). For compensations the engine chooses the prefix;
   the tests use `[next_seq, k]` for the k-th compensation run.
2. **Fuel.** `Config.FuelLimit` is the run total (default 50,000,000);
   exhausting it fails with `out_of_fuel`. `Run(fuel)` / `Resume(r, fuel)`
   take a slice; exhausting a slice returns the resumable `OutOfFuel`. Fuel
   is checked before an instruction executes, so stopping never changes the
   result. Unused fuel of a slice that ended in `OutOfFuel` carries over, so
   any positive slices make progress (LOOP 5, EFFECT 50, BUILTIN 1 + n/16).
   `range` is charged for the elements it produces, since its inputs are two
   integers.
3. **Live heap (64 MiB).** Allocations are counted; when
   `live_at_last_measure + allocated_since` would exceed the limit the VM
   measures the live heap exactly (each object once, constants excluded).
   It only re-measures after 1/8 of the limit has been allocated, otherwise
   fails, so the effective limit is between 7/8 and 1x the configured value
   (fail closed). Both counters are in the snapshot and snapshots keep
   object sharing, so the decision is deterministic across restore.
   Values entering the VM (inputs, effect results) are deep-copied so the
   accounting does not depend on how the engine built them.
4. **Lists** share backing stores: LIST_PUSH on the longest prefix extends in
   place (amortized O(1)) unless the pushed value may reference that store
   (bounded check), which keeps the object graph acyclic. `take` shares.
5. **Floats.** NaN raises `arith_nan`; an infinite result raises
   `arith_overflow` and float division by zero raises `div_zero`, so every
   Float value is finite and JSON-safe.
6. **Comparisons.** EQ/NE are structural across all kinds (different kinds
   compare unequal); LT..GE require the same kind and use the total
   structural order of `Compare` (also used by `sort`).
7. **Effect results.** `EffectResult.Failed` fails the run with its code
   (default `effect_failed`). Domain errors (scope denial, approval denial)
   are Ok values of type Result built by the engine with the built-in
   ScopeDenied / Denied records.
8. **TRY in the workflow body** fails with `approval_denied` when the Err
   payload is the built-in `Denied` record, otherwise `unwrap_failed`. In
   par_map tasks and compensations TRY returns from the closure.
9. **par_map** `Join` with on_error = fail reports the first failing task in
   input order (the engine should cancel siblings when the first failure
   arrives); with collect each element is `Ok(v)` or
   `Err(TaskError{code, message})`. The result keeps the Stream flag. A
   task that failed with `nondeterminism_detected` (its replay diverged
   from the log) fails the par_map with that code in both modes, so the
   run fails with it instead of `effect_failed` or a collected `Err`.
10. **scope.allows** is the `scope_allows` builtin over `Config.Scope`, a
    read-only snapshot of the run's pinned scope version (false when nil).
    The engine must supply the same snapshot on replay; it is the one VM
    input besides the module and recorded effects.
11. **#[effects(max = N)]** has no field in the module format; the engine
    passes N as `Limits.Effects` (at most 1,000,000).
12. **JSON forms.** Option is null or the payload (nested `Some(None)` is not
    distinguishable from `None`); Result is `{"ok": v}` / `{"err": e}`;
    other enums `{"variant": name, "values": [...]}`; Map with string-like
    keys is an object (decoded in sorted key order, so insertion order is
    not preserved), other maps are `[[k, v], ...]`; Duration is Go duration
    text, Time RFC 3339 UTC. ScopeProofs are rejected in outputs and
    unwrapped to their target only with `JSONOptions.UnwrapProofs` (tool
    inputs).
13. **Display (INTERP, to_string).** Strings are raw at top level and quoted
    inside collections; floats always show a decimal point; Duration uses Go
    duration text.
14. **Snapshots** are a fixed 19-field CBOR array (documented in
    snapshot.go) and must be canonical: Restore re-encodes and compares. The
    pending effect's arguments are part of the snapshot.
15. **Suspension sites.** `Suspended.Site` locates the EFFECT instruction
    (function, code offset, closest source span); it is recomputed from the
    saved pc on Restore and is not part of the effect identity (path, kind
    and argument hash are). The engine uses it to find the per-site
    annotations of the module (bytecode OPEN_QUESTIONS 11).
