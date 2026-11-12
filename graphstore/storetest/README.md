# graphstore/storetest

The shared behaviour suite of `graphstore.Store` implementations and the
b_sys_01 workload (`BenchIngest`). Each implementation calls the suite
functions from its own `TestGraph...` tests, optionally with an oracle
factory (Mem) whose results must match byte for byte. The package does not
import `testkit`; callers pass conformance cases and a seeded `core.Rng`.
