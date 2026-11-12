// Package compiler lowers checked Flowforge programs (package dsl) to
// verified bytecode modules (package bytecode), following the lowering
// rules of spec 8.5 and the compiler obligations of bytecode/doc.go. It
// fills the module sections of spec 8.3 (CONSTS, TYPES with the entry's
// signature, FUNCS with span tables, EFFECTS, TOOLS, META) and the
// optional ANNOTS section that carries the annotations of spec 7.5 to the
// engine, and runs the verifier of spec 8.8 on its own output.
//
// # Entry points
//
//	res, diags := compiler.Compile(src, compiler.Options{Workflow: "recon", Tools: registry, Policy: policy})
//	res.Encoded         // the module bytes (spec 8.3); res.ModuleSHA256 its hash
//	res.Verified        // the verifier's result, ready for vm.New
//	res.Annotations()   // workflow and per-effect annotations (bytecode.Annotations)
//	res.SourceSpan(...) // map a vm.Failed span back to line and column
//
// EngineAdapter wraps Compile in the shape of the engine's Compiler port.
// Package compiler/treewalk is the tree-walking reference evaluator used
// by the differential tests (vm_008).
//
// # Lowering
//
// One bytecode function is emitted for the entry workflow (parameters are
// locals 0..n-1, the body ends in HALT), one per reachable fn, one per
// lambda that becomes a value (captures by value, in Lambda.Captures
// order, read with LOAD_CAP), one per compensation (zero arguments, `it`
// as capture 0) and one wrapper per tool, library function or variant
// constructor used as a value. Higher-order library functions are loops
// whose state travels on the stack; lambda literals passed to them are
// inlined. `?`, match, approvals, par_map, compensation, string
// interpolation, casts and short-circuit operators follow spec 8.5; the
// details are in README.md.
package compiler
