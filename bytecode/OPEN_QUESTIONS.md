# bytecode: open questions and decisions

1. **Section details the spec leaves open (8.3; now stated in spec Part II
   §42.1).** FUNCS gets an explicit
   `nspans u32` before the span table; META strings are `u32 len + bytes`;
   TYPES entries are `kind u8, name u32` followed by record fields
   `(name u32, type u32)`, enum variants `(name u32, arity u8, type u32*)`
   or a signature `(func u16, params, result u32)`. Type references are
   String constants holding DSL type text, or `0xFFFFFFFF` for none.
   Flags must be 0 in version 1. Spans must be strictly increasing and
   inside the code.
2. **Constant tags.** 1 Int, 2 Float (finite only), 3 String (UTF-8, at most
   1 MiB), 4 Duration (ns), 5 Time (Unix ns).
3. **No per-function max stack (8.8 "declared maximum").** The verifier
   computes each function's maximum operand depth and requires
   `nlocals + max_depth <= 65,536`; the VM checks the total stack once per
   call instead of per push. The value is returned in `Verified`, not
   stored in the module.
4. **"Each local assigned once" versus loops (8.4, 8.5).** Interpreted per
   iteration: a LOOP back edge does not contribute local definitions to the
   loop header, so locals first written in a loop body are fresh every
   iteration, while a local written before the loop cannot be rewritten in
   it. The spec's for-loop lowering (an incremented index local) therefore
   keeps the index on the stack instead (see doc.go). The only backward
   edge is LOOP, so verification is a single forward pass; a LOOP whose
   target is reachable only through the back edge is rejected.
5. **Which tool an EFFECT calls.** EFFECT has no tool operand, so the tool
   name is the last argument and must be pushed by the CONST immediately
   before the EFFECT (which must not be a jump target). The VM moves it to
   `Args[0]` of the suspension.
6. **Effect classes of non-tool effects.** The mask is u16; bits 8-11 are
   engine effects (scope, approval, alert, plan) the DSL does not name.
   now and sleep count as time, agent_call and embed as llm,
   similarity_query as read, par_map as nothing (its closure's function is
   reachable through CLOSURE). All TOOLS entries are checked against the
   resolver, referenced or not; a nil resolver rejects any TOOLS entry.
7. **Failure code 16 `type_mismatch`.** Not in spec 8.7 as first written
   (8.7 now points to Part II §42.1, which adds it). The verifier checks
   stack shape, not value types, so a module not produced by a correct
   compiler can apply an instruction to the wrong kind; the VM fails with
   this code. Correct compilers never cause it.
8. **Builtins without closures.** sort_by and group_by take precomputed key
   lists (`sort_by_keys`, `group_by_keys`); `first(xs)` is
   `list_get_opt(xs, 0)`; `scope.allows` is the `scope_allows` builtin
   backed by a read-only scope snapshot passed to the VM.
9. **Built-in record layouts** (ScopeDenied, Approval, Denied, PlanError,
   TaskError, Budget) are those of the DSL standard library (dsl
   OPEN_QUESTIONS 18): ScopeDenied `{target, reason, rule_id,
   decision_id}`, Approval `{id, role, decided_by, decided_at}`, Denied
   `{id, role, reason}`, PlanError and TaskError `{code, message}`, Budget
   `{tokens, tool_calls, iterations}`. Plan is provisional (the DSL type is
   opaque).
10. `ErrTooComplex` (bitset memory over 2^24 words) and `ErrTooManyItems`
    guard verifier resources; no realistic compiler output hits them.
11. **Annotations (ANNOTS).** Spec 8.3 has no place for the annotations the
    engine enforces (#[timeout], #[retry], #[effects(max)],
    #[on_unknown(retry)], #[idempotent]). Flag bit 1 (`FlagAnnotations`)
    announces an optional ANNOTS section between META and entry (layout in
    annotations.go): workflow-level values and one entry per annotated
    EFFECT instruction, keyed by (function, code offset) as reported by
    `vm.Suspended.Site`. Without the flag the layout is exactly spec 8.3.
    The verifier checks that the flag and the section agree, that sites are
    sorted, unique and on EFFECT instructions, and that values are in range
    (rule `annotations`).
