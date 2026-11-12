# vm

The Flowforge virtual machine (spec 8.1, 8.2, 8.6, 8.7): runs a
`bytecode.Verified` module as a pure function of its inputs and recorded
effect results. Layer 3; imports `core`, `core/netnorm`, `bytecode` and
`github.com/fxamacker/cbor/v2`.

## API

| Use | Call |
|-----|------|
| run the workflow body | `New(prog, Config)`, `Start(args)`, `Run(fuel)`, `Resume(EffectResult, fuel)` |
| outcomes | `Suspended{Path, Seq, Kind, Args}`, `Completed{Value}`, `Failed{Code, Message, Span}`, `OutOfFuel{}` |
| par_map | `ParMapArgs(s)`, `ParMap.NewTask(prog, s, i, cfg)`, `ParMap.Join(outcomes)` |
| compensation | `VM.Compensations()` (run order), `NewChild(prog, closure, nil, prefix, cfg)` |
| snapshots | `VM.Snapshot()`, `Restore(prog, bytes, cfg)` |
| effect args | `EncodeArgs`, `ArgsHash`, `DecodeArgs`, `EncodeValue`, `DecodeValue` (deterministic CBOR) |
| JSON | `ParseType`, `ToJSON`, `FromJSON`, `DecodeInputs`, `EncodeOutput` |
| drivers | `RunWithEffects`, `Drive`, `ScriptedEffects`, `Replayer` |
| reference evaluators | `CallBuiltin` (native builtins outside a run), `NewClosure` |
| per-site policies | `Suspended.Site` (function, code offset, source span of the EFFECT) with `bytecode.Annotations.Site` |

## Tests and benchmarks

    go test ./vm/...
    go test ./vm/ -run XXX -fuzz FuzzVerifier -fuzztime 20s
    go test ./vm/ -run XXX -fuzz FuzzSnapshotDecode -fuzztime 20s
    go test ./vm/ -run XXX -bench . -benchtime 3x

vm_001..vm_007 are `TestVM001_Arithmetic` .. `TestVM007_CompensationOrder`
(vm_008 and the `conformance/vm` suites run in package `compiler`);
properties `TestVMProp_VerifiedNeverPanics`, `TestVMProp_FuelMonotonic`;
benchmarks `BenchmarkVM01_*` (fib(30), arithmetic loop with a zero
per-instruction allocation check, map/filter over 1M elements, string
building), `BenchmarkVM02_Snapshot` (10, 1k, 100k live values) and
`BenchmarkVM03_Replay` (10k, 100k effects).

## Known limitations

See `OPEN_QUESTIONS.md`.
