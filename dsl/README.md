# dsl — Flowforge language front end

Lexer, parser, syntax tree, name resolution, type checker, effect and
policy checker, and pretty-printer for the Flowforge DSL. Its output is a
fully typed and resolved syntax tree that the bytecode compiler and the
tree-walking evaluator lower without redoing any analysis.

Spec sections: 7 (all of it), 8.5 (what the tree must give the compiler),
8.4 (runtime effect kinds), 11.2 / 11.4 / 12.6 (diagnostic and
CheckWorkflow shapes), 16.1 (agent plans use the same checker), 22.9
(`similar`, `embed`), 25.3-25.8 (tests).

## API

```go
f, diags := dsl.Parse("recon.ff", src)          // *File, syntax diagnostics
prog, cdiags := dsl.Check(f, dsl.Options{       // annotates f in place
    Policy: dsl.Policy{MaxClass: core.ClassActive, Known: true},
    Tools:  []dsl.ToolSpec{{Name: "resolve", Signature: "(Fqdn) -> Option<IpAddress>", Effects: []string{"passive"}}},
})
res := dsl.Analyze(source, opts)                // {diagnostics, inferred_effects, inferred_class, ok}
out := dsl.Format(f)                            // canonical source; Parse(Format(f)) == f
js, _ := dsl.ASTJSON(f)                         // canonical AST JSON
```

What the compiler reads after `Check` (all documented on the types):

| Node | Fields |
|------|--------|
| every `Expr` | `Type()`, `Effects()` |
| `*Ident`, `*PathExpr` | `Ref{Kind, Binding, Capture, Fn, Tool, Builtin, Variant}` |
| `*Binding` | unique `ID`, `Kind` (param, let, pattern, lambda param, `it`), `Frame`, `Slot` |
| `*Lambda` | `Frame`, `Captures []*Capture{Binding, Outer}`; compensations are `Synthetic` lambdas over `it` |
| `*CallExpr` | `Kind` (builtin/tool/fn/variant/closure), `Builtin`/`Tool`/`FnDecl`/`Variant`, `Own` effects, `TypeArgs`, `Arg.Param` |
| `*FieldExpr` | `Index` (record field) or `Builtin` (`scope.check`, `scope.allows`) |
| `*TryExpr` | `Mode` (return Err / return None / fail run), `OnOption` |
| `*CastExpr` | `Kind` |
| `*ForStmt` | `ElemType`, `IterKind` |
| `*RecordLit`, `*RecordPat` | `Def`, field `Index` |
| `*BindPat`, `*VariantPat` | `Binding` or `Variant` |
| `*LetStmt`, items | `Attrs` (timeout, retry/backoff, idempotent, deprecated, effects max, on_unknown) |
| `*FnDecl`, `*WorkflowDecl` | `Func *FuncInfo` (params, result, declared/inferred effects, `Ops`, `Tools`, `Frame`) |
| `*Program` | `Types` (ID order), `Tools`, `Fns`, `Workflows` (effects, class, ops, tools) |

`BuiltinTypes()` lists the prelude types in ID order: the builtin
generics and scalars, the records of spec 7.4 (Service, TlsCertificate,
...), and the V2 content records `Snapshot`, `Features`, `ChangeSet` and
`ChangeHit` (V2_DESIGN §8; fields in `contentRecords` and
`OPEN_QUESTIONS.md` 35), which the content tools return.

`Builtins()` returns the standard library with stable IDs; `Builtin.Op` is
the EFFECT kind of spec 8.4 for effectful builtins (`OpNone` for pure ones,
which lower to `BUILTIN id`).

## Effects

Static effects are the eight kinds of spec 7.5 (read, passive, active,
intrusive, write, financial_write, llm, time), reported in that order.
Engine operations that are not effect kinds (scope_check, approval,
emit_alert, log, execute_plan, agent_call, par_map, ...) are tracked
separately as `OpSet` so the compiler knows what suspends the VM; this is
why the recon program of spec 7.1 infers `["passive", "active"]` as in
spec 12.6. A `fn` without `!` is pure. Function types written in source
are pure; the higher-order builtins (map, filter, par_map, ...) are
effect-polymorphic.

## Tests

```
go test ./dsl/...                                   # unit, conformance, property, fuzz seeds
go test ./dsl -run 'TestDSLConformance' -update     # regenerate conformance/dsl goldens
FF_PROPTEST_CASES=100000 go test ./dsl -run Prop    # nightly-size property runs
go test ./dsl -run X -fuzz '^FuzzDSLChecker$' -fuzztime=60s
go test ./dsl -run X -bench BenchmarkDSL01 -benchtime=10x
```

Required cases: `TestDSL001_Grammar` … `TestDSL006_Closures` (dsl_001 to
dsl_006), `TestDSLProp_PrintParseRoundTrip`, `TestDSLProp_WellTypedChecks`,
`FuzzDSLLexer`, `FuzzDSLParser`, `FuzzDSLChecker`, `BenchmarkDSL01_FrontEnd`.
Conformance data lives in `conformance/dsl/{parse,check}` (format
documented in `conformance/dsl/README.md`).

## Known limitations

* Comments are not kept in the tree, so `Format` drops them.
* Lambdas created before an approval and called after it are reported by
  E0403 (fail closed); see `OPEN_QUESTIONS.md` for this and the other
  decisions.
* `-9223372036854775808` cannot be written as a literal.
