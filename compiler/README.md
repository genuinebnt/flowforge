# compiler — DSL to bytecode

Lowers a checked Flowforge program (package `dsl`) to a verified bytecode
module (package `bytecode`) and provides the tree-walking reference
evaluator (`compiler/treewalk`) the differential tests compare the VM
against. Layer 4; imports `core`, `dsl`, `bytecode` (tests also `vm`).

Spec sections: 8.3 (module sections), 8.5 (lowering rules), 8.8 (the
verifier runs on every output), 7.5 (annotations), 25.3 vm_008, 25.4
(tree-walker equals VM), 25.5 (fuzz), 25.6 (differential), 25.8 b_vm_01.

## API

```go
res, diags := compiler.Compile(src, compiler.Options{
    FileName: "recon.ff",           // diagnostics; "<workflow>.ff" by default
    Policy:   dsl.Policy{MaxClass: core.ClassActive, Known: true},
    Tools:    []compiler.Tool{{Spec: dsl.ToolSpec{...}, SpecSHA256: h, Idempotent: true}},
    Workflow: "recon",              // the entry; required when the file has several
})
// res == nil: diags holds the errors (spec 7.7 JSON shape), in source order.
res.Module, res.Encoded, res.ModuleSHA256, res.Verified, res.Tools
res.Effects        // ["passive","active","scope","alert"]: module mask names
res.Class          // core.ActivityClass of the workflow
res.Ops            // EFFECT kinds used ("tool_call", "par_map", ...)
res.Diagnostics    // warnings
res.SourceSHA256, res.ProgramPolicySHA256
res.Annotations()  // *bytecode.Annotations or nil
res.SourceSpan(off, len, label)  // vm.Failed.Span -> line/column
res.Program, res.Closures        // for compiler/treewalk
```

