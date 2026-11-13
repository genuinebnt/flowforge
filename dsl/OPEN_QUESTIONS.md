# dsl: open questions and decisions

Spec ambiguities met while implementing section 7, the choice made, and
the test that pins it. Where scope or policy is involved the conservative
(fail-closed) reading was taken.

## Lexing and syntax

1. **Record literal separators.** The EBNF says `IDENT ":" expr` but the
   spec's own example writes `Budget { tokens = 20000, ... }`. Both `:` and
   `=` are accepted; `Format` prints `:`. Pinned by `g_primary`,
   `spec_triage`.
2. **Effect clause braces.** Section 22.9 writes `!{read}`. Accepted as an
   alternative to `! read`; printed without braces (`g_tool_decl`).
3. **`Unit` as a type name.** The spec writes both `()` and `-> Unit`; both
   denote the unit type.
4. **Minimum Int literal.** `-9223372036854775808` overflows (the literal
   is lexed before the minus). Write `-9223372036854775807 - 1`. E0003.
5. **`1e5` and `1.5s`.** A float needs a fractional part; `1e5` is reported
   as an unknown duration unit `e5` (E0004) with a hint; `1.5s` is E0004.
6. **Lone `}` in a string** is E0008 (write `}}`), as is `{}`. Strings may
   span lines; an unterminated string ends at its first newline for
   recovery (E0001).
7. **Block-like expression statements.** As in Rust, `if`, `match` and a
   block at the start of a statement end the statement without a `;`.
   `Format` parenthesizes an expression statement whose leftmost operand is
   block-like.
8. **No record literals in `if`/`match`/`for` heads** unless parenthesized
   (the usual ambiguity with the following block). Match guards do not
   start lambdas (`Some(x) if x => ...` is a guard `x`).
9. **Annotations on statements other than `let`** are E0603 (reported by
   the parser).
