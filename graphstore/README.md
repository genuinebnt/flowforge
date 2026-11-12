# graphstore

The Flowforge graph store: GraphService (spec 11.6) over the OAM graph,
in memory (`Mem`, built on `oam.Graph`) and in Postgres (`graphstore/pg`),
with identical observable behaviour.

**Spec:** 5.7 ingest/merge, 5.9 traversal and analysis, 6.1 observations,
10.5 graph tables, 10.7 RLS and trigram search, 11.6 GraphService, 12.7
REST examples, 21.2/21.4/21.6 snapshots, 23.5 convergent merge, 25.8
`b_sys_01`. Layer 4: imports `core`, `oam`, `scope`, `csr`
(`graphstore/pg` adds pgx).

## Layout

| Path | Contents |
|------|----------|
| `store.go`, `types.go` | `Store` (one method per RPC plus `BlastRadius`, `Components`, `Export`, `ReadChanges`, snapshots), `Caller`, `Config`, request/result types |
| `request.go`, `validate.go`, `ids.go`, `page.go`, `errors.go` | Boundary checks shared by both stores: canonical ids, request/envelope cross-checks, page tokens, `*core.Error` mapping |
| `traverse.go` | `Reader` port; BFS neighbours, Dijkstra path, blast radius |
| `search.go` | `SearchText`, pg_trgm-compatible `Trigrams`/`Similarity`, `MatchSearch` |
| `snapshot.go`, `blob.go` | `BuildCSR`, `PublishSnapshot`, `SnapshotInfo`; `BlobStore`, `MemBlobs`, `DirBlobs` |
| `scopeport.go`, `watch.go`, `views.go` | `PolicyScope`, `WatchLoop`/`Notifier`, oam view conversions, `TagProperty` |
| `mem.go`, `mem_read.go` | `Mem` |
| `internal/kernel` | The merge kernel of the Postgres store: oam's rules over loaded rows, plus the storage encoding of oam's bookkeeping; tested against `oam.Graph` |
| `pg/` | Postgres store and outbox `Relay` |
| `storetest/` | Shared behaviour suite and the b_sys_01 workload |

## Usage

```go
mem := graphstore.NewMem(graphstore.Config{Scope: graphstore.PolicyScope(policyFor), Clock: clock})
store, err := pg.Open(ctx, dsn, pg.Config{Config: graphstore.Config{Scope: ..., Clock: clock}})
res, err := store.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tok.Tenant, ProgramID: prg, RunID: run, Observation: o})
sub, err := store.Neighbors(ctx, graphstore.Caller{TenantID: tok.Tenant},
	graphstore.NeighborsRequest{EntityID: id, Direction: graphstore.DirectionOut, Labels: []string{"dns_record", "port"}, Depth: 2})
info, err := store.WriteSnapshot(ctx, caller, prg, graphstore.DirBlobs{Root: dir})
relay := &pg.Relay{DB: ownerPool, Publish: publishToBus} // at least once
```

## Tests and benchmarks

```sh
go test -race ./graphstore/...                       # Mem, kernel vs oam.Graph
scripts/devpg.sh start                               # throwaway PostgreSQL 16
FF_TEST_PG_DSN=postgres://... go test -race ./graphstore/...   # + Postgres, compared with Mem
go test ./graphstore/ -run '^$' -bench Sys01 -benchtime 20000x
FF_TEST_PG_DSN=... go test ./graphstore/pg/ -run '^$' -bench Sys01 -benchtime 20000x
```

The suite (`storetest`) replays `conformance/oam/merge`, checks
idempotency, atomicity, convergence under permutation and duplication (and
Mem against Postgres byte for byte, also with 8 concurrent writers),
neighbours/path/blast/components/diff/search on a fixture graph (including
the spec's `jenkin` fuzzy example), staleness, tenant isolation on every
path, WatchChanges resume and snapshot determinism (same sha256 for Mem
and Postgres). The Postgres tests clone a migrated template database per
store; RLS is checked with a non-owner role.

b_sys_01 on a 4 vCPU container, PostgreSQL 16 on the same machine, 20,000
simulated tool observations (FQDN, IP, DNS edge; every third adds a
service and a port edge): Mem 15,000-17,000 obs/s (p99 0.22 ms, one
writer); Postgres about 470 obs/s with one writer (p50 2.0 ms, p99 5 ms)
and 950-1,270 obs/s with 8 writers (p50 6-8 ms, p99 12-20 ms). The Postgres
cost is round trips (five per observation) and server CPU shared with the
benchmark.

## Known limitations

* `Mem` keeps every observation id and change event forever, and scans
  per query; it is a dev and test store.
* WatchChanges in Postgres wakes on commits of the same `pg.Store` and
  otherwise polls (`PollInterval`); there is no LISTEN/NOTIFY.
* Merge bookkeeping that the 10.5 columns cannot hold (per-field stamps,
  per-tool covering-run counters) lives in hidden `ff:` tag rows.
* Search similarity is whole-text trigram similarity, so typo tolerance on
  long banners is weak; substring matches always hit.
* GCS blob storage is not implemented (port only).

Ambiguities and the choices made are in `OPEN_QUESTIONS.md`.
