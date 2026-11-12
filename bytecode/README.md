# bytecode

Module format, instruction set, assembler and verifier of the Flowforge VM
(spec 8.3, 8.4, 8.5, 8.8). Layer 2; imports only the standard library.

## Contents

| File | Purpose |
|------|---------|
| `module.go` | `Module`, constants, layouts, `Hash` |
| `encode.go`, `decode.go` | exact little-endian format with strict bounds checks (module <= 4 MiB) |
| `opcodes.go` | opcodes with the spec 8.4 values and operand widths |
| `effects.go` | effect kinds (EFFECT operand), effect mask bits, per-kind argument counts |
| `failcodes.go` | failure codes (FAIL operand), numbered in spec order |
| `types.go` | reserved type ids: Option 0, Result 1, Host 2, Severity 3, Tuple 4, ScopeDenied 5, Approval 6, Denied 7, PlanError 8, TaskError 9, LogLevel 10, Budget 11, Plan 12; user types start at 32 |
| `builtins.go` | stable builtin ids and arities (pure natives for BUILTIN) |
| `builder.go` | assembler: deduplicated constants, labels, jump patching, spans |
| `verify.go` | all spec 8.8 rules, one exported sentinel per rule (`RuleID` gives stable ids) |
| `disasm.go` | `Disassemble` for debugging and goldens |

The compiler obligations that the verifier enforces (loop-carried values on
the stack, tool_call shape, match lowering, TRY failure codes) are
documented in `doc.go`.

## Tests

    go test ./bytecode/...
    go test ./bytecode/ -run XXX -fuzz FuzzBytecodeDecode -fuzztime 20s
    FF_UPDATE_CONFORMANCE=1 go test ./bytecode/ -run Conformance   # regenerate conformance/bytecode/verify

`TestBC001_RoundTrip` (bc_001), `TestBC002_VerifierRejects` (bc_002, one
subtest per rule), `TestBCProp_EncodeDecodeIdentity`, and the
`conformance/bytecode/verify` runner. `FuzzVerifier` lives in `vm` because
it runs accepted modules.

## Known limitations

See `OPEN_QUESTIONS.md`.