Compile parses, checks (with the registry tools injected as declarations),
lowers the entry workflow and everything it reaches, assembles the module,
encodes it and runs `bytecode.Verify` with a resolver built from the
registry. A verifier rejection is an internal compiler error (E0699, never
the program's fault; the tests treat it as a bug); exceeding a module limit
is E0602. Compile never panics (a recovered panic would also be E0699).

`Options.AllFns` also compiles fns the entry does not reach (tooling and
the whole-file property test).

### Engine adapter

`EngineAdapter{Version}` has `Check(ctx, AdapterRequest)` and
`Compile(ctx, AdapterRequest)`, mirroring the engine's `Compiler` port:
the request carries the workflow name, the source, an optional
`AdapterPolicy{MaxClass}` and the registered tools as
`AdapterTool{Name, InputType, OutputType, EffectClass, Idempotent,
SpecSHA256}`; results are `AdapterCheckResult{Diagnostics, Effects, Class,
OK}` and `AdapterCompileResult{..., Module, ModuleSHA256, Attributes}`
with `Attributes{TimeoutNanos, MaxEffects, OnUnknownRetry, Idempotent}`.
The adapter imports nothing from `engine`; `services` converts the structs
(the diagnostic JSON is identical). Tool signatures are built as
`(InputType) -> OutputType`, or `InputType -> OutputType` when the input
type is already a parenthesized tuple or `()`.

## Lowering strategy

| Source | Lowering |
|--------|----------|
| literals, `[..]`, `(..)`, records | CONST / TRUE / FALSE / UNIT, MAKE_LIST, MAKE_REC Tuple, MAKE_REC id n in declaration order (fields evaluated in source order, through temps when the orders differ) |
| variants, Option/Result/Host/Severity | MAKE_ENUM with the reserved ids (Option 0, Result 1, Host 2, Severity 3; ScopeDenied 5, Approval 6, Denied 7, PlanError 8, TaskError 9, Budget 11); user types from 32 |
| names | LOAD_LOCAL, LOAD_CAP for captured bindings; a fn as a value is CLOSURE fn 0; a tool, library function or variant constructor as a value is CLOSURE of a wrapper function |
| `a && b`, `a \|\| b` | JUMP_IF_FALSE short-circuit |
| arithmetic | *_I for Int, Duration and Time (the VM's checked integer ops implement the checker's table), *_F for Float, CONCAT for String `+` |
| `x in xs`, `a..b` | BUILTIN contains (x evaluated first), BUILTIN range |
| `as` | by `CastExpr.Kind`: int_to_float, float_to_int, Int/Duration/Time through MUL_I/DIV_I/ADD_I/SUB_I with unit constants, to_string, `fqdn_parse` & co. followed by an unwrap that fails with unwrap_failed |
| `"..{e}.."` | pieces then INTERP n (INTERP 255 + CONCAT for longer strings) |
| `if` | JUMP_IF_FALSE / JUMP |
| `match` | scrutinee in a temp; per arm TEST_VARIANT + JUMP_IF_FALSE to `pop: POP; fail:`, UNPACK into fresh locals, nested tests, guard JUMP_IF_FALSE to `fail:`, body, JUMP end; final FAIL match_failed |
| `e?` | TRY where its VM semantics match `TryExpr.Mode` (fail the run in the workflow body, return Err/None from a fn); otherwise TEST_VARIANT / UNPACK with FAIL (unwrap_failed, or approval_denied for a `Denied` error) or RET |
| `require approval(..)` | EFFECT approval (role, reason, timeout; Duration 0 = engine default when omitted), then TRY in the workflow body or an explicit FAIL approval_denied elsewhere; POP |
| `for p in xs` | loop below; maps iterate as `zip(map_keys, map_values)` |
| map, filter, filter_map, flat_map, fold, any, all, first, count, sort_by, group_by | loops; flat_map = map + flatten; sort_by/group_by compute the keys with a map loop then sort_by_keys / group_by_keys; `first(xs)` is list_get_opt(xs, 0), `count(xs)` is len |
| function arguments of those | lambda literals inlined (their parameters alias the element local), fns CALL, tools/variants/library functions called directly, any other value CALL_CLOSURE |
| `par_map(xs, f, limit, on_error)` | one EFFECT par_map (list, closure, limit, 0 = fail / 1 = collect) |
| tool call | inputs in parameter order, CONST name, EFFECT tool_call argc+1 |
| scope.check / scope.allows | EFFECT scope_check / BUILTIN scope_allows |
| now, sleep, log, emit_alert, embed, similar, approve, execute | their EFFECT kinds |
| `agent name(..)` | EFFECT agent_call (planner name, goal, context, tool names, budget; () when absent) |
| `let x = e compensate c` | e; STORE x; LOAD x (as `it`), captures; PUSH_COMP |

Loops keep their state (index, accumulators) on the stack across LOOP and
store it into fresh locals at the head; the list and its length are locals
written once before the loop:

    <xs> STORE xs; LOAD xs; LIST_LEN; STORE n; <acc..>; CONST 0
    head: STORE i; STORE acc..; LOAD i; LOAD n; LT; JUMP_IF_FALSE exit
          LOAD xs; LOAD i; LIST_GET; STORE x; ...body...; <acc'..>
          LOAD i; CONST 1; ADD_I; LOOP head
    exit: LOAD acc

`any`, `all` and `first` leave early with a forward JUMP from the body.

### Local allocation

Parameters are locals 0..n-1. Every other binding gets a fresh local where
its single STORE is emitted, and so does every temp (scrutinee, loop state,
reordered arguments). Slots are never reused, so each local is written at
most once per path; a binding first written in a loop body is fresh in
each iteration and never written before the loop, which is exactly what
the verifier's single-assignment rule (bytecode OPEN_QUESTIONS 4) needs.
Variable patterns alias the local that already holds the value.

### Span tables

Before the first instruction of each expression, and again before the
instruction that can fail (arithmetic, INDEX, CALL, EFFECT, TRY, FAIL,
MAKE_*), the expression's source range is recorded. `vm.Failed.Span`
therefore points at the failing expression; `Result.SourceSpan` converts
it to line and column.

### Annotations

The module format of spec 8.3 has no place for them, so the module gets an
optional ANNOTS section (bytecode `FlagAnnotations`, `bytecode.Annotations`)
that is present only when there is something to say:

* workflow: `#[timeout(d)]` (run deadline), `#[effects(max = N)]`,
  `#[on_unknown(retry)]`, `#[idempotent]`;
* per EFFECT instruction (function index and code offset): the
  `#[timeout]` and `#[retry(max, backoff = exp(initial, max))]` of the let
  statement that lexically contains it, including effects in lambdas
  written inside that statement (the httpx calls inside the recon par_map
  closure inherit the statement's retry), plus `idempotent` when the called
  tool is `#[idempotent]` (or the registry says so) or the enclosing fn or
  workflow is.

`vm.Suspended.Site` gives the engine the function and offset of a
suspension; `Annotations.Site(fn, offset)` returns its policy.

## Changes to other packages

* **bytecode**: new optional ANNOTS section between META and entry,
  announced by flag bit 1 (`FlagAnnotations`); `Module.Annotations`,
  `Annotations`, `SiteAnnotation`, `Builder.SetAnnotations`,
  `MaxEffectsAnnotation`, decode error `ErrBadAnnotations`, verifier rule
  `ErrAnnotations` (`annotations`); modules without annotations are
  byte-identical to before. Built-in record layouts now match the DSL
  prelude: ScopeDenied `{target, reason, rule_id, decision_id}`, Approval
  `{id, role, decided_by, decided_at}`, Denied `{id, role, reason}`. Two
  new `conformance/bytecode/verify` cases (`accept_annotations`,
  `annotations_site`) and `TestBC_Annotations`.
* **vm**: `Suspended.Site` (also restored from snapshots);
  `CallBuiltin` (the native builtins outside a run, used by the
  tree-walker); `NewClosure`; `ParMap.Join` fails with
  `nondeterminism_detected` when any task failed with it (it used to become
  `effect_failed` or a collected Err). Tests updated for the record layouts;
  `TestVM_SuspendedSite`, `TestVM_CallBuiltin`.
* **dsl**: prelude type `TaskError {code, message}` (appended, existing ids
  unchanged); `par_map`'s `on_error` must be a string literal, and
  `"collect"` gives `Seq<Result<B, TaskError>>` (it was typed `Seq<B>`,
  which did not match the VM's result).
* **dsl** (v2): prelude records `Snapshot`, `Features`, `ChangeSet`,
  `ChangeHit` (V2_DESIGN §8), appended after TaskError. They are
  non-reserved builtin records, so every module gains their four TYPES
  entries and user type ids move up by four; the replay goldens were
  regenerated (dsl OPEN_QUESTIONS 36).

## Tests and benchmarks

    go test ./compiler/...
    go test ./compiler -run 'TestVMConformance' -update     # regenerate conformance/vm goldens, then review
    go test ./compiler -run XXX -fuzz FuzzCompile -fuzztime 60s
    go test ./compiler -run XXX -bench . -benchtime 3x
    FF_PROPTEST_CASES=20000 go test ./compiler -run Prop

| Test | What |
|------|------|
| `TestVMConformance_Run` | conformance/vm/run (vm_001-vm_003, vm_006, vm_007 material): value or failure and effect trace; fuel slicing does not change a run |
| `TestVMConformance_Replay` | conformance/vm/replay (vm_004): module.hex is still what the compiler produces; replay equals live; tampered logs give nondeterminism_detected |
| `TestVM008_TreeWalkVsVM` | vm_008 on every run case |
| `TestCompilerProp_TreeWalkEqualsVM`, `TestCompilerProp_GeneratedVerify` | generated well-typed programs (`gen_test.go`) |
| `TestCompilerProp_VerifierAcceptsOutput` | every checked program of conformance/dsl/check compiles and verifies |
| `TestCompilerCoverage_BuiltinsAndEffects` | the run cases use every library function, every EFFECT kind and every native builtin the compiler emits |
| `TestCompilerAnnotations_*`, `TestCompiler_*`, `TestEngineAdapter` | annotations, metadata, diagnostics, spans, agent workflows, adapter |
| `FuzzCompile` | arbitrary source: diagnostics or a verified module, never a panic or E0699 |
| `BenchmarkVM01_TreeWalkVsVM` | b_vm_01: fib(30), an arithmetic loop, map/filter over 1M, string building; reports `treewalk/vm` |
| `BenchmarkCompile` | 10 KB and 1 MB generated programs (MB/s) |

Last recorded b_vm_01 ratios (treewalk time / VM time, `-benchtime 3x`):
fib30 2.3, loop_arith 1.1, map_filter_1M 0.5, string_build 1.1. The VM wins
on calls; on list building its LIST_PUSH growth and heap accounting cost
more than the tree-walker's Go slices (see OPEN_QUESTIONS.md).

## Known limitations

See `OPEN_QUESTIONS.md`.

## Choosing an algorithm

Decision point: how a compiled workflow is evaluated (spec 8). The
bytecode VM (`vm`) runs workflows; the tree-walking interpreter
(`compiler/treewalk`) is the reference the differential tests compare it
with (values and effect traces).

| Evaluator | fib30 | loop_arith | map_filter_1M | string_build |
|---|---|---|---|---|
| `vm` (default) | 141 ms | 118 ms | 305 ms | 7.9 ms |
| `treewalk` | 325 ms | 122 ms | 182 ms | 8.1 ms |

`go test -run '^$' -bench VMAlgo ./compiler` (3 iterations). Measured 2026-11-13 on a 4-vCPU Linux container (Go 1.26, `GOMAXPROCS=4`); see `docs/ALGORITHMS.md` for the consolidated entry. The VM
wins on calls (2.3x on fib30) and allocates far less; it is slower on list
building (map_filter_1M), the open b_vm_01 item G-LANG-02. The VM stays
the default because only it supports fuel, snapshots and replay.
