// Package dsl is the front end of the Flowforge language (spec section 7):
// lexer (7.2), parser with error recovery (7.3), syntax tree, name
// resolution, bidirectional type checking with rank-1 polymorphism (7.4,
// 7.5), effect inference and checking, scope typing, the compile-time
// policy check, annotation validation, and diagnostics in the JSON shape
// of 7.7. Agent plans (16.1) are checked with the same entry points, and
// the similarity builtins of 22.9 are part of the prelude.
//
// # Entry points
//
//	f, diags := dsl.Parse("recon.ff", src)        // syntax tree + syntax diagnostics
//	prog, more := dsl.Check(f, dsl.Options{...})   // annotates f in place
//	res := dsl.Analyze(source, opts)               // CheckWorkflowResponse shape (spec 11.4, 12.6)
//	text := dsl.Format(f)                          // canonical pretty-printer
//	js, _ := dsl.ASTJSON(f)                        // canonical AST JSON (conformance goldens)
//
// # The typed tree
//
// Check fills the "set by Check" fields of the tree so that a compiler can
// lower it (spec 8.5) without repeating any analysis:
//
//   - Every Expr has Type() (zonked: no inference variables remain in a
//     program without errors) and Effects().
//   - Every *Ident and *PathExpr has a Ref: a local or parameter
//     (RefLocal, with a *Binding whose Slot indexes its Frame), a captured
//     variable (RefCapture, with Capture indexing the enclosing
//     Lambda.Captures), a fn, tool, builtin, enum variant, or the scope
//     namespace.
//   - Every *Lambda (including the synthetic compensation lambda of a
//     let statement, whose only parameter is `it`) has a Frame and a
//     Captures list in first-use order; Capture.Outer tells the creating
//     frame where to find each captured value. Captures are by value.
//   - Every *CallExpr has a Kind (builtin, tool, fn, variant constructor
//     or closure) with the resolved target, each Arg has the parameter
//     index it binds to (named arguments may be reordered; evaluate in
//     source order), Own holds the callee's own effects, and TypeArgs the
//     instantiation of a generic callee. Pipes are desugared at parse time
//     (CallExpr.Pipe records the written form for Format).
//   - *TryExpr.Mode says whether a failing `?` returns Err, returns None
//     or fails the run (unwrap_failed); *CastExpr.Kind names the
//     conversion; *ForStmt has ElemType and IterKind; patterns carry their
//     resolved variants, record definitions and field indices; record
//     literals carry field indices.
//   - Items carry validated annotations in Attrs; fns and workflows carry a
//     FuncInfo (signature, declared and inferred effects, runtime
//     operations, tools used, frame).
//
// Program lists the type definitions in ID order (builtins first), the
// tools, fns and workflows with their inferred effects and activity
// class. Builtins() lists the standard library with stable IDs and, for
// effectful builtins, the EFFECT kind of spec 8.4 (OpKind values equal
// the instruction operand).
//
// Nothing in the package panics on any input; problems are reported as
// diagnostics in source order.
package dsl
