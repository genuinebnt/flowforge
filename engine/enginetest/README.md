# enginetest

Test support for the engine (imported only from tests): in-memory fakes of
every engine port, `CrashStore` (a worker that dies after N appends), the
hand-assembled workflows used by eng_001..eng_008 and `Harness`, which wires
an engine with a fake clock starting at 2026-11-12T14:02:11Z and seeded
randomness. `Normalize` and `NormalJSON` render logs with timestamps,
durations, module hashes and `now()` results replaced, for golden files and
crash-equivalence checks.
