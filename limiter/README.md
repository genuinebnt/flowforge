# limiter

Rate limiting for Flowforge (spec section 14): one contract, several
algorithms, the key hierarchy and the distributed strategies. Pure Go,
standard library plus `core`; the Redis adapter is in
[`redislimiter`](redislimiter/).

| Spec | What | Where |
|------|------|-------|
| 14.1 | `Limiter` (`Acquire`, atomic `AcquireAll`, `Configure`), `Result`, `Rules` (exact / `prefix*` / `*` patterns) | `limiter.go`, `params.go` |
| 14.2 | `GCRA` (default), `AtomicGCRA`, `TokenBucket`, `SlidingWindowCounter`, `SlidingWindowLog`, `LeakyBucket`, `AIMD`, `WFQ` | one file each |
| 14.3 | key builders and `ToolCall.Requests` | `keys.go` |
| 14.4 | `LeasedBudget` over the `Store` port (`MemStore` fake), `GCounter` / `CRDTLimiter`, Redis Lua script | `lease.go`, `crdt.go`, `redislimiter/` |
| 23.2, 23.8 | `Failover`: fail closed for active work, local budgets for passive work | `failover.go` |

Time is always an argument (`now time.Duration`, a monotonic reading); no
algorithm reads a clock. Every limiter is safe for concurrent use.

```go
g, _ := limiter.NewGCRA(
    limiter.Rule{Pattern: "rl:t:*", Params: limiter.Params{Rate: 20, Burst: 40}},
    limiter.Rule{Pattern: "rl:h:*", Params: limiter.Params{Rate: 5, Burst: 5}},
    limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 100, Burst: 100}},
)
reqs, _ := limiter.ToolCall{Class: "active", TenantID: t, ProgramID: p, Target: host, Tool: "httpx"}.Requests(1)
res, err := g.AcquireAll(reqs, clock.Monotonic())
```

## Tests

```sh
go test -race ./limiter/...                         # unit, conformance, property tests
FF_PROPTEST_CASES=100000 go test ./limiter/         # nightly property depth
go test -run '^$' -fuzz FuzzLimGCRA -fuzztime 30m ./limiter/
go test -run '^$' -bench . -cpu 1,2,4,8,16 ./limiter/   # b_lim_01, b_lim_02
FF_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./limiter/redislimiter/  # lim_010, b_lim_03
```

* Required cases `TestLim001`..`TestLim011` (lim_010 is in `redislimiter`;
  lim_009, the spec's loom test, is a `-race` stress test of `AtomicGCRA`).
* Conformance data: `conformance/limiter/gcra/*.json` (run against GCRA,
  AtomicGCRA and the leaky bucket) and `conformance/limiter/models/*.json`
  (token bucket, sliding windows, AIMD rate trace).
* Properties: `TestLimProp_WindowBound` (allowed in any `[t, t+w]` is at
  most `burst + rate*w`), `TestLimProp_MatchesReference` (every variant
  equals a timestamp-list reference model; atomic GCRA equals mutex GCRA
  exactly), plus retry honesty and G-counter merge laws.

## Benchmarks (4 vCPU Xeon, `-benchtime=10000x`; 300ms for b_lim_02)

| Benchmark | ns/op | allocs/op |
|-----------|------:|----------:|
| b_lim_01 gcra / gcra_atomic | ~56 / ~46 | 0 / 0 |
| b_lim_01 token bucket / leaky / sliding counter / sliding log | ~60-73 / ~62 / ~53 / ~54 | 0 |
| b_lim_02 1 key, 4 threads: atomic GCRA vs global-mutex token bucket | ~80 vs ~122 | 0 |
| b_lim_02 1,000 keys, 1 → 4 threads: atomic GCRA | ~73 → ~25 | 0 |
| b_lim_02 1,000 keys, 4 threads: global-mutex token bucket | ~119 | 0 |
| b_lim_03 Redis, 1 connection | p50 ~45 µs | |

## Known limitations

* `SlidingWindowCounter` (and `CRDTLimiter`) are approximations: they can
  admit up to about 2L in an arbitrary sliding window of length W, within
  the spec 25.4 bound `L + (L/W)*w`. Use `SlidingWindowLog` when exactness
  matters.
* `AtomicGCRA.AcquireAll` locks the cells it touches (single-key callers
  of those keys yield until it finishes); only single-key `Acquire` is
  lock-free.
* `LeasedBudget` holds one mutex across store round trips per node.
* Consistent assignment (rendezvous ownership, 14.4) is not implemented
  here; it belongs with `placement`.
* See [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md) for spec ambiguities and the
  choices pinned by tests.
