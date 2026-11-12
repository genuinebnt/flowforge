# compiler: open questions and decisions

Spec ambiguities met while implementing the lowering of spec 8.5, the
choice made, and where it is pinned.

1. **Option variant numbering.** The DSL prelude declares `Some | None`
   (Some = 0), the VM numbers `None` 0 and `Some` 1 (bytecode/types.go).
   Changing the VM would have changed existing bytecode conformance
   modules, so `compiler/internal/abi.VariantIndex` maps Option variants;
   every other enum keeps declaration order. Both evaluators use it.
2. **`?` modes versus TRY.** The VM's TRY fails the run only in the bottom
   frame of a root VM and otherwise returns the Err/None (vm
   OPEN_QUESTIONS 8). The checker's modes do not always agree: `?` inside a
   lambda or in a fn with a non-Result return must fail the run, and `?`
   in a workflow returning Result must return the Err as the run's value.
   TRY is used where the semantics agree; elsewhere the compiler emits
   TEST_VARIANT / UNPACK with FAIL (unwrap_failed, or approval_denied when
   the error type is `Denied`) or RET. `option_result_try`,
   `workflow_result_try`, `lambda_try_in_fn`, `try_fail_run`.
3. **`?` in a par_map closure** fails that task; with on_error = fail the
   par_map effect then fails the run with effect_failed (the VM's Join),
   not unwrap_failed.
4. **Invalid domain casts.** `s as Fqdn` with an invalid string fails the
   run with unwrap_failed (`cast_invalid_domain`).
5. **Approval timeout default.** `approve(role, reason)` and
   `require approval(...)` without `timeout` pass Duration 0, meaning "the
   engine's default".
6. **agent_call arguments.** `(planner name, goal, context, tool names as
   List<String>, budget)`, with `()` for omitted arguments. The engine
   rejects agent workflows at registration (DECISIONS D1); they still
   compile (`TestCompiler_AgentWorkflowCompiles`, `spec_triage`).
7. **Stream flag.** Loops build plain lists, so `map`/`filter`/... over a
   Stream yield values without the VM's Stream display flag even though the
   type is `Stream<_>`; builtins (dedupe, reverse, take, ...) keep the flag
   as the VM implements them. The flag only affects argument hashes, and
   the tree-walker reproduces the same flags.
8. **Inlining.** Spec 8.5 inlines closures "when they have no captures
   that escape". A lambda literal passed directly to a higher-order library
   function can never escape, so all of them are inlined (no frame, no
   CALL_CLOSURE); any other function value is called with CALL_CLOSURE.
   Inlining is visible only in the call-depth limit, which the tree-walker
   models the same way.
9. **Annotation scope.** Let-statement `#[timeout]`/`#[retry]` apply to the
   EFFECT instructions lexically inside the statement, including lambdas
   written there (compiled as separate functions), but not to effects in
   fns the statement calls: those sites are shared by every caller.
   `#[idempotent]` marks sites of idempotent tools and every site of an
   idempotent fn or workflow.
10. **Hashes.** A tool declared only in source (no registry entry) is
    pinned with `ToolSpecSHA256` of its rendered spec; registry tools use
    the registry hash. `program_policy_sha256` is the SHA-256 of the
    canonical JSON `{"known":..,"max_class":..}`.
11. **Entry selection.** A file may hold several workflows; Compile
    requires `Options.Workflow` then (E0201 otherwise) and compiles only
    what the entry reaches (fns unreachable from it are left out unless
    `AllFns`).
12. **Standard-library records without reserved ids** (Service,
    TlsCertificate, Product, Organization, RunInfo, GraphSummary) are always
    emitted as TYPES entries, so the engine can decode tool results that
    mention them (vm.ParseType finds them by name).
13. **EFFECTS mask.** The declared mask is the workflow's inferred DSL
    effects plus the bits of every EFFECT the compiler emitted (tools'
    declared effects, scope, approval, alert, plan, time, llm, read).
    `Result.Effects` lists its names.
14. **Compensation paths.** The engine chooses the effect prefix of a
    compensation run; the tests use `[next_seq, k]` for the k-th
    compensation (vm OPEN_QUESTIONS 1).
15. **VM versus tree-walker speed (b_vm_01).** The VM is faster on calls
    (fib) but slower when building large lists: each LIST_PUSH accounts the
    heap and grows the backing store element by element, while the
    tree-walker appends to preallocated Go slices. Recorded, not hidden.
16. **parse_int and underscores.** The bytecode builtin table documents
    `_` separators, the VM's implementation rejects them
    (`parse_and_to_string` pins the implemented behaviour).
17. **Fuel** is not modeled by the tree-walker; vm_008 runs the VM without
    the case's fuel limit. String, list, effect-count and call-depth limits
    are modeled.
