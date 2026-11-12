// Package treewalk is the tree-walking reference evaluator of the
// Flowforge language: it evaluates the checked syntax tree of package dsl
// directly, producing the same vm.Value results and the same effect
// requests (kind, arguments, hierarchical paths, order) as the bytecode
// VM running the compiled module. It is the oracle of the differential
// test vm_008 (spec 25.3, 25.6) and of the property "compiled program
// evaluated by the tree-walker equals the VM result" (spec 25.4).
//
// The evaluator shares only the value representation with the compiler
// (compiler/internal/abi: type ids, variant indices, which native builtin
// implements a pure library function) and calls the VM's native builtins
// for leaf library functions (vm.CallBuiltin). Control flow, pattern
// matching, closures, higher-order functions, arithmetic, `?`,
// compensation and par_map are evaluated independently.
//
// Effects are driven by the vm.Handler signature of vm.RunWithEffects;
// par_map tasks are evaluated here, one child evaluation per element with
// paths parent/task/seq. Fuel and the live-heap limit are not modeled; the
// string, list, effect-count and call-depth limits are.
package treewalk
