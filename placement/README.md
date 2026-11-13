# placement

Work distribution for Flowforge workers: which live worker owns a key (run
affinity, target affinity) and in which order workers try to claim it.

Spec: section 23.3 (work distribution), the `Placement` port of 24.4, tests
plc_001..003 and the ff-placement properties of 25.3/25.4, benchmark
b_plc_01 (25.8).

## API

| Identifier | What it is |
|---|---|
| `Placement` | Port: `Owner(key, live) (WorkerID, bool)`, `Ranking(key, live) []WorkerID` |
| `Rendezvous` | Highest-random-weight hashing, O(workers) per lookup, no state |
| `ConsistentHash`, `Ring`, `ConsistentConfig` | Virtual-node ring (default 256 points per unit of weight), O(log points) lookup, optional per-worker weights |
| `Hash64` | The fixed 64-bit score: `fmix64(FNV-1a-64(uint64_le(len(key)) ‖ key ‖ worker))` |
| `Rank`, `ClaimDelay` | Position in the claim order and the derived back-off (rank × step) |
| `Membership`, `StaticMembership`, `HeartbeatMembership` | Live sets; heartbeats every 5 s, live for 15 s (monotonic clock) |

Every result is a pure function of the key and the membership *set*: the
order of `live` and duplicate ids do not matter, so every node computes the
same answer. Ranks only change who tries first; the lease and fence decide
who may write.

## Tests and benchmarks

```
go test ./placement/...
go test -run '^$' -bench Plc01 ./placement/
```

Golden data in `conformance/placement/` pins `Hash64` vectors, rendezvous
and consistent-hash owners and rankings for other implementations;
`TestPlcConformance` only compares, it never rewrites the files.

## Limitations

* `ConsistentHash` caches one ring (the last membership seen). Callers that
  alternate between memberships rebuild on every call; use `NewRing`
  directly in that case. Its port lookups also compare the membership slice
  (O(workers)); the bare `Ring.Owner` is the logarithmic path.
* No gossip/SWIM membership (spec stretch goal); `HeartbeatMembership` is
  in-process. A Postgres or Redis heartbeat table is an adapter concern.

## Choosing an algorithm

Decision point: run ownership (spec 23.3). `placement.New(scheme, cfg)`
selects it; `Schemes()` lists `rendezvous` (default) and `consistent`,
and the golden conformance data runs through `New` for each.

| Scheme | Owner, 10 workers | Owner, 100 | Owner, 1,000 | Ranking, 1,000 |
|---|---|---|---|---|
| `rendezvous` (default) | 145 ns | 758 ns | 7.6 µs | 195 µs |
| `consistent` | 104 ns | 381 ns | 2.7 µs | 175 µs |

`go test -run '^$' -bench PlcAlgo ./placement` (200 iterations). Measured 2026-11-13 on a 4-vCPU Linux container (Go 1.26, `GOMAXPROCS=4`); see `docs/ALGORITHMS.md` for the consolidated entry. The
ring is faster per lookup at large memberships but holds a ring that is
rebuilt on every membership change; rendezvous needs no state, and engine
memberships are small (tens of workers), where the two are within 2x, so it
stays the default.
