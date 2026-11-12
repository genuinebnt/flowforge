# eventstore/storetest

Conformance suite for `eventstore.Store` implementations (spec 23.4
properties (1)-(4), 23.8, es_001-es_007, the random-interleavings property
of 25.4) plus the b_es_01/b_es_02 benchmark bodies.

```go
func newHarness(t *testing.T) *storetest.Harness {
    return &storetest.Harness{
        Store:  store,                 // empty store
        Now:    func(ctx) (time.Time, error) { ... },  // the store's clock
        Elapse: func(ctx, d) error { ... },            // make d pass
        // optional: WorkflowVersion, Restart, PropCases
    }
}
func TestES007_AllImplementations(t *testing.T) { storetest.Run(t, newHarness) }
```

| Function | Case |
|----------|------|
| `ES001` | append with correct expected_seq and fence; read in order |
| `ES002` | stale expected_seq: SeqConflict, nothing written |
| `ES003` | older fence, correct expected_seq: Fenced, nothing written |
| `ES004` | acquire, expire, acquire by another worker: fence increases, old holder fenced |
| `ES005` | two goroutines race for one expected_seq, 100 iterations: one winner, no gaps |
| `ES006` | read-your-writes; Subscribe once and in order, resume from a seq |
| `CreateRun`, `Snapshots`, `ReadyAndClaim`, `RestartFences` | the rest of the contract |
| `Scripts` | every `conformance/eventstore/*.json` scenario |
| `PropRandomInterleavings` | model-checked random interleavings + a concurrent phase |
| `BenchAppend`, `BenchLeaseClaim` | b_es_01, b_es_02 |

Time never passes on the wall clock: lease expiry and wake times are driven
by `Harness.Elapse` (MemStore: advance the FakeClock; Postgres: shift the
stored timestamps back). The suite is deterministic for a given `FF_SEED`
except for the goroutine races in ES005/ES006 and the concurrent property
phase, which check invariants only.

Like `testkit`, import this package only from `_test.go` files.