10. **Assignment.** There is no assignment syntax; `x = e;` is parsed so
    that the checker can report E0312 ("cannot assign twice to immutable
    binding") instead of a bare syntax error (dsl_006).
11. **Codes not named by the spec.** Lexing: E0005 unexpected character,
    E0006 unterminated block comment, E0007 malformed number, E0008 bad
    interpolation. Names: E0204 unknown type, E0205 duplicate item, E0206
    unknown variant. Types: E0305 not callable, E0306 invalid `?`, E0307
    field errors, E0308 named arguments, E0309 cast, E0310 operator
    operands, E0311 not displayable, E0312 immutable, E0313 `return` in a
    lambda, E0314 map key. Effects: E0404 compensate without a write, E0405
    unknown effect. Patterns: E0502 refutable `let`/`for` pattern, E0503
    pattern type mismatch, E0504 not a variant, E0505 pattern arity, E0506
    literal pattern type, E0507 record pattern. Annotations: E0603
    misplaced, E0604 bad arguments. Warnings: W0601 use of a deprecated
    item. Limits: E0602 also covers nesting deeper than 256 and operator
    chains longer than 4096 (protects the recursive descent).

## Types

12. **`?` outside Result functions.** The spec fixes Result functions
    (error types must be equal) and workflows with non-Result returns (fail
    the run). Decisions: in a function returning `Option<_>`, `?` on an
    Option returns None; `?` on a Result in any non-Result function fails
    the run; `?` on an Option in a Result function is E0306; inside a lambda
    or compensation `?` always fails the run (this is what the spec 7.1
    `par_map(n => httpx(scope.check(...)?))` needs). `t_try_modes_ok`,
    `t_E0306_try_error_type`.
13. **`return` inside a lambda** is E0313 (a lambda's value is its body).
14. **Indexing.** `xs[i]` on List/Stream yields `T` and `m[k]` on a Map
    yields `V`; out-of-range or missing keys are run-time failures.
15. **Ranges** `a..b` have type `List<Int>` (spec 7.5: Stream/List are
    materialized). `x in xs` works on List, Stream, Map (keys) and String
    (substring).
16. **Casts.** `as` converts Int↔Float, Int↔Duration, Int↔Time, a domain
    scalar to String, and String to a domain scalar (validated at run time;
    this is how validated construction of `Fqdn` etc. is written). Casting
    to `InScope<T>` is E0302.
17. **Arithmetic** follows a fixed table (Int, Float, Duration, Time with
    Duration, String `+`); no implicit conversions (`t_E0310_no_implicit_coercion`).
18. **Prelude types the spec names without fields.** Fields chosen for
    Product, Service, TlsCertificate, Organization, ScopeDenied, Approval,
    Denied, RunInfo, PlanError, Budget (`tokens`, `tool_calls`,
    `iterations`, from the example) and GraphSummary (from the planner
    request of 16.2). `Scope`, `Entity`, `Vector`, `Plan<T>` are opaque.
19. **`log(level, msg)`** takes the level as a String. `par_map`'s
    `on_error` must be the String literal `"fail"` or `"collect"` (E0308
    otherwise), because it decides the result type: with `"collect"` the
    result is `Seq<Result<B, TaskError>>` (spec 8.6), where `TaskError
    {code, message}` is a prelude record, the error of a failed task.
20. **Stdlib additions** beyond 7.6: `get`, `keys`, `values` on maps (the
    spec has no map literal or accessor otherwise).
21. **Function values and effects.** Function types written in source are
    pure; passing an effectful function where one is expected is E0401.
    Higher-order builtins propagate their function arguments' effects.

## Effects, scope and policy

22. **Which engine operations are effect kinds.** Only the eight kinds of
    7.5 appear in `inferred_effects`; scope.check, approve, emit_alert, log
    and execute are runtime operations (spec 8.4) without a kind, which
    matches spec 12.6 (`["passive","active"]` for recon). `similar` is
    `read`, `embed` and agent calls are `llm`, `sleep`/`now` are `time`.
    `execute(plan)` adds no static effect: the plan was checked when it was
    produced.
23. **A `fn` without `!` is pure** (E0401 if its body performs effects);
    a workflow without `!` has its effects inferred. Workflows cannot be
    called from code (E0305).
24. **Policy only with a known program.** E0402 and E0403 are checked only
    when `Options.Policy.Known`; with no program the checker checks
    internal consistency (spec 7.5).
25. **What counts as an approval for E0403.** `require approval(...)`,
    `approve(...)?`, and the `Ok` arm of `match approve(...) { ... }`. A
    bare `approve(...)` whose result is ignored does not count. Approvals
    inside one branch, a loop body, the right operand of `&&`/`||` or a
    lambda do not count after it. A lambda's body is checked with the
    approval state at its creation, so a lambda created before an approval
    and called after it is reported (fail closed). Fns export the gated
    effects they perform without an internal approval; their callers must
    be approved (`f_E0403_through_fn`).
26. **Scope targets.** `scope.check` and `scope.allows` accept Fqdn,
    IpAddress, Netblock, Asn, Url, Host and Entity only (E0301 otherwise).
27. **Where proofs may not go (E0302).** User record fields and enum
    payloads (declared, or instantiated through generics at a record
    literal, as in the 7.7 example), workflow parameters and results (they
    cross run boundaries), tool results (a tool cannot mint a proof),
    `Plan<T>` with a proof inside, agent `context`, and casts.
28. **Registry agreement.** A source `tool` declaration that duplicates a
    registry tool must match it exactly, else E0203.

## Diagnostics

29. **REST example span.** Spec 12.6 shows the E0301 primary span starting
    at col 49 / offset 48 and ending at col 71 / offset 70 for
    `httpx(Host::Name(roots[0]))`. The end matches the argument expression
    `Host::Name(roots[0])`; the start in the spec points two bytes earlier
    (inside `httpx(`), which looks like a counting slip. We report the
    argument's span (col 51 / offset 50 to col 71 / offset 70) and the
    spec's exact message, label and help. `rest_check_example`,
    `TestDSL005_ScopeTyping/rest_example_location`.
30. **Columns** count Unicode code points (1-based); offsets are bytes;
    end positions are exclusive.
31. **File name in Analyze.** CheckWorkflowRequest carries no file name;
    `Analyze` uses `Options.FileName`, else `<first workflow>.ff` (the REST
    example shows `recon.ff`), else `main.ff`.
32. **`help`** is omitted from the JSON when empty; `secondary` is always
    present (possibly empty), as in the REST example.

## Not covered

33. The `scope_check(h) is InScope(_)` form in 22.9's example is not in
    the grammar of 7.3 and is not supported; write
    `scope.allows(h)` or match on `scope.check(h)`.
34. Exhaustiveness checking gives up silently on pathological matches
    (more than 200,000 usefulness steps); such a match keeps its run-time
    `match_failed` fall-through.

## V2 content records

35. **Fields of Snapshot, Features, ChangeSet and ChangeHit.** V2_DESIGN §8
    leaves their fields to Part II; they are chosen in `contentRecords`
    (prelude.go) from what the content service stores and what a
    monitoring workflow decides on: a Snapshot names its subresources
    (`resources: List<Snapshot>`, the only self-referencing prelude
    record; one level deep in practice), Features keep URL kinds as `Url`
    and graph keys as `String`, a ChangeSet carries `score` for ordering
    but not the score's reasons (V2 §12.4: scores rank, never send), and
    secrets appear only as `<rule>:<redacted preview>`.
36. **Adding prelude records changes module bytes.** Every non-reserved
    prelude record gets a TYPES entry in every module (compiler abi
    `NewTypes`), so appending the four content records changed the bytes,
    hashes and user type ids (and with them the argument hashes of
    effects that carry user records) of every compiled workflow;
    `conformance/vm/replay/*/module.hex` and the affected `expected.json`
    files were regenerated. Registered workflows keep their pinned module
    bytes, so running and replaying them is unaffected; only a recompile
    yields a new module hash.
