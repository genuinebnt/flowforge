# conformance/vm

Language-neutral cases for the bytecode VM (spec 19, 25.3, 25.9). Each case
is a directory with DSL source; an implementation compiles it, runs it
against scripted effects and compares with the golden. The Go reference
runs them in `compiler` (`TestVMConformance_Run`, `TestVMConformance_Replay`,
`TestVM008_TreeWalkVsVM`) and regenerates goldens with
`go test ./compiler -run TestVMConformance -update` (review the diff).

## run/<case>/

```
case.ff        source; the workflow to run is effects.json "workflow" or the only one
effects.json   optional: inputs, registry, limits and scripted effect results
expected.json  final value or failure, and the effect trace
```

`effects.json`:

| key | meaning |
|-----|---------|
| `workflow` | entry workflow (when the file has several) |
| `inputs` | JSON object of workflow inputs (decoded with the entry signature) |
| `tools` | registry tools `[{name, signature, effects}]` (source `tool` items work too) |
| `scope_allows` | targets (display text) the pinned scope snapshot allows; backs `scope.allows` |
| `fuel_slice` | run with this fuel per Run/Resume (resumable OutOfFuel); the result must equal an unsliced run (vm_003) |
| `fuel_limit` | total fuel of the run (`out_of_fuel` when exhausted) |
| `par_map_order` | `{ "<par_map path>": [task order] }`: completion order of the tasks; results are joined in input order (vm_006) |
| `no_compensate` | do not run compensations after a failure |
| `results` | scripted results by effect path (`"3"`, `"6/1/0"`) |
| `by_tool`, `by_kind` | fallback results by tool name, then by effect kind |

A scripted result is one of `{"type": "<DSL type>", "value": <JSON>}`
(JSON forms of vm OPEN_QUESTIONS 12), `{"scope": "allow", "decision": id}`
(Ok of a proof for the checked target), `{"scope": "deny", "reason": r,
"decision": id}` (Err ScopeDenied), `{"approval": "approve", "by": who}`
(Ok Approval with id `ap-<path>`, the requested role, decided_at
2023-11-14T22:13:20Z), `{"approval": "deny", "by": who, "reason": r}`
(Err Denied), or `{"fail": code, "message": m}` (effect failure).
Unscripted `sleep`, `log` and `emit_alert` return `()`; any other
unscripted effect fails with `effect_failed`.

The run follows the engine: par_map tasks get paths
`<par_map>/<task>/<seq>`; on failure the compensations run in reverse
registration order, the k-th with prefix `<next_seq>/<k>`.

`expected.json`: `value` (vm.ToJSON of the result) and `display` (its
display text), or `failure` (the code); `trace`, one entry per effect in
scheduling order, compensations included: `path`, `kind`, `args` (display
text of each argument; for tool_call the tool name first), `args_hash`
(SHA-256 of the canonical CBOR arguments, vm.ArgsHash), `result` (`ok
<display>` or `failed <code>`); `compensations`, the number run.

Cases cover arithmetic (overflow, div_zero, floats), comparison and
short-circuit, strings and interpolation, lists, every higher-order
function, maps, records, enums and exhaustive matches with guards,
Option/Result and `?` in every mode, closures (by value, nested,
returned), recursion and the call-depth limit, loops, casts, tool calls,
scope.check / scope.allows, approvals (approve, deny, match), par_map
(shuffled completion, collect, fail-fast), compensation order and the
no-failure case, now/sleep/log/emit_alert, embed/similar, streams,
generics, fuel resume and exhaustion, the three programs of spec 7.1, and
the V2 content records returned by the content tools (`content_records`:
Snapshot with nested resources, Features, ChangeSet, ChangeHit).

## replay/<case>/

```
case.ff        source
effects.json   as above (used to record the log); "tamper": path corrupts that entry
module.hex     the encoded module (the compiler must still produce it byte for byte)
log.json       event-log-like trace: [{path, kind, args_hash, value_cbor | fail, message}]
expected.json  module_sha256, the run's value/failure and trace, replayed and executed counts
```

The module is run with no live effects: every suspension is answered from
`log.json` by path when the kind and args_hash match (spec 8.6). An intact
log reproduces the live run (no effect executed); a tampered one fails with
`nondeterminism_detected`, including inside a par_map task.
