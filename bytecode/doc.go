// Package bytecode is the Flowforge module format, instruction set,
// assembler and verifier (spec 8.3, 8.4, 8.5 and 8.8).
//
// A Module mirrors the binary layout of spec 8.3 (magic "FFBC", format
// version 1, CONSTS, TYPES, FUNCS, EFFECTS, TOOLS, META, entry). Encode and
// Decode are exact inverses on every module Decode accepts; Module.Hash is
// the SHA-256 of the encoding and is stored with every run. Builder is the
// assembler used by the DSL compiler and by tests. Verify checks every
// rule of spec 8.8 and returns a *Verified, the only form the VM loads.
//
// # Compiler obligations beyond spec 8.5
//
// The verifier enforces these, so a compiler must follow them:
//
//   - Locals are single-assignment on every path. A loop back edge (LOOP)
//     does not carry local definitions: locals first written inside a loop
//     are fresh in every iteration, and a local written before a loop must
//     not be written inside it. Loop-carried state (index, accumulator,
//     the iterated list) therefore travels on the operand stack: the loop
//     header starts with STORE_LOCAL into per-iteration locals, the body
//     reloads them, and the iteration ends by pushing the next state and
//     LOOP. Locals written in the loop body are not readable after the loop
//     unless written before the exit test.
//
//     NEW_LIST; <xs>; CONST 0                    ; [acc, xs, i]
//     head: STORE i; STORE xs; STORE acc
//     LOAD i; LOAD xs; LIST_LEN; LT; JUMP_IF_FALSE exit
//     LOAD xs; LOAD i; LIST_GET; STORE x; ...body pushing onto acc...
//     LOAD acc'; LOAD xs; LOAD i; CONST 1; ADD_I; LOOP head
//     exit: LOAD acc
//
//   - Stack depth must agree at joins. In a match, TEST_VARIANT leaves the
//     enum under the Bool, so the "no match" target of an arm starts with
//     POP and a failed guard jumps past that POP.
//
//   - LIST_LEN pops the list (it does not peek).
//
//   - tool_call: push the inputs, then CONST of the tool name (a String
//     constant naming a TOOLS entry), then EFFECT 1 with argc = inputs + 1.
//     The EFFECT must not be a jump target, so the CONST always precedes it.
//     Use FuncBuilder.ToolCall.
//
//   - The fixed argument counts of the other effect kinds are listed in
//     EffectKind.Argc; par_map takes (list, closure, limit, on_error) with
//     on_error ParMapFail or ParMapCollect.
//
//   - CALL targets functions without captures; closures go through CLOSURE
//     and CALL_CLOSURE. PUSH_COMP names a zero-argument function whose
//     captures include `it`.
//
//   - Higher-order library functions are loops; sort_by and group_by
//     compute the keys in a loop and call sort_by_keys / group_by_keys.
//     `a..b` is the range builtin.
//
//   - `e?` is TRY. In the workflow body a failing TRY fails the run with
//     approval_denied when the Err payload is the built-in Denied record
//     (so `require approval(...)` is EFFECT approval; TRY), and with
//     unwrap_failed otherwise.
//
//   - The entry function should carry a KindSignature layout (Builder.Signature)
//     so the engine can decode inputs and encode outputs.
//
// Effect classes: EffectMask bits 0-7 are the DSL effects read, passive,
// active, intrusive, write, financial_write, llm, time (spec 7.5); bits
// 8-11 are scope, approval, alert and plan. A tool_call contributes the
// effects the ToolResolver reports for the tool; other kinds contribute
// EffectKind.Mask. The declared mask must cover every EFFECT reachable
// from the entry through CALL, CLOSURE and PUSH_COMP.
package bytecode
